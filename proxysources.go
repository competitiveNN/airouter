package main

// Source registry for the public-proxy pool.
//
// Every source gets its own extractor because the formats genuinely differ:
// one returns JSON with a "protocols" ARRAY, two are plain ip:port lists, and
// three are HTML sites that do not merely not put the port in a <td> -- one of
// them computes the port with obfuscated JavaScript. Format sniffing
// ("is it JSON/HTML/text?") was the previous approach and it quietly extracted
// NOTHING from three of the six live sources, because it looked for
// <td>1.2.3.4</td><td>8080</td> adjacency that none of those sites emit.
//
// Verification of all six sources was done live on 2026-10-19 against captured
// bodies in testdata/proxies/; see proxy_source_test.go for the counts each
// extractor must hit.

import (
	"encoding/json"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// proxySourceMaxAge is how fresh a source's contents must be to be trusted.
// A proxy list that has not been updated in this window is a list of addresses
// that have probably gone dead, so the whole source is discarded rather than
// polluting the pool with probes we will waste attempts on.
const proxySourceMaxAge = 15 * time.Minute

// proxyUserAgent is the User-Agent sent when fetching proxy lists. Some list
// hosts (e.g. spys.one) return 403 to Go's default "Go-http-client/1.1", so we
// identify as a browser to keep the refresh working from any egress.
const proxyUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

// proxydbHrefRe matches the only unambiguous part of a proxydb.net row: the
// anchor href, which carries IP, port and protocol together. The visible port
// is shadowed by a display:none decoy, so matching the <td> text reads the
// decoy rather than the real value.
var proxydbHrefRe = regexp.MustCompile(`href="/([0-9]{1,3}(?:\.[0-9]{1,3}){3})/([0-9]{1,5})#(https?|socks4|socks5)"`)

// sourceExtract is what a source-specific extractor returns.
type sourceExtract struct {
	// Proxies are the usable entries. Extractors must NOT stamp SourceURL or
	// SourceIndex: the registry does that once, after extraction, so the two
	// cannot disagree.
	Proxies []*ProxyInfo

	// AsOf is the newest "last checked" timestamp the body reports for its own
	// contents. Zero means the source carries no usable date, in which case the
	// caller falls back to the HTTP response Date before giving up.
	AsOf time.Time
}

// proxySource is one URL we pull proxies from, with the function that pulls
// them out of whatever comes back.
type proxySource struct {
	// Name is a short human label for log lines.
	Name string

	// URL is the exact source URL. It doubles as the lookup key, so an
	// override file that names a URL we know about gets its real extractor
	// rather than the sniffing fallback.
	URL string

	// Protocol is the protocol every entry from this source has, used when the
	// body does not state one per entry (the two text lists leave it implied).
	Protocol string

	// Extract turns a response body into proxies plus its own as-of date.
	Extract func(body []byte) sourceExtract
}

// defaultProxySources is the built-in source list. Proxy lists are long-lived
// and frequently updated, so they belong in the repo rather than in a file only
// one machine happens to have; docs/proxy_sources.txt keeps the same list in a
// human-readable form for reference.
var defaultProxySources = []proxySource{
	{
		Name: "thordata/awesome-free-proxy-list",
		URL:  "https://raw.githubusercontent.com/Thordata/awesome-free-proxy-list/refs/heads/main/proxies/top-trusted.txt",
		// Plain ip:port lines, no protocol column, no date.
		Protocol: "https",
		Extract:  extractPlainProxyList,
	},
	{
		Name:     "iplocate/free-proxy-list",
		URL:      "https://raw.githubusercontent.com/iplocate/free-proxy-list/refs/heads/main/protocols/https.txt",
		Protocol: "https",
		Extract:  extractPlainProxyList,
	},
	{
		Name: "proxylist.geonode.com",
		URL:  "https://proxylist.geonode.com/api/proxy-list?anonymityLevel=anonymous&protocols=https%2Csocks5&speed=medium&page=1&limit=500&sort_by=responseTime&sort_type=asc",
		// JSON where the protocol is an ARRAY ("protocols":["https"]) and the
		// port is a string. Each entry carries updated_at/lastChecked.
		Protocol: "https",
		Extract:  extractGeonode,
	},
	{
		Name:     "spys.one anonymous",
		URL:      "https://spys.one/en/anonymous-proxy-list/",
		Protocol: "https",
		Extract:  extractorSpysOne("https"),
	},
	{
		Name:     "spys.one socks",
		URL:      "https://spys.one/en/socks-proxy-list/",
		Protocol: "socks5",
		Extract:  extractorSpysOne("socks5"),
	},
	{
		Name:     "proxydb.net",
		URL:      "https://proxydb.net/?anonlvl=4&country=&protocol=https&protocol=socks5&sort_column_id=checked&sort_order_desc=true",
		Protocol: "https",
		Extract:  extractProxydbNet,
	},
}

// extractorFor returns the extractor registered for url, or nil if the URL is
// not one we know about.
func extractorFor(url string) func([]byte) sourceExtract {
	trimmed := strings.TrimSpace(url)
	for i := range defaultProxySources {
		if defaultProxySources[i].URL == trimmed {
			return defaultProxySources[i].Extract
		}
	}
	return nil
}

// defaultProxySourceURLs returns the URLs of the built-in sources.
func defaultProxySourceURLs() []string {
	out := make([]string, 0, len(defaultProxySources))
	for _, s := range defaultProxySources {
		out = append(out, s.URL)
	}
	return out
}

// extractPlainProxyList handles the plain-text "ip:port" lists: one proxy per
// line, optional protocol as a third field or scheme. No date is carried, so
// AsOf stays zero and the freshness rule falls back to the HTTP response.
func extractPlainProxyList(body []byte) sourceExtract {
	var out []*ProxyInfo
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if p := parseTextLine(line); p != nil {
			out = append(out, p)
		}
	}
	return sourceExtract{Proxies: out}
}

