package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/net/proxy"

	"github.com/nao1215/tornago"
)

// ProxyInfo describes a single proxy server in the pool.
type ProxyInfo struct {
	URL         string // full proxy URL, e.g. "http://ip:port" or "socks5://ip:port"
	Protocol    string // "https" or "socks5"
	IP          string
	Port        string
	Country     string // ISO country code (e.g. "US", "DE")
	Continent   string // continent code (e.g. "NA", "EU")
	SourceURL   string // source URL this proxy was fetched from
	SourceIndex int    // index in proxy_sources.txt (lower = higher priority)
}

// isPreferredContinent reports whether the proxy is in Europe or North
// America, which are the preferred regions.
func isPreferredContinent(continent string) bool {
	switch strings.ToUpper(continent) {
	case "EU", "NA":
		return true
	}
	return false
}

// proxyPool is the current set of usable proxies, sorted by priority:
// preferred-continent proxies first (EU then NA), then all others. Within
// each group, proxies from higher-priority sources (lower SourceIndex) come
// first, and ties are broken by source order then by IP.
//
// It is deliberately a plain snapshot: selection is ProxyService.pick's job,
// because only that knows about cooldowns, readiness and measured latency. A
// round-robin cursor here would pick proxies the request path must skip.
type proxyPool struct {
	mu   sync.RWMutex
	list []*ProxyInfo
}

func (pp *proxyPool) get() []*ProxyInfo {
	pp.mu.RLock()
	defer pp.mu.RUnlock()
	out := make([]*ProxyInfo, len(pp.list))
	copy(out, pp.list)
	return out
}

func (pp *proxyPool) set(list []*ProxyInfo) {
	pp.mu.Lock()
	pp.list = list
	pp.mu.Unlock()
}

// len is the current pool size, for observability only.
func (pp *proxyPool) len() int {
	pp.mu.RLock()
	defer pp.mu.RUnlock()
	return len(pp.list)
}

// ProxyService fetches proxy lists from the configured sources, geolocates
// each proxy, and maintains a priority-sorted pool. The pool is refreshed
// every refreshInterval.
// Timings for the readiness rule.
//
// A proxy is "ready" only while it has been PROVEN reachable recently. The TTL
// is short on purpose: free proxies die constantly, so a pool that was good a
// few minutes ago is not evidence that any of it still works. A stale ready
// mark is worse than no mark, because it makes the router commit to a proxy it
// will then have to abandon mid-request.
const (
	// proxyReadyTTL is how long a proven proxy stays counted as ready.
	proxyReadyTTL = 1 * time.Minute
	// proxyVerifyInterval is how often the top-up loop re-checks whether the
	// ready pool still meets the requirement.
	proxyVerifyInterval = 15 * time.Second
	// proxyVerifyTimeout bounds one readiness probe.
	proxyVerifyTimeout = 500 * time.Millisecond
	// proxyProbeHTTPTimeout bounds the HTTPS request issued through the proxy
	// during a readiness probe (CONNECT + TLS + one request). Generous enough
	// for a slow but working proxy, short enough that a dead one does not
	// stall the top-up loop.
	proxyProbeHTTPTimeout = 2 * time.Second
	// maxProbesPerCycle bounds how much of the pool one top-up pass will walk,
	// so a pool where nothing answers cannot stall the loop for minutes.
	maxProbesPerCycle = 40
	// proxyProbeConcurrency is how many proxies a top-up pass probes at once.
	proxyProbeConcurrency = 8
	// maxAcceptableLatency is the low-latency threshold (<500ms) for active proxies.
	maxAcceptableLatency = 500 * time.Millisecond
)

type ProxyService struct {
	// sourcesFile is an optional extra-sources file. The built-in list in
	// proxysources.go is always used; this file can only ADD sources, so a
	// stale or missing file cannot silently shrink the pool.
	sourcesFile     string
	apiKey          string
	client          *http.Client
	refreshInterval time.Duration
	pool            proxyPool
	stopCh          chan struct{}
	running         atomic.Bool

	downMu sync.Mutex
	down   map[string]time.Time // proxy URL -> cooldown expiry

	// blacklisted is the set of proxies that have failed at least once and are
	// therefore never used again for the rest of the process's life. Terminal:
	// it survives pool refreshes and is not subject to expiry.
	blacklistedMu sync.Mutex
	blacklisted   map[string]struct{}

	// source bookkeeping: per-source failure rates for the dashboard.
	sourceMu     sync.Mutex
	sourceOf     map[string]string // proxy URL -> source URL
	sourceIndex  map[string]int    // source URL -> source index
	seenProxy    map[string]bool   // proxy URL ever seen
	failedProxy  map[string]bool   // proxy URL ever blacklisted
	sourceTotal  map[string]int    // proxies seen per source
	sourceFailed map[string]int    // proxies failed per source

	// ready records when each proxy was last PROVEN usable. An entry older
	// than proxyReadyTTL no longer counts; readiness is continuously re-earned
	// rather than granted once.
	readyMu sync.Mutex
	ready   map[string]time.Time

	// readyLatency records the measured dial latency for each ready proxy,
	// so pick() can prefer the fastest. Entries are evicted together with
	// the readiness mark, and when the proxy leaves the pool: a map keyed by
	// proxy URL is otherwise unbounded, because a free-proxy pool is
	// replaced wholesale on every refresh.
	readyLatencyMu sync.Mutex
	readyLatency   map[string]time.Duration

	// required is the minimum number of simultaneously-ready proxies: the sum
	// of the proxy budgets of every enabled provider, so that a request which
	// needs N proxies has N ready to draw on. Pushed in from the config by
	// Proxy.UpdateRequiredReady on startup and on every hot reload.
	required atomic.Int32

	// pickCounter is a monotonic counter used to round-robin among proxies
	// that tie on readiness/latency, so a pool of equal-cost proxies (notably
	// Tor instances, which all report latency 0) is actually distributed
	// across instead of always landing on the first entry.
	pickCounter atomic.Uint64

	stopOnce sync.Once

	// Tor mode: when torInstances > 0 the pool is backed by that many Tor
	// daemons instead of fetched HTTP proxy lists.
	torInstances int
	torMode      bool
	torMu        sync.Mutex
	torProcesses []*tornago.TorProcess
	// torState tracks the observed bootstrap state of every spawned Tor
	// daemon: the control port and cookie used to query it, the last
	// bootstrap percentage reported by Tor, and the last error seen while
	// querying. Surfaced on the dashboard so an operator can tell whether a
	// proxy routing failure is because a Tor instance is still building its
	// first circuit or because of something else.
	torStateMu sync.Mutex
	torState   map[string]*torInstanceState

	// probeTargets are the real provider base URLs the readiness probe tunnels
	// through the proxy and performs a TLS handshake against, so an MITM or
	// TLS-breaking proxy is retired at probe time instead of on a live request.
	// Guarded by probeMu. Empty means "TCP-only probe" (backward compatible).
	probeMu      sync.RWMutex
	probeTargets []string

	// geoCache memoizes IP -> country/continent. Geolocation is a remote API
	// call per proxy, so without this every refresh re-resolves the whole pool
	// (hundreds of calls) even though a given IP's country never changes.
	geoMu    sync.Mutex
	geoCache map[string]geoEntry
}

// geoEntry is one memoized geolocation result.
type geoEntry struct {
	country   string
	continent string
	at        time.Time
}

// geoCacheTTL is how long a geolocation result is reused. IP-to-country is
// effectively static for the life of a proxy, so a long TTL is safe; the bound
// exists only so a bad answer is not permanent.
const geoCacheTTL = 24 * time.Hour

// geoCacheMaxEntries bounds the geolocation cache. A free-proxy pool is
// replaced wholesale on every refresh, so without a bound the cache would
// keep every address ever seen for the life of the process. The limit is far
// above any real pool, so it only ever fires once the process has resolved
// thousands of distinct addresses.
const geoCacheMaxEntries = 8192

// geoWorkers bounds how many geolocation lookups run concurrently. Geolocation
// is one remote API call per address, so a cold refresh of a few hundred
// proxies used to be a few hundred sequential round-trips; a small fan-out
// collapses that while keeping the request burst modest.
const geoWorkers = 4

