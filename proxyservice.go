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
type ProxyService struct {
	sources         []string
	apiKey          string
	client          *http.Client
	refreshInterval time.Duration
	pool            proxyPool
	stopCh          chan struct{}
	running         atomic.Bool

	downMu sync.Mutex
	down   map[string]time.Time // proxy URL -> cooldown expiry
}

// NewProxyService builds a proxy service from the sources file. If no sources
// are configured or the API key is missing, the service is still created but
// geolocation will treat every proxy as non-preferred (backup).
func NewProxyService(sourcesFile string, refreshInterval time.Duration) *ProxyService {
	var sources []string
	if data, err := os.ReadFile(sourcesFile); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			sources = append(sources, line)
		}
	}

	apiKey := os.Getenv("IPGEOLOCATION_API_KEY")

	return &ProxyService{
		sources: sources,
		apiKey:  apiKey,
		client: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 10 * time.Second,
			},
		},
		refreshInterval: refreshInterval,
		stopCh:          make(chan struct{}),
	}
}

// geolocate looks up the country and continent for an IP using the
// IPGeolocation.io API (the same API the ipgeolocation CLI calls). On failure
// it returns empty strings, which marks the proxy as non-preferred.
func (s *ProxyService) geolocate(ip string) (country, continent string) {
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
func (s *ProxyService) fetchAll() []*ProxyInfo {
	type tagged struct {
		proxy  *ProxyInfo
		prefer bool // true for EU/NA
		source int  // lower = higher priority
		order  int  // sequence within source
	}

	var all []tagged
	order := 0
	for idx, source := range s.sources {
		body, err := s.fetchSource(source)
		if err != nil {
			log.Printf("[proxy] source %d (%s) fetch failed: %v", idx, source, err)
			continue
		}
		proxies := parseProxies(body, source, idx)
		log.Printf("[proxy] source %d (%s): %d proxies parsed", idx, source, len(proxies))
		for _, p := range proxies {
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

// refresh fetches all sources and replaces the pool. Called every
// refreshInterval and once at startup.
func (s *ProxyService) refresh() {
	list := s.fetchAll()
	s.pool.set(list)
	log.Printf("[proxy] pool refreshed: %d proxies (%d preferred, %d backup)",
		len(list), s.countPreferred(list), len(list)-s.countPreferred(list))
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

// pick returns the next proxy from the pool using round-robin, skipping
// proxies that are currently in cooldown. If every proxy is down, returns nil
// so the caller can fall back to a direct connection.
func (s *ProxyService) pick() *ProxyInfo {
	list := s.pool.get()
	if len(list) == 0 {
		return nil
	}
	// Probe at most len(list) proxies to find one that isn't down. Each call
	// to pool.pick() advances the round-robin cursor, so down proxies are
	// skipped in order and revisited once their cooldown expires.
	n := len(list)
	for i := 0; i < n; i++ {
		p := s.pool.pick()
		if !s.isDown(p.URL) {
			return p
		}
	}
	return nil // all proxies are down
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
// the cooldown expires. Used when a proxy fails at the transport layer.
func (s *ProxyService) MarkDown(url string, d time.Duration) {
	s.downMu.Lock()
	defer s.downMu.Unlock()
	if s.down == nil {
		s.down = make(map[string]time.Time)
	}
	s.down[url] = time.Now().Add(d)
}

// Start launches the periodic refresh loop. It blocks until stopCh is closed
// or ctx is cancelled. It performs an immediate refresh on startup.
func (s *ProxyService) Start(ctx context.Context) {
	if s.running.Swap(true) {
		return // already running
	}
	defer s.running.Store(false)

	// Immediate refresh so the pool is populated before the first request.
	s.refresh()

	ticker := time.NewTicker(s.refreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.refresh()
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