// extractGeonode handles proxylist.geonode.com's JSON API.
//
// The shape is {"data":[ ... ]} and each entry's protocol is an ARRAY named
// "protocols": ["https"] or ["https","socks5"]. The previous parser read only
// the scalar "protocol" / "type" fields, so for this source it always fell
// through to the default "https" -- a SOCKS5 entry would have been handed to
// TransportFor as an HTTP CONNECT proxy, where it can only fail.
//
// Ports arrive as JSON strings ("port":"80") in the observed traffic, but
// numeric ports are accepted too rather than silently dropping the entry.
func extractGeonode(body []byte) sourceExtract {
	var doc struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || len(doc.Data) == 0 {
		return sourceExtract{}
	}

	out := make([]*ProxyInfo, 0, len(doc.Data))
	var asOf time.Time
	for _, raw := range doc.Data {
		var e struct {
			IP        string          `json:"ip"`
			Port      json.RawMessage `json:"port"`
			Protocols []string        `json:"protocols"`
			Protocol  string          `json:"protocol"`
			Type      string          `json:"type"`
			Country   string          `json:"country"`
			UpdatedAt string          `json:"updated_at"`
			LastCheck json.Number     `json:"lastChecked"`
		}
		if err := json.Unmarshal(raw, &e); err != nil {
			continue
		}
		if e.IP == "" {
			continue
		}
		port := jsonPortString(e.Port)
		if port == "" {
			continue
		}

		protocol := "https"
		if len(e.Protocols) > 0 {
			// A proxy that speaks SOCKS5 must be dialed as SOCKS5, not as a
			// CONNECT proxy, so prefer it over any HTTP-only listing.
			protocol = "socks5"
			for _, p := range e.Protocols {
				if strings.EqualFold(p, "http") || strings.EqualFold(p, "https") {
					protocol = "https"
					break
				}
			}
		} else if e.Protocol != "" || e.Type != "" {
			protocol = normalizeProxyProtocol(e.Protocol, e.Type)
		}

		p := &ProxyInfo{
			URL:      proxyURLFor(protocol, e.IP, port),
			Protocol: protocol,
			IP:       e.IP,
			Port:     port,
			Country:  strings.ToUpper(e.Country),
		}

		// Freshness: this source reports it per entry, as an RFC3339 timestamp
		// or a unix epoch in lastChecked.
		if t, ok := parseAnyTimestamp(e.UpdatedAt, string(e.LastCheck)); ok && t.After(asOf) {
			asOf = t
		}

		out = append(out, p)
	}
	return sourceExtract{Proxies: out, AsOf: asOf}
}