// torStatusInterval is how often each Tor daemon's bootstrap state is
// refreshed from its control port for the dashboard. It is intentionally
// short: an operator watching proxy failures needs to see within a few
// seconds whether Tor has finished bootstrapping.
const torStatusInterval = 5 * time.Second

// torControlTimeout bounds each control-port bootstrap query so a hung Tor
// control port cannot block the dashboard poller or a request path.
const torControlTimeout = 5 * time.Second

// NewProxyService builds a proxy service from the sources file. If no sources
// are configured or the API key is missing, the service is still created but
// geolocation will treat every proxy as non-preferred (backup).
func NewProxyService(sourcesFile string, refreshInterval time.Duration) *ProxyService {
	apiKey := os.Getenv("IPGEOLOCATION_API_KEY")

	return &ProxyService{
		sourcesFile: sourcesFile,
		apiKey:      apiKey,
		client: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 10 * time.Second,
			},
		},
		down:            make(map[string]time.Time),
		blacklisted:     make(map[string]struct{}),
		sourceOf:        make(map[string]string),
		sourceIndex:     make(map[string]int),
		seenProxy:       make(map[string]bool),
		failedProxy:     make(map[string]bool),
		sourceTotal:     make(map[string]int),
		sourceFailed:    make(map[string]int),
		ready:           make(map[string]time.Time),
		readyLatency:    make(map[string]time.Duration),
		geoCache:        make(map[string]geoEntry),
		torState:        make(map[string]*torInstanceState),
		refreshInterval: refreshInterval,
		stopCh:          make(chan struct{}),
	}
}

// loadSources returns the sources to fetch on this pass: the built-in list,
// plus any extra URLs named in the optional sources file.
//
// The file is re-read on every refresh rather than once at construction, so
// adding a source takes effect without a restart. It is additive by design: the
// built-in sources are always fetched, so a missing, empty or truncated file
// cannot quietly reduce the pool to nothing.
func (s *ProxyService) loadSources() []proxySource {
	sources := make([]proxySource, 0, len(defaultProxySources)+4)
	sources = append(sources, defaultProxySources...)

	seen := make(map[string]bool, len(sources))
	for _, src := range sources {
		seen[src.URL] = true
	}

	for _, raw := range s.readSourcesFile() {
		if seen[raw] {
			continue
		}
		seen[raw] = true
		src := proxySource{
			Name:     raw,
			URL:      raw,
			Protocol: "https",
		}
		if extract := extractorFor(raw); extract != nil {
			src.Extract = extract
		} else {
			// An unrecognised URL still gets parsed; it just falls back to
			// sniffing the body instead of a source-specific extractor, which
			// is strictly better than ignoring a source an operator added.
			src.Extract = func(body []byte) sourceExtract {
				return sourceExtract{Proxies: parseProxies(body, "", 0)}
			}
		}
		sources = append(sources, src)
	}
	return sources
}

// readSourcesFile reads the optional extra-sources file. A missing file is not
// an error: the built-in list is the normal case.
func (s *ProxyService) readSourcesFile() []string {
	if s.sourcesFile == "" {
		return nil
	}
	data, err := os.ReadFile(s.sourcesFile)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// geolocate returns the country and continent for an IP, memoizing the answer.
//
// The pool is refreshed on a timer and every refresh used to issue one remote
// API call per proxy, so a few hundred proxies meant a few hundred sequential
// HTTP round-trips every 15 minutes for information that does not change. The
// cache makes a refresh cost one call per IP we have not seen before.
func (s *ProxyService) geolocate(ctx context.Context, ip string) (country, continent string) {
	if e, ok := s.cachedGeo(ip); ok {
		return e.country, e.continent
	}

	country, continent = s.geolocateRemote(ctx, ip)
	if country == "" && continent == "" {
		// Do not cache a negative result: a transient API failure would
		// otherwise mark the proxy non-preferred for a whole day.
		return "", ""
	}

	s.geoMu.Lock()
	s.enforceGeoCacheBoundLocked()
	s.geoCache[ip] = geoEntry{country: country, continent: continent, at: time.Now()}
	s.geoMu.Unlock()
	return country, continent
}

// cachedGeo reports the memoized geolocation for an IP without touching the
// network, so a caller can tell "already known" from "needs a lookup".
func (s *ProxyService) cachedGeo(ip string) (geoEntry, bool) {
	s.geoMu.Lock()
	defer s.geoMu.Unlock()
	e, ok := s.geoCache[ip]
	if !ok || time.Since(e.at) >= geoCacheTTL {
		return geoEntry{}, false
	}
	return e, true
}

// enforceGeoCacheBoundLocked keeps the cache within geoCacheMaxEntries. The
// caller must hold geoMu. An arbitrary half is dropped rather than one entry,
// because the addresses in a pool move together: evicting a single entry would
// simply be refilled on the next pass.
func (s *ProxyService) enforceGeoCacheBoundLocked() {
	if len(s.geoCache) < geoCacheMaxEntries {
		return
	}
	excess := len(s.geoCache) - geoCacheMaxEntries/2
	for k := range s.geoCache {
		delete(s.geoCache, k)
		excess--
		if excess <= 0 {
			break
		}
	}
}

// geolocateMissing resolves every IP in the list that is not already cached,
// concurrently, so a cold refresh costs one fan-out instead of one round-trip
// per proxy. Results land in the geoCache that geolocate reads, so the pass
// that follows costs no lookups at all.
//
// ctx is honoured while feeding the workers, so a shutdown cannot leave the
// refresh blocked behind a queue of lookups.
func (s *ProxyService) geolocateMissing(ctx context.Context, ips []string) {
	var pending []string
	seen := make(map[string]bool, len(ips))
	for _, ip := range ips {
		if ip == "" || seen[ip] {
			continue
		}
		seen[ip] = true
		if _, ok := s.cachedGeo(ip); ok {
			continue
		}
		pending = append(pending, ip)
	}
	if len(pending) == 0 {
		return
	}

	work := make(chan string)
	var wg sync.WaitGroup
	workers := geoWorkers
	if workers > len(pending) {
		workers = len(pending)
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ip := range work {
				s.geolocate(ctx, ip)
			}
		}()
	}
feed:
	for _, ip := range pending {
		select {
		case work <- ip:
		case <-ctx.Done():
			break feed
		}
	}
	// Closed by the sending goroutine only: closing early would race a send
	// and panic. Whatever is already queued drains, so no worker is left
	// blocked on a channel that never closes.
	close(work)
	wg.Wait()
}

