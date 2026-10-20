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
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/proxy"
)

// ProxyInfo describes a single proxy server in the pool.
type ProxyInfo struct {
	URL         string // full proxy URL, e.g. "http://ip:port" or "socks5://ip:port"
	Protocol    string // "https" or "socks5"
	IP          string
	Port        string
	Country     string        // ISO country code (e.g. "US", "DE")
	Continent   string        // continent code (e.g. "NA", "EU")
	SourceURL   string        // source URL this proxy was fetched from
	SourceIndex int           // index in proxy_sources.txt (lower = higher priority)
	Latency     time.Duration // measured dial latency; lower is preferred
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
type proxyPool struct {
	mu      sync.RWMutex
	list    []*ProxyInfo
	nextIdx atomic.Int32 // round-robin cursor into list
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
	pp.nextIdx.Store(0)
	pp.mu.Unlock()
}

func (pp *proxyPool) len() int {
	pp.mu.RLock()
	defer pp.mu.RUnlock()
	return len(pp.list)
}

// pick returns the next proxy from the pool using round-robin.
func (pp *proxyPool) pick() *ProxyInfo {
	pp.mu.RLock()
	list := pp.list
	pp.mu.RUnlock()
	if len(list) == 0 {
		return nil
	}
	idx := int(pp.nextIdx.Add(1) % int32(len(list)))
	return list[idx]
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
	proxyVerifyTimeout = 3 * time.Second
	// maxProbesPerCycle bounds how much of the pool one top-up pass will walk,
	// so a pool where nothing answers cannot stall the loop for minutes.
	maxProbesPerCycle = 40
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

	// ready records when each proxy was last PROVEN usable. An entry older
	// than proxyReadyTTL no longer counts; readiness is continuously re-earned
	// rather than granted once.
	readyMu sync.Mutex
	ready   map[string]time.Time

	// readyLatency records the measured dial latency for each ready proxy,
	// so pick() can prefer the fastest. Entries are evicted together with
	// the readiness mark.
	readyLatencyMu sync.Mutex
	readyLatency   map[string]time.Duration

	// required is the minimum number of simultaneously-ready proxies: the sum
	// of the proxy budgets of every enabled provider, so that a request which
	// needs N proxies has N ready to draw on. Pushed in from the config by
	// Proxy.UpdateRequiredReady on startup and on every hot reload.
	required atomic.Int32

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
		ready:           make(map[string]time.Time),
		readyLatency:    make(map[string]time.Duration),
		geoCache:        make(map[string]geoEntry),
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
func (s *ProxyService) geolocate(ip string) (country, continent string) {
	s.geoMu.Lock()
	if e, ok := s.geoCache[ip]; ok && time.Since(e.at) < geoCacheTTL {
		s.geoMu.Unlock()
		return e.country, e.continent
	}
	s.geoMu.Unlock()

	country, continent = s.geolocateRemote(ip)
	if country == "" && continent == "" {
		// Do not cache a negative result: a transient API failure would
		// otherwise mark the proxy non-preferred for a whole day.
		return "", ""
	}

	s.geoMu.Lock()
	s.geoCache[ip] = geoEntry{country: country, continent: continent, at: time.Now()}
	s.geoMu.Unlock()
	return country, continent
}

// geolocateRemote looks up the country and continent for an IP using the
// IPGeolocation.io API (the same API the ipgeolocation CLI calls). On failure
// it returns empty strings, which marks the proxy as non-preferred.
func (s *ProxyService) geolocateRemote(ip string) (country, continent string) {
	if s.apiKey == "" {
		return "", ""
	}
	u := "https://api.ipgeolocation.io/v2/ipgeo?apiKey=" + url.QueryEscape(s.apiKey) + "&ip=" + url.QueryEscape(ip)
	req, err := http.NewRequestWithContext(context.Background(), "GET", u, nil)
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
	body, err := io.ReadAll(resp.Body)
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
func (s *ProxyService) fetchSource(sourceURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(context.Background(), "GET", sourceURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("source %s returned status %d", sourceURL, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
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
	re := regexp.MustCompile(`(?is)<td[^>]*>\s*([0-9a-fA-F:.]{1,45})\s*</td>\s*<td[^>]*>\s*(\d{1,5})\s*</td>`)
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
func (s *ProxyService) fetchAll(sources []proxySource) []*ProxyInfo {
	type tagged struct {
		proxy  *ProxyInfo
		prefer bool // true for EU/NA
		source int  // lower = higher priority
		order  int  // sequence within source
	}

	var all []tagged
	order := 0
	for idx, source := range sources {
		body, err := s.fetchSource(source.URL)
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
		result := extract(body)

		// Freshness: a list that says its newest entry was checked more than
		// proxySourceMaxAge ago is a list of addresses that have probably gone
		// dead. Discard the source rather than spend the next requests
		// discovering that one attempt at a time.
		//
		// Sources whose format carries no date (the two raw GitHub text lists)
		// report a zero AsOf. That is genuinely unknown, not stale, so they are
		// kept -- and logged as such, so a silently never-discarded source is
		// visible instead of looking like it passed the check.
		if result.AsOf.IsZero() {
			log.Printf("[proxy] source %d (%s): no date in body, freshness unknown (kept)", idx, source.Name)
		} else if age := time.Since(result.AsOf); age > proxySourceMaxAge {
			log.Printf("[proxy] source %d (%s): DISCARDED, contents dated %s (%s old, limit %s)",
				idx, source.Name, result.AsOf.UTC().Format(time.RFC3339), age.Round(time.Second), proxySourceMaxAge)
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
				c, cont := s.geolocate(p.IP)
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

// refresh fetches every source and replaces the pool. Called every
// refreshInterval and once at startup. The source list is re-resolved each time
// so an edit to the sources file is picked up without a restart.
func (s *ProxyService) refresh() {
	list := s.fetchAll(s.loadSources())
	s.pool.set(list)
	log.Printf("[proxy] pool refreshed: %d proxies (%d preferred, %d backup, %d ready/%d required)",
		len(list), s.countPreferred(list), len(list)-s.countPreferred(list),
		s.readyCount(), s.RequiredReady())
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

	// Among ready proxies, prefer the lowest measured latency.
	var best *ProxyInfo
	var bestLatency time.Duration
	var fallback *ProxyInfo
	for _, c := range cands {
		if c.down {
			continue
		}
		if c.ready {
			if best == nil || c.latency < bestLatency {
				best = c.p
				bestLatency = c.latency
			}
		} else if fallback == nil {
			fallback = c.p
		}
	}

	if best != nil {
		return best
	}
	return fallback
}

func (s *ProxyService) isDown(url string) bool {
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

	s.dropReady(url)
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
	probes := 0
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
		if probes >= maxProbesPerCycle {
			break
		}
		probes++

		if s.probeReachable(ctx, p) {
			s.markReady(p.URL, 0)
		} else {
			// Charge the failed probe to the proxy, not the pool: this is the
			// same attribution the request path makes for a transport failure.
			s.MarkDown(p.URL, proxyCooldown)
		}
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

// probeReachable reports whether a TCP connection to the proxy can be
// established. It is deliberately the cheap half of "usable": it filters out
// the large majority of dead free proxies (unreachable hosts and closed ports)
// at a few hundred milliseconds each, without issuing a request through an
// untrusted proxy. A proxy that answers TCP but refuses CONNECT still fails its
// real attempt, at which point the request path marks it down and withdraws the
// readiness mark, so a false positive costs one attempt rather than a
// corruption of the ready count.
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

	// Record the measured latency so pick() can prefer faster proxies.
	s.readyLatencyMu.Lock()
	if s.readyLatency == nil {
		s.readyLatency = make(map[string]time.Duration)
	}
	s.readyLatency[p.URL] = latency
	s.readyLatencyMu.Unlock()

	return true
}

// Start launches the periodic refresh loop. It blocks until stopCh is closed
// or ctx is cancelled. It performs an immediate refresh on startup.
func (s *ProxyService) Start(ctx context.Context) {
	if s.running.Swap(true) {
		return // already running
	}
	defer s.running.Store(false)

	// Immediate refresh so the pool is populated before the first request, then
	// an immediate top-up so the ready pool is already at its required size
	// instead of getting there one slow request at a time.
	s.refresh()
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
			s.refresh()
			s.topUpReady(ctx)
		case <-topUpTicker.C:
			s.topUpReady(ctx)
		}
	}
}

// Stop signals the refresh loop to exit.
func (s *ProxyService) Stop() {
	close(s.stopCh)
}

// Pool returns a copy of the current proxy pool.
func (s *ProxyService) Pool() []*ProxyInfo {
	return s.pool.get()
}

// TransportFor returns an http.Transport configured to route through the given
// proxy. For HTTPS (HTTP CONNECT) proxies it sets Transport.Proxy; for SOCKS5
// proxies it sets Transport.DialContext to a SOCKS5 dialer. The returned
// transport is a clone of the shared base transport so connection pooling is
// preserved across attempts.
func (s *ProxyService) TransportFor(base *http.Transport, p *ProxyInfo) *http.Transport {
	tr := base.Clone()
	switch p.Protocol {
	case "socks5":
		dialer, err := proxy.SOCKS5("tcp", net.JoinHostPort(p.IP, p.Port), nil, proxy.Direct)
		if err != nil {
			log.Printf("[proxy] failed to create SOCKS5 dialer for %s: %v", p.URL, err)
			return tr
		}
		tr.DialContext = dialer.(proxy.ContextDialer).DialContext
	default:
		proxyURL, err := url.Parse(p.URL)
		if err != nil {
			log.Printf("[proxy] invalid proxy URL %q: %v", p.URL, err)
			return tr
		}
		tr.Proxy = http.ProxyURL(proxyURL)
	}
	return tr
}