// extractProxydbNet parses proxydb.net. The port is NOT a plain <td> value: it
// is hidden inside a display:none decoy and then repeated inside an anchor
// whose href carries the protocol as a fragment:
//
//	<a href="/219.148.171.178/9445#https" title="">219.148.171.178</a>
//	<a href="/219.148.171.178/9445#https" title="">9445</a>
//
// The href is the only unambiguous part, so that is what we match. The page
// lists each address once per protocol it speaks, so the same ip:port appears
// under both #https and #socks5: those are two different proxies (dialling the
// wrong one is a guaranteed failure), so the dedupe key carries the protocol.
func extractProxydbNet(body []byte) sourceExtract {
	matches := proxydbHrefRe.FindAllStringSubmatch(string(body), -1)
	out := make([]*ProxyInfo, 0, len(matches))
	seen := make(map[string]bool, len(matches))
	var asOf time.Time
	for _, m := range matches {
		ip, port, proto := m[1], m[2], m[3]
		protocol := normalizeProxyProtocol(proto, "")
		key := protocol + "://" + net.JoinHostPort(ip, port)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, &ProxyInfo{
			URL:      proxyURLFor(protocol, ip, port),
			Protocol: protocol,
			IP:       ip,
			Port:     port,
		})
	}
	return sourceExtract{Proxies: out, AsOf: asOf}
}

// normalizeProxyProtocol collapses the spellings the sources use for a protocol
// into the two the pool knows about. An empty or unrecognised value becomes
// "https", the safe default (a CONNECT proxy that cannot serve https simply
// fails its attempt; a SOCKS5 proxy dialed as CONNECT fails at the transport).
func normalizeProxyProtocol(values ...string) string {
	for _, v := range values {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "socks4", "socks5", "socks5h":
			return "socks5"
		case "http", "https":
			return "https"
		}
	}
	return "https"
}

// proxyURLFor builds the Pool URL a transport can consume directly.
func proxyURLFor(protocol, ip, port string) string {
	if protocol == "socks5" {
		return "socks5://" + net.JoinHostPort(ip, port)
	}
	return "http://" + net.JoinHostPort(ip, port)
}

// jsonPortString reads a JSON value that may be a string ("80") or a number
// (80) and returns it as a decimal string, or "" when it is not a usable port.
// A JSON list is the one place a non-numeric or out-of-range port can reach the
// pool, because the regex-based extractors constrain the shape structurally;
// checking here keeps every extractor from having to repeat it.
func jsonPortString(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	if s[0] == '"' {
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return ""
		}
		s = strings.TrimSpace(v)
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 || n > 65535 {
		return ""
	}
	return strconv.FormatInt(n, 10)
}

// parseAnyTimestamp accepts the two spellings the sources report: RFC3339
// ("2026-10-19T01:06:10.861Z") and a unix epoch string (1792371970). Both
// appear in the live bodies. ok=false means neither parsed.
func parseAnyTimestamp(rfc3339, epoch string) (time.Time, bool) {
	if s := strings.TrimSpace(rfc3339); s != "" {
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05Z07:00"} {
			if t, err := time.Parse(layout, s); err == nil {
				return t, true
			}
		}
	}
	if s := strings.TrimSpace(epoch); s != "" {
		if secs, err := strconv.ParseInt(s, 10, 64); err == nil && secs > 0 {
			return time.Unix(secs, 0), true
		}
	}
	return time.Time{}, false
}