// geolocateRemote looks up the country and continent for an IP using the
// IPGeolocation.io API (the same API the ipgeolocation CLI calls). On failure
// it returns empty strings, which marks the proxy as non-preferred.
func (s *ProxyService) geolocateRemote(ctx context.Context, ip string) (country, continent string) {
	if s.apiKey == "" {
		return "", ""
	}
	u := "https://api.ipgeolocation.io/v2/ipgeo?apiKey=" + url.QueryEscape(s.apiKey) + "&ip=" + url.QueryEscape(ip)
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return "", ""
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxLargeRequestBytes))
	if err != nil {
		return "", ""
	}
	var res struct {
		Location struct {
			CountryCode2  string `json:"country_code2"`
			ContinentCode string `json:"continent_code"`
		} `json:"location"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return "", ""
	}
	return res.Location.CountryCode2, res.Location.ContinentCode
}

// fetchSource fetches one proxy source URL and returns the raw body.
//
// The request is bound to the caller's context rather than to a background
// one: a refresh bound to context.Background() kept issuing fetches after the
// service had been asked to stop, which on shutdown meant the daemon waited on
// sources it would never use.
func (s *ProxyService) fetchSource(ctx context.Context, sourceURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", sourceURL, nil)
	if err != nil {
		return nil, err
	}
	// Some list hosts 403 Go's default "Go-http-client/1.1"; identify as a browser.
	req.Header.Set("User-Agent", proxyUserAgent)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("source %s returned status %d", sourceURL, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxLargeRequestBytes))
}

// parseProxies parses a raw response body into ProxyInfo entries. It detects
// the format (JSON, plain text, or HTML) and parses accordingly. sourceURL
// and sourceIndex are recorded on each entry for priority ordering.
func parseProxies(body []byte, sourceURL string, sourceIndex int) []*ProxyInfo {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil
	}

	// JSON detection: body starts with '[' or '{'
	if body[0] == '[' || body[0] == '{' {
		return parseJSONProxies(body, sourceURL, sourceIndex)
	}

	// HTML detection: looks like HTML markup
	if isHTML(body) {
		return parseHTMLProxies(body, sourceURL, sourceIndex)
	}

	// Plain text: one proxy per line
	return parseTextProxies(body, sourceURL, sourceIndex)
}

// parseTextProxies parses plain-text proxy lists. Each line may be:
//
//	ip:port
//	ip:port:protocol
//	protocol://ip:port
//	ip:port (whitespace trimmed, comments and blank lines skipped)
func parseTextProxies(body []byte, sourceURL string, sourceIndex int) []*ProxyInfo {
	var out []*ProxyInfo
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		proxy := parseTextLine(line)
		if proxy == nil {
			continue
		}
		proxy.SourceURL = sourceURL
		proxy.SourceIndex = sourceIndex
		out = append(out, proxy)
	}
	return out
}

var (
	textProxyRe = regexp.MustCompile(`^(?:(\w+)://)?([0-9a-fA-F:.]+):(\d+)(?::(\w+))?$`)
	htmlProxyRe = regexp.MustCompile(`(?is)<td[^>]*>\s*([0-9a-fA-F:.]{1,45})\s*</td>\s*<td[^>]*>\s*(\d{1,5})\s*</td>`)
)

func parseTextLine(line string) *ProxyInfo {
	m := textProxyRe.FindStringSubmatch(line)
	if m == nil {
		return nil
	}
	scheme := m[1]
	ip := m[2]
	port := m[3]
	proto := m[4]

	protocol := "https"
	if scheme != "" {
		switch strings.ToLower(scheme) {
		case "socks5", "socks5h":
			protocol = "socks5"
		case "http", "https":
			protocol = "https"
		}
	} else if proto != "" {
		switch strings.ToLower(proto) {
		case "socks5", "socks5h":
			protocol = "socks5"
		case "http", "https":
			protocol = "https"
		}
	}

	var proxyURL string
	if protocol == "socks5" {
		proxyURL = "socks5://" + net.JoinHostPort(ip, port)
	} else {
		proxyURL = "http://" + net.JoinHostPort(ip, port)
	}

	return &ProxyInfo{
		URL:      proxyURL,
		Protocol: protocol,
		IP:       ip,
		Port:     port,
	}
}

// isHTML reports whether the body looks like an HTML document.
func isHTML(body []byte) bool {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return false
	}
	if trimmed[0] == '<' {
		return true
	}
	// Some HTML starts with whitespace or a BOM before <html>
	lower := strings.ToLower(string(trimmed))
	return strings.HasPrefix(lower, "<!doctype") ||
		strings.HasPrefix(lower, "<html") ||
		strings.Contains(lower, "<table")
}

// parseJSONProxies parses a JSON proxy list. Accepted shapes:
//
//	[{"ip":"...","port":"...","protocol":"https"}, ...]
//	{"proxies":[{...}, ...]}
//	{"data":[{"ip":...,"port":...}, ...]}
func parseJSONProxies(body []byte, sourceURL string, sourceIndex int) []*ProxyInfo {
	var raw json.RawMessage = body

	// Try unwrapping common wrapper objects.
	var wrapper struct {
		Proxies []json.RawMessage `json:"proxies"`
		Data    []json.RawMessage `json:"data"`
		Result  []json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &wrapper); err == nil {
		if len(wrapper.Proxies) > 0 {
			raw = json.RawMessage("[" + joinJSON(wrapper.Proxies) + "]")
		} else if len(wrapper.Data) > 0 {
			raw = json.RawMessage("[" + joinJSON(wrapper.Data) + "]")
		} else if len(wrapper.Result) > 0 {
			raw = json.RawMessage("[" + joinJSON(wrapper.Result) + "]")
		}
	}

	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil
	}

	var out []*ProxyInfo
	for _, item := range arr {
		var obj struct {
			IP       string `json:"ip"`
			Port     string `json:"port"`
			Protocol string `json:"protocol"`
			// GeoLite-style field names
			Country string `json:"country"`
			// Some lists use "type" for protocol
			Type string `json:"type"`
		}
		if err := json.Unmarshal(item, &obj); err != nil {
			continue
		}
		if obj.IP == "" || obj.Port == "" {
			continue
		}
		protocol := obj.Protocol
		if protocol == "" {
			protocol = obj.Type
		}
		switch strings.ToLower(protocol) {
		case "socks5", "socks5h":
			protocol = "socks5"
		case "http", "https", "":
			protocol = "https"
		default:
			protocol = "https"
		}

		var proxyURL string
		if protocol == "socks5" {
			proxyURL = "socks5://" + net.JoinHostPort(obj.IP, obj.Port)
		} else {
			proxyURL = "http://" + net.JoinHostPort(obj.IP, obj.Port)
		}

		out = append(out, &ProxyInfo{
			URL:         proxyURL,
			Protocol:    protocol,
			IP:          obj.IP,
			Port:        obj.Port,
			Country:     obj.Country,
			SourceURL:   sourceURL,
			SourceIndex: sourceIndex,
		})
	}
	return out
}

func joinJSON(parts []json.RawMessage) string {
	b := strings.Builder{}
	for i, p := range parts {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(p)
	}
	return b.String()
}

// parseHTMLProxies extracts proxies from HTML table rows. It looks for
// patterns like <td>1.2.3.4</td><td>8080</td> and similar common layouts
// used by free proxy list sites.
func parseHTMLProxies(body []byte, sourceURL string, sourceIndex int) []*ProxyInfo {
	html := string(body)

	// Match <td>ip</td><td>port</td> pairs in either order.
	re := htmlProxyRe
	matches := re.FindAllStringSubmatch(html, -1)

	var out []*ProxyInfo
	seen := make(map[string]bool)
	for _, m := range matches {
		ip, port := m[1], m[2]
		key := ip + ":" + port
		if seen[key] {
			continue
		}
		seen[key] = true

		proxyURL := "http://" + net.JoinHostPort(ip, port)
		out = append(out, &ProxyInfo{
			URL:         proxyURL,
			Protocol:    "https",
			IP:          ip,
			Port:        port,
			SourceURL:   sourceURL,
			SourceIndex: sourceIndex,
		})
	}
	return out
}

// fetchAll fetches every source in priority order, parses proxies from each,
// geolocates them, and returns a priority-sorted pool.
//
// Preferred continents (EU, NA) come first, then all others. Within each
// group, entries are ordered by source priority (lower SourceIndex first),
// then by their order within the source.
// fetched is one source's body after extraction, kept aside so the whole
// pass's addresses are known before any geolocation is performed.
type fetched struct {
	idx    int
	result sourceExtract
}

func (s *ProxyService) fetchAll(ctx context.Context, sources []proxySource) []*ProxyInfo {
	type tagged struct {
		proxy  *ProxyInfo
		prefer bool // true for EU/NA
		source int  // lower = higher priority
		order  int  // sequence within source
	}

	var all []tagged
	order := 0

	// Fetch and extract every source first, so the whole pass's addresses are
	// known before any geolocation happens. Geolocation is a remote call per
	// address, and doing it inline between fetches made a cold refresh a
	// sequential round-trip per proxy: hundreds of proxies meant hundreds of
	// blocking calls, with the refresh loop (and the ready pool behind it)
	// stalled behind them. Resolving them as one bounded fan-out up front
	// collapses that to a few concurrent rounds, and the lookups then hit the
	// cache during the tagging loop below.
	var bodies []fetched
	for idx, source := range sources {
		body, err := s.fetchSource(ctx, source.URL)
		if err != nil {
			log.Printf("[proxy] source %d (%s) fetch failed: %v", idx, source.Name, err)
			continue
		}
		extract := source.Extract
		if extract == nil {
			extract = func(body []byte) sourceExtract {
				return sourceExtract{Proxies: parseProxies(body, "", 0)}
			}
		}
		bodies = append(bodies, fetched{idx: idx, result: extract(body)})
	}
	s.geolocateMissing(ctx, uniqueIPs(bodies))

	for _, f := range bodies {
		idx, source := f.idx, sources[f.idx]
		result := f.result

		// Freshness: a list that says its newest entry was checked more than
		// its MaxAge (or the global proxySourceMaxAge) ago is a list of addresses
		// that have probably gone dead. Discard the source rather than spend the
		// next requests discovering that one attempt at a time.
		//
		// Sources whose format carries no date (the two raw GitHub text lists)
		// report a zero AsOf. That is genuinely unknown, not stale, so they are
		// kept -- and logged as such, so a silently never-discarded source is
		// visible instead of looking like it passed the check.
		maxAge := source.MaxAge
		if maxAge == 0 {
			maxAge = proxySourceMaxAge
		}
		if result.AsOf.IsZero() {
			// Source format carries no date (e.g., plain text lists from GitHub).
			// Fall back to the fetch time. If the fetch time is older than maxAge,
			// discard the source to prevent permanently stale proxies from a source
			// that stopped updating.
			fetchTime := time.Now()
			if age := time.Since(fetchTime); age > maxAge {
				log.Printf("[proxy] source %d (%s): DISCARDED, no body date, fetch time %s (%s old, limit %s)",
					idx, source.Name, fetchTime.UTC().Format(time.RFC3339), age.Round(time.Second), maxAge)
				continue
			}
			log.Printf("[proxy] source %d (%s): no date in body, used fetch time %s (%s old, limit %s)",
				idx, source.Name, fetchTime.UTC().Format(time.RFC3339), time.Since(fetchTime).Round(time.Second), maxAge)
		} else if age := time.Since(result.AsOf); age > maxAge {
			log.Printf("[proxy] source %d (%s): DISCARDED, contents dated %s (%s old, limit %s)",
				idx, source.Name, result.AsOf.UTC().Format(time.RFC3339), age.Round(time.Second), maxAge)
			continue
		}

		if source.Protocol != "" {
			for _, p := range result.Proxies {
				if p.Protocol == "" {
					p.Protocol = source.Protocol
					p.URL = proxyURLFor(p.Protocol, p.IP, p.Port)
				}
			}
		}
		log.Printf("[proxy] source %d (%s): %d proxies parsed", idx, source.Name, len(result.Proxies))
		for _, p := range result.Proxies {
			p.SourceURL = source.URL
			p.SourceIndex = idx
			if p.Country == "" || p.Continent == "" {
				c, cont := s.geolocate(ctx, p.IP)
				p.Country = c
				p.Continent = cont
			}
			all = append(all, tagged{
				proxy:  p,
				prefer: isPreferredContinent(p.Continent),
				source: idx,
				order:  order,
			})
			order++
		}
	}

	// Sort: preferred first, then by source priority, then by discovery order.
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].prefer != all[j].prefer {
			return all[i].prefer // preferred (true) sorts before non-preferred (false)
		}
		if all[i].source != all[j].source {
			return all[i].source < all[j].source
		}
		return all[i].order < all[j].order
	})

	result := make([]*ProxyInfo, 0, len(all))
	for _, t := range all {
		result = append(result, t.proxy)
	}
	return result
}

// uniqueIPs returns the addresses one pass still has to resolve, in first-seen
// order and without duplicates, so the geolocation fan-out does not repeat work
// a source that lists the same proxy twice would otherwise cause.
func uniqueIPs(bodies []fetched) []string {
	seen := map[string]bool{}
	var out []string
	for _, b := range bodies {
		for _, p := range b.result.Proxies {
			if p.IP == "" || seen[p.IP] {
				continue
			}
			seen[p.IP] = true
			out = append(out, p.IP)
		}
	}
	return out
}

// refresh fetches every source and replaces the pool. Called every
// refreshInterval and once at startup. The source list is re-resolved each time
// so an edit to the sources file is picked up without a restart.
func (s *ProxyService) refresh(ctx context.Context) {
	list := s.fetchAll(ctx, s.loadSources())
	// Record per-source bookkeeping for the dashboard. Only the first sighting
	// of a proxy URL counts toward a source's total, so a proxy that survives
	// refreshes does not inflate the denominator.
	s.sourceMu.Lock()
	if s.sourceOf == nil {
		s.sourceOf = make(map[string]string)
	}
	if s.sourceIndex == nil {
		s.sourceIndex = make(map[string]int)
	}
	if s.seenProxy == nil {
		s.seenProxy = make(map[string]bool)
	}
	if s.sourceTotal == nil {
		s.sourceTotal = make(map[string]int)
	}
	if s.sourceFailed == nil {
		s.sourceFailed = make(map[string]int)
	}
	for _, p := range list {
		if p == nil {
			continue
		}
		s.sourceOf[p.URL] = p.SourceURL
		s.sourceIndex[p.SourceURL] = p.SourceIndex
		if !s.seenProxy[p.URL] {
			s.seenProxy[p.URL] = true
			s.sourceTotal[p.SourceURL]++
		}
	}
	s.sourceMu.Unlock()
	s.pool.set(list)
	// The pool was just replaced, so bookkeeping for proxies that are no
	// longer in it is dead weight. Every one of these maps is keyed by proxy
	// URL and a free-proxy pool churns completely, so without this the
	// cooldown and latency maps would grow for the life of the process.
	s.pruneDroppedProxies(list)
	log.Printf("[proxy] pool refreshed: %d proxies (%d preferred, %d backup, %d ready/%d required)",
		len(list), s.countPreferred(list), len(list)-s.countPreferred(list),
		s.readyCount(), s.RequiredReady())
}

// pruneDroppedProxies forgets cooldowns, readiness marks and measured latencies
// for proxies that are no longer in the pool. Expired cooldowns are dropped
// regardless: pick() already treats them as not down, so keeping them bought
// nothing.
func (s *ProxyService) pruneDroppedProxies(list []*ProxyInfo) {
	live := make(map[string]bool, len(list))
	for _, p := range list {
		live[p.URL] = true
	}
	now := time.Now()

	s.downMu.Lock()
	for u, expiry := range s.down {
		if !live[u] || !now.Before(expiry) {
			delete(s.down, u)
		}
	}
	s.downMu.Unlock()

	s.readyMu.Lock()
	for u := range s.ready {
		if !live[u] {
			delete(s.ready, u)
		}
	}
	s.readyMu.Unlock()

	s.readyLatencyMu.Lock()
	for u := range s.readyLatency {
		if !live[u] {
			delete(s.readyLatency, u)
		}
	}
	s.readyLatencyMu.Unlock()
}

func (s *ProxyService) countPreferred(list []*ProxyInfo) int {
	n := 0
	for _, p := range list {
		if isPreferredContinent(p.Continent) {
			n++
		}
	}
	return n
}

// pick returns the next proxy to attempt, skipping proxies in cooldown.
//
// Among ready (proven-reachable within proxyReadyTTL) proxies, the one with
// the lowest measured dial latency is returned first, so low-latency proxies
// (<5s) are preferred as required. If nothing is marked ready (a cold pool, or
// the TTLs all lapsed) it falls back to any proxy that is not in cooldown,
// because an unproven proxy is still better than no proxy. Returns nil only
// when the whole pool is down, so the caller can go direct.
func (s *ProxyService) pick() *ProxyInfo {
	list := s.pool.get()
	if len(list) == 0 {
		return nil
	}

	now := time.Now()

	// Snapshot readiness, cooldown and latency for every pool member without
	// holding the pool lock while calling into the per-map mutexes.
	type cand struct {
		p       *ProxyInfo
		down    bool
		ready   bool
		latency time.Duration
	}
	cands := make([]cand, 0, len(list))
	for _, p := range list {
		s.downMu.Lock()
		expiry, down := s.down[p.URL]
		downOk := down && now.Before(expiry)
		s.downMu.Unlock()

		s.readyMu.Lock()
		at, isR := s.ready[p.URL]
		ready := isR && now.Sub(at) <= proxyReadyTTL
		s.readyMu.Unlock()

		s.readyLatencyMu.Lock()
		latency := s.readyLatency[p.URL]
		s.readyLatencyMu.Unlock()

		cands = append(cands, cand{p: p, down: downOk, ready: ready, latency: latency})
	}

	// Among ready proxies, prefer the lowest measured latency. A latency of
	// zero means nobody has measured this proxy, not that it is infinitely
	// fast: treating it as fast pinned every request to whichever proxy
	// happened to gain its readiness mark from a successful request rather
	// than from a probe, which is the proxy most likely to be busy.
	//
	// Proxies that tie on latency (notably Tor instances, which all report
	// 0 because their readiness is never re-measured) are distributed
	// round-robin via pickCounter instead of always landing on the first one
	// in pool order, so load spreads across the pool.
	var readyBest []cand
	var bestLatency time.Duration
	hasBest := false
	var fallbacks []cand
	for _, c := range cands {
		if c.down || s.isBlacklisted(c.p.URL) {
			continue
		}
		if c.ready {
			if !hasBest || faster(c.latency, bestLatency) {
				readyBest = readyBest[:0]
				readyBest = append(readyBest, c)
				bestLatency = c.latency
				hasBest = true
			} else if c.latency == bestLatency {
				readyBest = append(readyBest, c)
			}
		} else {
			fallbacks = append(fallbacks, c)
		}
	}

	if len(readyBest) > 0 {
		if len(readyBest) == 1 {
			return readyBest[0].p
		}
		idx := int(s.pickCounter.Add(1)-1) % len(readyBest)
		return readyBest[idx].p
	}
	if len(fallbacks) > 0 {
		idx := int(s.pickCounter.Add(1)-1) % len(fallbacks)
		return fallbacks[idx].p
	}
	return nil
}

// faster reports whether candidate latency a should displace the incumbent.
// A zero latency is the "never measured" sentinel: it loses to any real
// measurement, and two of them compare equal.
func faster(a, best time.Duration) bool {
	if a == best {
		return false
	}
	if best == 0 {
		return true // an unmeasured incumbent is beaten by anything measured
	}
	if a == 0 {
		return false // but an unmeasured candidate never displaces a measured one
	}
	return a < best
}

func (s *ProxyService) isDown(url string) bool {
	if s.isBlacklisted(url) {
		return true
	}
	s.downMu.Lock()
	defer s.downMu.Unlock()
	expiry, ok := s.down[url]
	if !ok {
		return false
	}
	if time.Now().After(expiry) {
		delete(s.down, url)
		return false
	}
	return true
}

// MarkDown puts a proxy into cooldown for d, so it is skipped by pick() until
// the cooldown expires. Used when a proxy fails at the transport layer. It also
// withdraws the readiness mark: a proxy that just failed is by definition not
// ready, and keeping the mark would make the failure that caused it invisible
// to the ready-pool count.
func (s *ProxyService) MarkDown(url string, d time.Duration) {
	s.downMu.Lock()
	if s.down == nil {
		s.down = make(map[string]time.Time)
	}
	s.down[url] = time.Now().Add(d)
	s.downMu.Unlock()

	// Terminal blacklist: a proxy that fails once is never retried. A negative
	// duration is an explicit "expired cooldown" probe (tests) and must not
	// promote the proxy to the permanent blacklist. Tor daemons are exempt:
	// they are long-running and recover from intermittent failures, so a
	// transient failure only earns them a cooldown, not a permanent ban.
	if d >= 0 && !s.isTorProxy(url) {
		s.blacklistedMu.Lock()
		if s.blacklisted == nil {
			s.blacklisted = make(map[string]struct{})
		}
		s.blacklisted[url] = struct{}{}
		s.blacklistedMu.Unlock()
	}

	// Attribute this failure to the proxy's source for dashboard stats.
	s.sourceMu.Lock()
	if s.sourceOf == nil {
		s.sourceOf = make(map[string]string)
	}
	if s.sourceIndex == nil {
		s.sourceIndex = make(map[string]int)
	}
	if s.seenProxy == nil {
		s.seenProxy = make(map[string]bool)
	}
	if s.failedProxy == nil {
		s.failedProxy = make(map[string]bool)
	}
	if s.sourceTotal == nil {
		s.sourceTotal = make(map[string]int)
	}
	if s.sourceFailed == nil {
		s.sourceFailed = make(map[string]int)
	}
	if d >= 0 && !s.isTorProxy(url) && !s.failedProxy[url] {
		s.failedProxy[url] = true
		if src := s.sourceOf[url]; src != "" {
			s.sourceFailed[src]++
		}
	}
	s.sourceMu.Unlock()

	s.dropReady(url)
}

func (s *ProxyService) Blacklist(url string)             { s.MarkDown(url, 0) }
func (s *ProxyService) BlacklistProxy(url string)        { s.MarkDown(url, 0) }
func (s *ProxyService) MarkBlacklisted(url string)       { s.MarkDown(url, 0) }
func (s *ProxyService) MarkProxyBlacklisted(url string)  { s.MarkDown(url, 0) }
func (s *ProxyService) MarkProxyDownTerminal(url string) { s.MarkDown(url, 0) }

func (s *ProxyService) IsBlacklisted(url string) bool      { return s.isBlacklisted(url) }
func (s *ProxyService) IsProxyBlacklisted(url string) bool { return s.isBlacklisted(url) }

// isTorProxy reports whether url names one of the Tor daemons this service
// spawned. Tor instances are long-running and recover from intermittent
// failures, so they must never be placed on the terminal blacklist.
func (s *ProxyService) isTorProxy(url string) bool {
	s.torStateMu.Lock()
	defer s.torStateMu.Unlock()
	if s.torState == nil {
		return false
	}
	_, ok := s.torState[url]
	return ok
}

func (s *ProxyService) isBlacklisted(url string) bool {
	if s.isTorProxy(url) {
		return false
	}
	s.blacklistedMu.Lock()
	defer s.blacklistedMu.Unlock()
	_, ok := s.blacklisted[url]
	return ok
}

func (s *ProxyService) dropReady(url string) {
	s.readyMu.Lock()
	delete(s.ready, url)
	s.readyMu.Unlock()
	s.readyLatencyMu.Lock()
	delete(s.readyLatency, url)
	s.readyLatencyMu.Unlock()
}

// MarkReady records that a proxy was just proven usable. Called when a request
// actually succeeded through it (the strongest possible evidence) and by the
// readiness probe.
func (s *ProxyService) MarkReady(url string) { s.markReady(url, 0) }

func (s *ProxyService) markReady(url string, latency time.Duration) {
	s.readyMu.Lock()
	if s.ready == nil {
		s.ready = make(map[string]time.Time)
	}
	s.ready[url] = time.Now()
	s.readyMu.Unlock()
	if latency > 0 {
		s.readyLatencyMu.Lock()
		if s.readyLatency == nil {
			s.readyLatency = make(map[string]time.Duration)
		}
		s.readyLatency[url] = latency
		s.readyLatencyMu.Unlock()
	}
}

// isReady reports whether the proxy was proven usable within proxyReadyTTL.
func (s *ProxyService) isReady(url string) bool {
	s.readyMu.Lock()
	defer s.readyMu.Unlock()
	at, ok := s.ready[url]
	if !ok {
		return false
	}
	if time.Since(at) > proxyReadyTTL {
		delete(s.ready, url)
		return false
	}
	return true
}

// readyCount counts proxies whose readiness has not expired, dropping the
// expired marks as it goes.
func (s *ProxyService) readyCount() int {
	s.readyMu.Lock()
	defer s.readyMu.Unlock()
	n := 0
	for url, at := range s.ready {
		if time.Since(at) > proxyReadyTTL {
			delete(s.ready, url)
			continue
		}
		n++
	}
	return n
}

// SetRequiredReady sets the minimum number of simultaneously-ready proxies.
func (s *ProxyService) SetRequiredReady(n int) {
	if n < 0 {
		n = 0
	}
	s.required.Store(int32(n))
}

// RequiredReady returns the current minimum.
func (s *ProxyService) RequiredReady() int { return int(s.required.Load()) }

// EnableTor switches the service into Tor-backed mode: instead of fetching HTTP
// proxy lists it spawns n Tor daemons, each providing one outbound IP (exit
// node). Exit nodes are restricted to the Americas and Europe and routing is
// tuned for fewer hops. n <= 0 disables Tor mode.
func (s *ProxyService) EnableTor(n int) {
	s.torMu.Lock()
	defer s.torMu.Unlock()
	if n <= 0 {
		s.torInstances = 0
		s.torMode = false
		return
	}
	s.torInstances = n
	s.torMode = true
}

// TorMode reports whether the service is backed by Tor.
func (s *ProxyService) TorMode() bool {
	s.torMu.Lock()
	defer s.torMu.Unlock()
	return s.torMode
}

// TorInstances returns the configured number of Tor daemons.
func (s *ProxyService) TorInstances() int {
	s.torMu.Lock()
	defer s.torMu.Unlock()
	return s.torInstances
}

// torInstanceState tracks one spawned Tor daemon's observed bootstrap state.
// It is keyed by SOCKS URL in s.torState.
type torInstanceState struct {
	// URL is the SOCKS5 URL the pool hands out for this instance.
	URL string
	// ControlAddr is the Tor control port used to query bootstrap status.
	ControlAddr string
	// CookiePath is the control-port cookie file used for authentication.
	CookiePath string
	// DataDir is the Tor data directory (used for restarts).
	DataDir string
	// TorrcPath is the torrc file path (used for restarts).
	TorrcPath string
	// SocksAddr is the Tor SOCKS port address.
	SocksAddr string
	// StartedAt is when tornago launched the daemon.
	StartedAt time.Time
	// Progress is the last bootstrap percentage reported by Tor (0-100).
	Progress int
	// Err is the last error seen while querying the control port.
	Err string
	// CheckedAt is when the state was last refreshed.
	CheckedAt time.Time
	// Process is the running Tor daemon (may be nil if exited).
	Process *tornago.TorProcess
	// PID is the OS process ID of the Tor daemon.
	PID int
}

// TorInstanceStatus is the dashboard-facing view of one Tor daemon.
type TorInstanceStatus struct {
	URL         string    `json:"url"`
	Progress    int       `json:"progress"`
	State       string    `json:"state"`
	Down        bool      `json:"down"`
	DownUntil   time.Time `json:"down_until,omitempty"`
	Blacklisted bool      `json:"blacklisted,omitempty"`
	Error       string    `json:"error,omitempty"`
	LastChecked time.Time `json:"last_checked"`
	StartedAt   time.Time `json:"started_at"`
}

// TorStatus is the dashboard-facing view of the Tor-backed proxy pool. When
// Tor mode is disabled, Enabled is false and Instances is empty.
type TorStatus struct {
	Enabled       bool                `json:"enabled"`
	Configured    int                 `json:"configured"`
	Running       bool                `json:"running"`
	Ready         int                 `json:"ready"`
	RequiredReady int                 `json:"required_ready"`
	Instances     []TorInstanceStatus `json:"instances"`
}

// TorStatus snapshots the Tor-backed proxy pool for the dashboard. It tells
// an operator whether Tor mode is on, how many daemons were configured, how
// many are currently proven ready, and -- per instance -- whether Tor has
// finished bootstrapping its first circuit ("ready") or is still booting
// ("bootstrapping"). A Tor instance whose control port cannot be queried
// shows "unreachable".
func (s *ProxyService) TorStatus() TorStatus {
	s.torMu.Lock()
	enabled := s.torMode
	configured := s.torInstances
	s.torMu.Unlock()

	out := TorStatus{
		Enabled:       enabled,
		Configured:    configured,
		Running:       s.running.Load(),
		Ready:         s.readyCount(),
		RequiredReady: int(s.required.Load()),
		Instances:     []TorInstanceStatus{},
	}
	if !enabled {
		return out
	}

	s.torStateMu.Lock()
	instances := make([]*torInstanceState, 0, len(s.torState))
	for _, st := range s.torState {
		instances = append(instances, st)
	}
	s.torStateMu.Unlock()

	for _, st := range instances {
		item := TorInstanceStatus{
			URL:         st.URL,
			Progress:    st.Progress,
			Error:       st.Err,
			LastChecked: st.CheckedAt,
			StartedAt:   st.StartedAt,
		}
		switch {
		case st.Err != "":
			item.State = "unreachable"
		case st.Progress >= 100:
			item.State = "ready"
		default:
			item.State = "bootstrapping"
		}
		s.downMu.Lock()
		expiry, down := s.down[st.URL]
		s.downMu.Unlock()
		if down && !time.Now().After(expiry) {
			item.Down = true
			item.DownUntil = expiry
		}
		item.Blacklisted = s.isBlacklisted(st.URL)
		out.Instances = append(out.Instances, item)
	}
	sort.Slice(out.Instances, func(i, j int) bool {
		return out.Instances[i].URL < out.Instances[j].URL
	})
	return out
}

// torStatusLoop periodically refreshes each Tor daemon's bootstrap state
// from its control port until the process shuts down.
func (s *ProxyService) torStatusLoop(ctx context.Context) {
	ticker := time.NewTicker(torStatusInterval)
	defer ticker.Stop()
	s.refreshTorStatus(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.refreshTorStatus(ctx)
		}
	}
}

// refreshTorStatus queries every spawned Tor daemon for its bootstrap phase
// and stores the observed state. It is safe to call concurrently; Tor's
// control port is queried one instance at a time so a slow daemon cannot
// pile up queries. If a daemon has exited, it is reported as unreachable.
func (s *ProxyService) refreshTorStatus(ctx context.Context) {
	s.torStateMu.Lock()
	instances := make([]*torInstanceState, 0, len(s.torState))
	for _, st := range s.torState {
		instances = append(instances, st)
	}
	s.torStateMu.Unlock()

	for _, st := range instances {
		// Check if the Tor process is still alive.
		if !torProcessAlive(st.PID) {
			s.torStateMu.Lock()
			cur := s.torState[st.URL]
			if cur == st {
				cur.Err = "tor process exited"
				cur.CheckedAt = time.Now()
			}
			s.torStateMu.Unlock()
			continue
		}

		progress, err := torBootstrapProgress(ctx, st.ControlAddr, st.CookiePath)
		s.torStateMu.Lock()
		cur := s.torState[st.URL]
		if cur == st {
			cur.Progress = progress
			cur.CheckedAt = time.Now()
			if err != nil {
				cur.Err = err.Error()
			} else {
				cur.Err = ""
				// A fully bootstrapped Tor daemon stays in the ready pool so
				// round-robin selection keeps distributing across instances
				// instead of letting their readiness TTLs lapse and falling
				// back to the unproven path.
				if progress >= 100 {
					s.markReady(st.URL, 0)
				}
			}
		}
		s.torStateMu.Unlock()
	}
}

// torProcessAlive reports whether the process with the given PID is still
// running. A PID of 0 or a process that cannot be found is treated as dead.
func torProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Unix, FindProcess always succeeds; signal 0 checks existence.
	err = proc.Signal(syscall.Signal(0))
	return err == nil
}

// torBootstrapProgress returns the bootstrap percentage reported by the Tor
// control port at addr, authenticated with cookiePath. A zero progress with
// no error means Tor has just started and is building its first circuit.
func torBootstrapProgress(ctx context.Context, controlAddr, cookiePath string) (int, error) {
	if controlAddr == "" || cookiePath == "" {
		return 0, fmt.Errorf("no control port configured")
	}
	client, err := tornago.NewControlClient(controlAddr, tornago.ControlAuthFromCookie(cookiePath), torControlTimeout)
	if err != nil {
		return 0, err
	}
	defer client.Close()
	qctx, cancel := context.WithTimeout(ctx, torControlTimeout)
	defer cancel()
	raw, err := client.GetInfo(qctx, "status/bootstrap-phase")
	if err != nil {
		return 0, err
	}
	return parseTorBootstrapProgress(raw), nil
}

// parseTorBootstrapProgress extracts the integer progress from a Tor
// bootstrap-phase line such as:
//
//	NOTICE BOOTSTRAP PROGRESS=100 TAG=done SUMMARY="Done"
func parseTorBootstrapProgress(raw string) int {
	for _, field := range strings.Fields(raw) {
		if v, ok := strings.CutPrefix(field, "PROGRESS="); ok {
			if n, err := strconv.Atoi(v); err == nil {
				return n
			}
		}
	}
	return 0
}

// SetProbeTargets stores the provider base URLs the readiness probe tunnels
// through each proxy and verifies TLS against. Only HTTPS targets are useful
// for catching MITM proxies; non-HTTPS (or empty) targets are ignored, and
// with no HTTPS targets the probe falls back to the cheap TCP-only check.
func (s *ProxyService) SetProbeTargets(targets []string) {
	out := make([]string, 0, len(targets))
	seen := make(map[string]bool, len(targets))
	for _, t := range targets {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	s.probeMu.Lock()
	s.probeTargets = out
	s.probeMu.Unlock()
}

// probeTargetsSnapshot returns a copy of the current probe targets.
func (s *ProxyService) probeTargetsSnapshot() []string {
	s.probeMu.RLock()
	defer s.probeMu.RUnlock()
	out := make([]string, len(s.probeTargets))
	copy(out, s.probeTargets)
	return out
}

// ReadyCount exposes the current ready-pool size (for tests and the dashboard).
func (s *ProxyService) ReadyCount() int { return s.readyCount() }

// topUpReady probes pool members until the ready pool meets the requirement.
//
// This is what makes a proxy "ready available" when a request needs one: the
// requirement is the sum of the configured proxy budgets, and this pass keeps
// that many proxies proven-reachable at all times rather than waiting for a
// request to discover the pool is empty. Probes are bounded per cycle so a pool
// of dead proxies cannot stall the loop.
func (s *ProxyService) topUpReady(ctx context.Context) {
	need := s.RequiredReady()
	if need <= 0 {
		return
	}
	if have := s.readyCount(); have >= need {
		return
	}

	list := s.pool.get()

	// Collect the proxies eligible for probing this pass, capped at
	// maxProbesPerCycle so a pool of dead proxies cannot stall the loop.
	var candidates []*ProxyInfo
	for _, p := range list {
		if ctx.Err() != nil {
			return
		}
		if s.readyCount() >= need {
			break
		}
		if s.isReady(p.URL) || s.isDown(p.URL) {
			continue
		}
		candidates = append(candidates, p)
	}

	// Probe up to proxyProbeConcurrency proxies at a time so a pass over a
	// large pool completes in a fraction of the serial time.
	probes := 0
	for i := 0; i < len(candidates); i += proxyProbeConcurrency {
		if ctx.Err() != nil {
			return
		}
		if s.readyCount() >= need {
			break
		}
		end := i + proxyProbeConcurrency
		if end > len(candidates) {
			end = len(candidates)
		}
		batch := candidates[i:end]

		var wg sync.WaitGroup
		for _, p := range batch {
			if s.readyCount() >= need {
				break
			}
			probes++
			wg.Add(1)
			go func(p *ProxyInfo) {
				defer wg.Done()
				if s.probeReachable(ctx, p) {
					s.markReady(p.URL, 0)
				} else {
					// Charge the failed probe to the proxy, not the pool: this is
					// the same attribution the request path makes for a transport
					// failure.
					s.MarkDown(p.URL, proxyCooldown)
				}
			}(p)
		}
		wg.Wait()
	}

	if have := s.readyCount(); have < need {
		log.Printf("[proxy] ready pool %d/%d (probed %d of %d) - not enough reachable proxies",
			have, need, probes, len(list))
	} else {
		// Only reached when this pass actually had to top up, so it is not
		// periodic noise: readiness lapses after proxyReadyTTL, and each lapse
		// is reported once when it is repaired.
		log.Printf("[proxy] ready pool %d/%d (probed %d of %d)", have, need, probes, len(list))
	}
}

// probeReachable reports whether the proxy is actually usable: it must answer
// TCP, stay under the latency threshold, and -- when provider targets are
// configured -- tunnel an HTTPS request to a real provider origin with a
// certificate that verifies. The last step is what retires MITM / TLS-breaking
// proxies at probe time instead of on a live request. With no HTTPS targets
// configured the probe keeps the cheap TCP-only behavior (backward compatible).
func (s *ProxyService) probeReachable(ctx context.Context, p *ProxyInfo) bool {
	addr := net.JoinHostPort(p.IP, p.Port)
	if p.IP == "" || p.Port == "" {
		return false
	}
	d := net.Dialer{Timeout: proxyVerifyTimeout}
	start := time.Now()
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	latency := time.Since(start)
	conn.Close()

	if latency > maxAcceptableLatency {
		return false
	}

	// Record the measured latency so pick() can prefer faster proxies.
	s.readyLatencyMu.Lock()
	if s.readyLatency == nil {
		s.readyLatency = make(map[string]time.Duration)
	}
	s.readyLatency[p.URL] = latency
	s.readyLatencyMu.Unlock()

	// Collect HTTPS probe targets. If none are configured, fall back to the
	// cheap TCP-only probe so unconfigured/legacy callers keep working.
	targets := s.probeTargetsSnapshot()
	var httpsTargets []string
	for _, t := range targets {
		if strings.HasPrefix(strings.ToLower(t), "https://") {
			httpsTargets = append(httpsTargets, t)
		}
	}
	if len(httpsTargets) == 0 {
		return true
	}

	// Tunnel a real HTTPS request through the proxy and require the TLS
	// handshake (and cert verification) to succeed. The HTTP status code is
	// deliberately not checked: a proxy that tunnels faithfully but gets a 403
	// from the origin is still a working proxy.
	base := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: proxyVerifyTimeout}).DialContext,
		TLSHandshakeTimeout: proxyVerifyTimeout,
	}
	tr := s.TransportFor(base, p)
	if tr == nil {
		return false
	}
	client := &http.Client{Transport: tr, Timeout: proxyProbeHTTPTimeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpsTargets[0], nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return true
}

// Start launches the periodic refresh loop. It blocks until stopCh is closed
// or ctx is cancelled. It performs an immediate refresh on startup.
func (s *ProxyService) Start(ctx context.Context) {
	if s.running.Swap(true) {
		return // already running
	}
	defer s.running.Store(false)

	// Tor-backed mode: spawn N tor daemons and use them as the proxy pool.
	// No HTTP source fetching or refresh loop.
	if s.TorMode() {
		s.startTorInstances(ctx)
		// Keep alive until stopped, and periodically refresh each daemon's
		// bootstrap state so the dashboard can distinguish a still-booting
		// instance from a broken one.
		go s.torStatusLoop(ctx)
		for {
			select {
			case <-ctx.Done():
				s.stopTorInstances()
				return
			case <-s.stopCh:
				s.stopTorInstances()
				return
			}
		}
	}

	// Immediate refresh so the pool is populated before the first request, then
	// an immediate top-up so the ready pool is already at its required size
	// instead of getting there one slow request at a time.
	s.refresh(ctx)
	s.topUpReady(ctx)
	s.topUpReady(ctx)

	refreshTicker := time.NewTicker(s.refreshInterval)
	defer refreshTicker.Stop()
	// The ready pool is re-checked far more often than the pool is refreshed:
	// readiness expires after proxyReadyTTL, so a refresh-interval-only loop
	// would leave the ready count at zero for most of every cycle.
	topUpTicker := time.NewTicker(proxyVerifyInterval)
	defer topUpTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-refreshTicker.C:
			s.refresh(ctx)
			s.topUpReady(ctx)
		case <-topUpTicker.C:
			s.topUpReady(ctx)
		}
	}
}

// Stop signals the refresh loop to exit.
func (s *ProxyService) Stop() {
	s.stopOnce.Do(func() { close(s.stopCh) })
}

// torExitNodes is the set of exit-node country codes (Americas + Europe) Tor
// is restricted to, keeping circuits on fast, nearby exits.
var torExitNodes = "{us},{ca},{mx},{ar},{br},{cl},{co},{pe},{ve},{de},{fr},{gb},{nl},{se},{ch},{it},{es},{pl},{pt},{be},{at},{cz},{dk},{fi},{no},{ie},{gr},{hu},{ro}"

// cleanStaleTorDataDirs removes data directories from previous runs that are
// older than 1 hour. This prevents conflicts with orphaned Tor processes
// from previous runs.
func cleanStaleTorDataDirs(baseDir string) {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "airouter-tor-") {
			continue
		}
		dir := filepath.Join(baseDir, e.Name())
		info, err := os.Stat(dir)
		if err != nil {
			continue
		}
		if time.Since(info.ModTime()) > time.Hour {
			_ = os.RemoveAll(dir)
		}
	}
}

// startTorInstances spawns s.torInstances Tor daemons and registers their SOCKS5
// endpoints in the pool as ready proxies.
func (s *ProxyService) startTorInstances(ctx context.Context) {
	n := s.torInstances
	if n <= 0 {
		return
	}

	// Clean up stale data directories from previous runs.
	baseDir := ""
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		baseDir = filepath.Join(home, ".local", "state", "airouter")
	} else {
		baseDir = os.TempDir()
	}
	cleanStaleTorDataDirs(baseDir)

	s.torMu.Lock()
	defer s.torMu.Unlock()

	var list []*ProxyInfo
	for i := 0; i < n; i++ {
		if err := os.MkdirAll(baseDir, 0o700); err != nil {
			log.Printf("[tor] cannot create base dir: %v", err)
			continue
		}
		dataDir := filepath.Join(baseDir, fmt.Sprintf("airouter-tor-%d-%d", os.Getpid(), i))
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			log.Printf("[tor] cannot create data dir: %v", err)
			continue
		}
		// resolve free ports
		socksAddr, err := resolveFreeAddr()
		if err != nil {
			log.Printf("[tor] socks addr: %v", err)
			continue
		}
		controlAddr, err := resolveFreeAddr()
		if err != nil {
			log.Printf("[tor] control addr: %v", err)
			continue
		}
		cookiePath := filepath.Join(dataDir, "control_auth_cookie")
		torrcPath := filepath.Join(dataDir, "torrc")
		torrc := fmt.Sprintf(`SocksPort %s
ControlPort %s
DataDirectory %s
CookieAuthentication 1
CookieAuthFile %s
ClientUseIPv6 0
RunAsDaemon 0
Log notice stdout
ExitNodes %s
StrictNodes 1
NumEntryGuards 1
`, socksAddr, controlAddr, dataDir, cookiePath, torExitNodes)
		if err := os.WriteFile(torrcPath, []byte(torrc), 0o600); err != nil {
			log.Printf("[tor] write torrc: %v", err)
			continue
		}
		cfg, err := tornago.NewTorLaunchConfig(
			tornago.WithTorSocksAddr(socksAddr),
			tornago.WithTorControlAddr(controlAddr),
			tornago.WithTorDataDir(dataDir),
			tornago.WithTorConfigFile(torrcPath),
			tornago.WithTorStartupTimeout(90*time.Second),
		)
		if err != nil {
			log.Printf("[tor] launch config error: %v", err)
			continue
		}
		proc, err := tornago.StartTorDaemon(cfg)
		if err != nil {
			log.Printf("[tor] failed to start daemon %d: %v", i, err)
			continue
		}
		s.torProcesses = append(s.torProcesses, proc)
		host, port, err := net.SplitHostPort(proc.SocksAddr())
		if err != nil {
			log.Printf("[tor] bad socks addr %q: %v", proc.SocksAddr(), err)
			continue
		}
		list = append(list, &ProxyInfo{
			URL:      "socks5://" + net.JoinHostPort(host, port),
			Protocol: "socks5",
			IP:       host,
			Port:     port,
		})
		s.torStateMu.Lock()
		if s.torState == nil {
			s.torState = make(map[string]*torInstanceState)
		}
		socksURL := "socks5://" + net.JoinHostPort(host, port)
		s.torState[socksURL] = &torInstanceState{
			URL:         socksURL,
			ControlAddr: controlAddr,
			CookiePath:  cookiePath,
			DataDir:     dataDir,
			TorrcPath:   torrcPath,
			SocksAddr:   socksAddr,
			StartedAt:   time.Now(),
			Process:     proc,
			PID:         proc.PID(),
		}
		s.torStateMu.Unlock()
	}
	s.pool.set(list)
	for _, p := range list {
		s.markReady(p.URL, 0)
	}
	log.Printf("[tor] started %d/%d instances", len(list), n)
}

// resolveFreeAddr returns a 127.0.0.1:0-resolved listen address string.
func resolveFreeAddr() (string, error) {
	lc := net.ListenConfig{}
	l, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := l.Addr().String()
	l.Close()
	return addr, nil
}

// stopTorInstances terminates all spawned Tor daemons.
func (s *ProxyService) stopTorInstances() {
	s.torMu.Lock()
	defer s.torMu.Unlock()
	for _, p := range s.torProcesses {
		_ = p.Stop()
	}
	s.torProcesses = nil
	// Clean up data directories.
	s.torStateMu.Lock()
	for _, st := range s.torState {
		if st.DataDir != "" {
			_ = os.RemoveAll(st.DataDir)
		}
	}
	s.torState = make(map[string]*torInstanceState)
	s.torStateMu.Unlock()
}

// TransportFor returns an http.Transport configured to route through the given
// proxy: HTTPS (HTTP CONNECT) proxies set Transport.Proxy, SOCKS5 proxies set
// Transport.DialContext to a SOCKS5 dialer. The returned transport is a clone
// of the shared base transport so connection pooling is preserved across
// attempts. It returns nil when no transport can be built for the proxy.
//
// Returning nil rather than a bare clone matters. The callers attribute the
// attempt to the proxy -- marking it ready on success, cooling it down on
// failure -- so handing back a transport with no proxy configured would let the
// request travel direct AND credit the proxy with a success it never carried.
func (s *ProxyService) TransportFor(base *http.Transport, p *ProxyInfo) *http.Transport {
	tr := base.Clone()
	switch p.Protocol {
	case "socks5":
		dialer, err := proxy.SOCKS5("tcp", net.JoinHostPort(p.IP, p.Port), nil, proxy.Direct)
		if err != nil {
			// Unreachable against x/net v0.61, where SOCKS5 builds its dialer
			// without touching the network and returns a nil error. Handled
			// because the alternative is a clone that dials direct.
			log.Printf("[proxy] cannot build a SOCKS5 dialer for %s: %v", p.URL, err)
			return nil
		}
		tr.DialContext = dialer.(proxy.ContextDialer).DialContext
	default:
		proxyURL, err := url.Parse(p.URL)
		if err != nil {
			// Also unreachable in practice: the URL is assembled by proxyURLFor
			// from a host and port the extractors have already validated.
			log.Printf("[proxy] unusable proxy URL %q: %v", p.URL, err)
			return nil
		}
		tr.Proxy = http.ProxyURL(proxyURL)
	}
	return tr
}

// ProxySourceStat is per-source failure statistics for the dashboard.
type ProxySourceStat struct {
	SourceURL   string  `json:"source_url"`
	SourceIndex int     `json:"source_index"`
	Total       int     `json:"total"`
	Failed      int     `json:"failed"`
	FailureRate float64 `json:"failure_rate"`
}

// ProxySourceStats returns per-source proxy failure rates, sorted by failure
// rate descending.
func (s *ProxyService) ProxySourceStats() []ProxySourceStat {
	s.sourceMu.Lock()
	defer s.sourceMu.Unlock()
	if s.sourceOf == nil {
		s.sourceOf = make(map[string]string)
	}
	if s.sourceIndex == nil {
		s.sourceIndex = make(map[string]int)
	}
	if s.seenProxy == nil {
		s.seenProxy = make(map[string]bool)
	}
	if s.failedProxy == nil {
		s.failedProxy = make(map[string]bool)
	}
	if s.sourceTotal == nil {
		s.sourceTotal = make(map[string]int)
	}
	if s.sourceFailed == nil {
		s.sourceFailed = make(map[string]int)
	}
	out := make([]ProxySourceStat, 0, len(s.sourceTotal))
	for src, total := range s.sourceTotal {
		failed := s.sourceFailed[src]
		rate := 0.0
		if total > 0 {
			rate = float64(failed) / float64(total)
		}
		out = append(out, ProxySourceStat{
			SourceURL:   src,
			SourceIndex: s.sourceIndex[src],
			Total:       total,
			Failed:      failed,
			FailureRate: rate,
		})
	}
	// sort by failure rate desc, then source index asc
	// (simple insertion sort to avoid import)
	for i := 1; i < len(out); i++ {
		j := i
		for j > 0 && (out[j-1].FailureRate < out[j].FailureRate ||
			(out[j-1].FailureRate == out[j].FailureRate && out[j-1].SourceIndex > out[j].SourceIndex)) {
			out[j-1], out[j] = out[j], out[j-1]
			j--
		}
	}
	return out
}
