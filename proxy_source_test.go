package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fixtureFor maps a built-in source to the captured body used to test its
// extractor. The bodies are real responses, fetched live on 2026-10-19, because
// the entire class of bug these tests exist for was invisible to hand-written
// samples: the old parser was only ever exercised against the shapes its author
// imagined, and three of the six real sources produced zero proxies.
//
// wantCount is the number of proxies in the fixture, counted independently from
// the extractor (line counts for the text lists, JSON array length for geonode,
// the deduped href count for proxydb).
var sourceFixtures = map[string]struct {
	file string
	want int
}{
	"https://raw.githubusercontent.com/Thordata/awesome-free-proxy-list/refs/heads/main/proxies/top-trusted.txt":                                                      {file: "thordata-top-trusted.txt", want: 28},
	"https://raw.githubusercontent.com/iplocate/free-proxy-list/refs/heads/main/protocols/https.txt":                                                                  {file: "iplocate-free-proxy-list.txt", want: 39},
	"https://proxylist.geonode.com/api/proxy-list?anonymityLevel=anonymous&protocols=https%2Csocks5&speed=medium&page=1&limit=500&sort_by=responseTime&sort_type=asc": {file: "geonode-api.json", want: 12},
	"https://spys.one/en/anonymous-proxy-list/": {file: "spys-one-anonymous.html", want: 30},
	"https://spys.one/en/socks-proxy-list/":     {file: "spys-one-socks.html", want: 30},
	"https://proxydb.net/?anonlvl=4&country=&protocol=https&protocol=socks5&sort_column_id=checked&sort_order_desc=true": {file: "proxydb-net.html", want: 30},
}

func loadFixture(t *testing.T, file string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "proxies", file))
	if err != nil {
		t.Fatalf("read fixture %s: %v", file, err)
	}
	return body
}

// TestEveryBuiltinSourceHasAnExtractorAndIsTested guards the registry itself:
// a source added without a fixture would silently go untested, which is how the
// original parsers rotted.
func TestEveryBuiltinSourceHasAnExtractorAndIsTested(t *testing.T) {
	for _, src := range defaultProxySources {
		if src.Extract == nil {
			t.Errorf("source %q has no extractor", src.Name)
		}
		if src.Name == "" || src.URL == "" {
			t.Errorf("source has empty name/url: %+v", src)
		}
		if _, ok := sourceFixtures[src.URL]; !ok {
			t.Errorf("source %q (%s) has no fixture in sourceFixtures", src.Name, src.URL)
		}
	}
	if len(defaultProxySources) != len(sourceFixtures) {
		t.Errorf("%d sources but %d fixtures; they must correspond", len(defaultProxySources), len(sourceFixtures))
	}
}

// TestBuiltinExtractorsParseTheirRealFixtures is the regression net for the
// parser bugs. Each real body must yield exactly the number of proxies it
// contains: a source that silently yields zero is worse than one that errors,
// because the pool just gets smaller and nothing says why.
func TestBuiltinExtractorsParseTheirRealFixtures(t *testing.T) {
	total := 0
	for _, src := range defaultProxySources {
		fixture := sourceFixtures[src.URL]
		body := loadFixture(t, fixture.file)

		t.Run(src.Name, func(t *testing.T) {
			got := src.Extract(body)
			if len(got.Proxies) != fixture.want {
				t.Errorf("extracted %d proxies, want %d", len(got.Proxies), fixture.want)
			}
			for _, p := range got.Proxies {
				if p.IP == "" || p.Port == "" {
					t.Errorf("entry with no ip/port: %+v", p)
				}
				if p.Protocol != "https" && p.Protocol != "socks5" {
					t.Errorf("unexpected protocol %q for %s", p.Protocol, p.IP)
				}
				// The URL handed to TransportFor has to agree with Protocol,
				// or a SOCKS5 entry gets dialed as a CONNECT proxy.
				switch p.Protocol {
				case "socks5":
					if !strings.HasPrefix(p.URL, "socks5://") {
						t.Errorf("socks5 entry has URL %q", p.URL)
					}
				default:
					if !strings.HasPrefix(p.URL, "http://") {
						t.Errorf("https entry has URL %q", p.URL)
					}
				}
			}
			total += len(got.Proxies)
		})
	}
	if t.Failed() {
		return
	}
	// The whole point of the fix: six live sources contribute, not two.
	if total < 150 {
		t.Errorf("all sources together yielded only %d proxies; expected the full set (>150)", total)
	}
	t.Logf("all six sources yielded %d proxies", total)
}

// TestSocksSourceIsNotMislabeledAsHTTPS is the specific bug the geonode
// extractor had: the body says ["https"]/["socks5"] in an array named
// "protocols", but the parser read only the scalar "protocol"/"type" keys, so
// every entry silently became "https" and SOCKS5 proxies were handed to a
// CONNECT transport that can never work with them.
func TestSocksSourceIsNotMislabeledAsHTTPS(t *testing.T) {
	body := []byte(`{"data":[
		{"ip":"1.1.1.1","port":"8080","protocols":["https"],"country":"DE"},
		{"ip":"2.2.2.2","port":"1080","protocols":["socks5"],"country":"FR"},
		{"ip":"3.3.3.3","port":"1081","protocols":["socks5","https"],"country":"US"}
	]}`)

	got := extractGeonode(body)
	if len(got.Proxies) != 3 {
		t.Fatalf("got %d proxies, want 3", len(got.Proxies))
	}
	want := map[string]string{"1.1.1.1": "https", "2.2.2.2": "socks5", "3.3.3.3": "https"}
	for _, p := range got.Proxies {
		if p.Protocol != want[p.IP] {
			t.Errorf("%s: protocol = %q, want %q", p.IP, p.Protocol, want[p.IP])
		}
	}
}

// TestGeonodeAcceptsNumericAndStringPorts covers the other half of the same
// bug family: unmarshalling the port into a plain string field fails the whole
// object when the API sends a number, silently discarding every entry in the
// list. Both spellings must work.
func TestGeonodeAcceptsNumericAndStringPorts(t *testing.T) {
	body := []byte(`{"data":[
		{"ip":"1.1.1.1","port":"8080"},
		{"ip":"2.2.2.2","port":3128},
		{"ip":"3.3.3.3"}
	]}`)

	got := extractGeonode(body)
	if len(got.Proxies) != 2 {
		t.Fatalf("got %d proxies, want 2 (the entry with no port must be skipped)", len(got.Proxies))
	}
	ports := map[string]string{}
	for _, p := range got.Proxies {
		ports[p.IP] = p.Port
	}
	if ports["1.1.1.1"] != "8080" {
		t.Errorf("string port = %q, want 8080", ports["1.1.1.1"])
	}
	if ports["2.2.2.2"] != "3128" {
		t.Errorf("numeric port = %q, want 3128", ports["2.2.2.2"])
	}
}

// TestGeonodeReportsNewestEntryDate pins the freshness input: the source-level
// as-of is the newest per-entry timestamp, from either updated_at (RFC3339) or
// lastChecked (unix epoch).
func TestGeonodeReportsNewestEntryDate(t *testing.T) {
	body := []byte(`{"data":[
		{"ip":"1.1.1.1","port":"80","updated_at":"2026-10-19T01:06:10.861Z","lastChecked":100},
		{"ip":"2.2.2.2","port":"81","lastChecked":1792374798},
		{"ip":"3.3.3.3","port":"82","updated_at":"2020-01-01T00:00:00Z"}
	]}`)

	got := extractGeonode(body)
	if got.AsOf.IsZero() {
		t.Fatal("AsOf is zero, want the newest entry timestamp")
	}
	want := time.Unix(1792374798, 0)
	if !got.AsOf.Equal(want) {
		t.Errorf("AsOf = %s, want %s", got.AsOf.UTC(), want.UTC())
	}
}

// TestProxydbReadsProtocolFromHref: proxydb puts the protocol in the anchor
// fragment and hides the port behind a decoy, so the href is the only reliable
// source. Getting this wrong means a SOCKS5 entry is dialed as CONNECT.
func TestProxydbReadsProtocolFromHref(t *testing.T) {
	body := []byte(`<tr><td><a href="/1.2.3.4/8080#https" title="">1.2.3.4</a></td>
	<td><div style="display:none">12</div><a href="/1.2.3.4/8080#https" title="">8080</a></td></tr>
	<tr><td><a href="/5.6.7.8/1080#socks5" title="">5.6.7.8</a></td>
	<td><div style="display:none">99</div><a href="/5.6.7.8/1080#socks5" title="">1080</a></td></tr>`)

	got := extractProxydbNet(body)
	if len(got.Proxies) != 2 {
		t.Fatalf("got %d proxies, want 2 (each row appears twice in the anchors)", len(got.Proxies))
	}
	for _, p := range got.Proxies {
		switch p.IP {
		case "1.2.3.4":
			if p.Port != "8080" || p.Protocol != "https" {
				t.Errorf("1.2.3.4: got %s/%s, want 8080/https", p.Port, p.Protocol)
			}
		case "5.6.7.8":
			if p.Port != "1080" || p.Protocol != "socks5" {
				t.Errorf("5.6.7.8: got %s/%s, want 1080/socks5", p.Port, p.Protocol)
			}
		default:
			t.Errorf("unexpected IP %s", p.IP)
		}
	}
}

// TestStaleSourceIsDiscarded: a list whose newest entry is older than
// proxySourceMaxAge is a list of addresses that have probably gone dead, so the
// whole source is dropped instead of being probed one failed request at a time.
func TestStaleSourceIsDiscarded(t *testing.T) {
	fresh := time.Now().Add(-2 * time.Minute)
	stale := time.Now().Add(-proxySourceMaxAge - time.Minute)

	cases := []struct {
		name     string
		asOf     time.Time
		wantKept bool
	}{
		{"fresh contents are kept", fresh, true},
		{"stale contents are discarded", stale, false},
		{"unknown date is kept, not assumed stale", time.Time{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, "1.2.3.4:8080\n")
			}))
			defer srv.Close()

			svc := NewProxyService("", time.Minute)
			sources := []proxySource{{
				Name: "test",
				URL:  srv.URL,
				Extract: func([]byte) sourceExtract {
					return sourceExtract{
						Proxies: []*ProxyInfo{{IP: "1.2.3.4", Port: "8080", Protocol: "https", URL: "http://1.2.3.4:8080"}},
						AsOf:    tc.asOf,
					}
				},
			}}

			got := svc.fetchAll(sources)
			if tc.wantKept && len(got) != 1 {
				t.Errorf("source was discarded, want kept (%d proxies)", len(got))
			}
			if !tc.wantKept && len(got) != 0 {
				t.Errorf("stale source was kept, want discarded (%d proxies)", len(got))
			}
		})
	}
}

// TestFetchAllStampsSourceIdentity: extractors must not stamp SourceURL /
// SourceIndex themselves; the registry does it once so the two cannot disagree.
func TestFetchAllStampsSourceIdentity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ignored")
	}))
	defer srv.Close()

	svc := NewProxyService("", time.Minute)
	sources := []proxySource{{
		Name: "test",
		URL:  srv.URL,
		Extract: func([]byte) sourceExtract {
			return sourceExtract{Proxies: []*ProxyInfo{
				{IP: "1.2.3.4", Port: "8080", Protocol: "https", URL: "http://1.2.3.4:8080"},
			}}
		},
	}}

	got := svc.fetchAll(sources)
	if len(got) != 1 {
		t.Fatalf("got %d proxies, want 1", len(got))
	}
	if got[0].SourceURL != srv.URL {
		t.Errorf("SourceURL = %q, want %q", got[0].SourceURL, srv.URL)
	}
	if got[0].SourceIndex != 0 {
		t.Errorf("SourceIndex = %d, want 0", got[0].SourceIndex)
	}
}

// TestLoadSourcesKeepsDefaultsAndAddsOverrides: the sources file is additive.
// A missing or empty file must not shrink the pool, and an unknown URL must
// still be fetched (falling back to sniffing) rather than ignored.
func TestLoadSourcesKeepsDefaultsAndAddsOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxy_sources.txt")
	extra := "https://example.invalid/extra-list.txt"
	if err := os.WriteFile(path, []byte("# comment\n\n"+extra+"\n"), 0o600); err != nil {
		t.Fatalf("write sources file: %v", err)
	}

	svc := NewProxyService(path, time.Minute)
	got := svc.loadSources()

	if len(got) != len(defaultProxySources)+1 {
		t.Fatalf("got %d sources, want %d (defaults + 1 override)", len(got), len(defaultProxySources)+1)
	}
	for i, src := range defaultProxySources {
		if got[i].URL != src.URL {
			t.Errorf("default source %d = %q, want %q", i, got[i].URL, src.URL)
		}
	}
	last := got[len(got)-1]
	if last.URL != extra {
		t.Errorf("override source = %q, want %q", last.URL, extra)
	}
	if last.Extract == nil {
		t.Error("override source has no extractor; an unknown URL must still be parsed")
	}
}

// TestLoadSourcesWithoutFileUsesBuiltins: the built-in list is the normal case,
// and a missing file is not an error.
func TestLoadSourcesWithoutFileUsesBuiltins(t *testing.T) {
	svc := NewProxyService(filepath.Join(t.TempDir(), "does-not-exist.txt"), time.Minute)
	got := svc.loadSources()
	if len(got) != len(defaultProxySources) {
		t.Fatalf("got %d sources, want %d", len(got), len(defaultProxySources))
	}
}

// TestSourcesReferenceDocIsInSync keeps docs/proxy_sources.txt honest: it is
// the human-readable copy of the built-in list, so it must name the same URLs.
func TestSourcesReferenceDocIsInSync(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("docs", "proxy_sources.txt"))
	if err != nil {
		t.Fatalf("read reference doc: %v", err)
	}
	doc := map[string]bool{}
	for _, line := range strings.Split(string(body), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			doc[line] = true
		}
	}
	for _, url := range defaultProxySourceURLs() {
		if !doc[url] {
			t.Errorf("docs/proxy_sources.txt is missing built-in source %s", url)
		}
	}
	if len(doc) != len(defaultProxySources) {
		t.Errorf("reference doc lists %d sources, built-ins are %d", len(doc), len(defaultProxySources))
	}
}

// --- readiness ---

// TestReadinessExpiresAfterTTL: a readiness mark is only good for
// proxyReadyTTL. Free proxies die constantly, so a mark from minutes ago is not
// evidence that the proxy still answers.
func TestReadinessExpiresAfterTTL(t *testing.T) {
	svc := NewProxyService("", time.Minute)
	url := "http://1.2.3.4:8080"

	svc.markReady(url, 0)
	if !svc.isReady(url) {
		t.Fatal("just-marked proxy is not ready")
	}
	if got := svc.readyCount(); got != 1 {
		t.Fatalf("readyCount = %d, want 1", got)
	}

	// Backdate the mark beyond the TTL rather than sleeping a minute.
	svc.readyMu.Lock()
	svc.ready[url] = time.Now().Add(-proxyReadyTTL - time.Second)
	svc.readyMu.Unlock()

	if svc.isReady(url) {
		t.Error("ready mark outlived its TTL")
	}
	if got := svc.readyCount(); got != 0 {
		t.Errorf("readyCount = %d, want 0", got)
	}
}

// TestMarkDownWithdrawsReadiness: a proxy that just failed cannot still be
// counted as ready, or the ready count would claim capacity that is not there.
func TestMarkDownWithdrawsReadiness(t *testing.T) {
	svc := NewProxyService("", time.Minute)
	url := "http://1.2.3.4:8080"

	svc.markReady(url, 0)
	svc.MarkDown(url, time.Minute)

	if svc.isReady(url) {
		t.Error("proxy is still marked ready after being marked down")
	}
	if got := svc.readyCount(); got != 0 {
		t.Errorf("readyCount = %d, want 0", got)
	}
}

// TestPickPrefersAReadyProxy: readiness has to actually influence selection, or
// it is bookkeeping that changes nothing. When one pool member is proven and
// another is not, every pick must hand back the proven one.
func TestPickPrefersAReadyProxy(t *testing.T) {
	svc := NewProxyService("", time.Minute)
	ready := &ProxyInfo{URL: "http://10.0.0.1:8080", IP: "10.0.0.1", Port: "8080", Protocol: "https"}
	unknown := &ProxyInfo{URL: "http://10.0.0.2:8080", IP: "10.0.0.2", Port: "8080", Protocol: "https"}
	svc.pool.set([]*ProxyInfo{unknown, ready})
	svc.markReady(ready.URL, 0)

	for i := 0; i < 5; i++ {
		got := svc.pick()
		if got == nil {
			t.Fatal("pick returned nil with a live pool")
		}
		if got.URL != ready.URL {
			t.Fatalf("pick %d returned %s, want the ready proxy %s", i, got.URL, ready.URL)
		}
	}
}

// TestPickFallsBackWhenNothingIsReady: an unproven proxy still beats no proxy,
// so a cold pool (all TTLs lapsed, or the first request after startup) must
// still hand one back rather than silently going direct.
func TestPickFallsBackWhenNothingIsReady(t *testing.T) {
	svc := NewProxyService("", time.Minute)
	a := &ProxyInfo{URL: "http://10.0.0.1:8080", IP: "10.0.0.1", Port: "8080", Protocol: "https"}
	svc.pool.set([]*ProxyInfo{a})

	if got := svc.pick(); got == nil || got.URL != a.URL {
		t.Fatalf("pick = %v, want the only pool entry", got)
	}
}

// TestPickSkipsDownProxies: a proxy in cooldown must not be handed out even as
// the unproven fallback.
func TestPickSkipsDownProxies(t *testing.T) {
	svc := NewProxyService("", time.Minute)
	a := &ProxyInfo{URL: "http://10.0.0.1:8080", IP: "10.0.0.1", Port: "8080", Protocol: "https"}
	svc.pool.set([]*ProxyInfo{a})
	svc.MarkDown(a.URL, time.Minute)

	if got := svc.pick(); got != nil {
		t.Fatalf("pick = %v, want nil when every proxy is down", got)
	}
}

// TestPickPrefersLowLatency: among ready proxies, the one with the lowest
// measured dial latency wins, so low-latency proxies are used first.
func TestPickPrefersLowLatency(t *testing.T) {
	svc := NewProxyService("", time.Minute)
	fast := &ProxyInfo{URL: "http://10.0.0.1:8080", IP: "10.0.0.1", Port: "8080", Protocol: "https"}
	slow := &ProxyInfo{URL: "http://10.0.0.2:8080", IP: "10.0.0.2", Port: "8080", Protocol: "https"}
	svc.pool.set([]*ProxyInfo{slow, fast})
	svc.markReady(fast.URL, 0)
	svc.markReady(slow.URL, 0)

	// fast answers in 50ms, slow in 400ms (both well under the 3s probe timeout)
	svc.readyLatencyMu.Lock()
	svc.readyLatency[fast.URL] = 50 * time.Millisecond
	svc.readyLatency[slow.URL] = 400 * time.Millisecond
	svc.readyLatencyMu.Unlock()

	for i := 0; i < 5; i++ {
		got := svc.pick()
		if got == nil {
			t.Fatal("pick returned nil with a live pool")
		}
		if got.URL != fast.URL {
			t.Fatalf("pick %d returned %s (latency %v), want the low-latency proxy %s (latency 50ms)",
				i, got.URL, svc.readyLatency[got.URL], fast.URL)
		}
	}
}

// startListener returns a real listening TCP address plus a stop function, so
// the readiness probe is exercised against an actual socket rather than a stub.
func startListener(t *testing.T) (string, func()) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	return l.Addr().String(), func() { l.Close() }
}

// proxyInfoAt builds a ProxyInfo pointing at a real address.
func proxyInfoAt(t *testing.T, addr string) *ProxyInfo {
	t.Helper()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	return &ProxyInfo{
		IP: host, Port: port, Protocol: "https",
		URL: "http://" + net.JoinHostPort(host, port),
	}
}

// TestTopUpReadyFillsTheRequirement is the core of "so that in case a proxy is
// required it is ready available": with a requirement of 2 and a pool where
// only 2 members are reachable, the top-up pass must prove exactly those and
// leave the ready count at the requirement.
func TestTopUpReadyFillsTheRequirement(t *testing.T) {
	liveA, stopA := startListener(t)
	defer stopA()
	liveB, stopB := startListener(t)
	defer stopB()

	// A port that nothing is listening on: bind then release so the dial is
	// refused rather than hanging.
	dead, stopDead := startListener(t)
	stopDead()

	svc := NewProxyService("", time.Minute)
	a, b, d := proxyInfoAt(t, liveA), proxyInfoAt(t, liveB), proxyInfoAt(t, dead)
	svc.pool.set([]*ProxyInfo{a, b, d})
	svc.SetRequiredReady(2)

	svc.topUpReady(context.Background())

	if got := svc.ReadyCount(); got != 2 {
		t.Errorf("ready count = %d, want 2", got)
	}
	if !svc.isReady(a.URL) || !svc.isReady(b.URL) {
		t.Error("the two reachable proxies were not marked ready")
	}
	if svc.isReady(d.URL) {
		t.Error("an unreachable proxy was marked ready")
	}
}

// TestTopUpReadyMarksUnreachableProxiesDown: a probe that fails is a transport
// failure, so it is charged to the proxy (cooldown), not silently ignored —
// otherwise every top-up pass would re-probe the same dead hosts forever.
func TestTopUpReadyMarksUnreachableProxiesDown(t *testing.T) {
	dead, stop := startListener(t)
	stop()

	svc := NewProxyService("", time.Minute)
	d := proxyInfoAt(t, dead)
	svc.pool.set([]*ProxyInfo{d})
	svc.SetRequiredReady(1)

	svc.topUpReady(context.Background())

	if svc.isReady(d.URL) {
		t.Error("an unreachable proxy was marked ready")
	}
	if !svc.isDown(d.URL) {
		t.Error("an unreachable proxy was not put into cooldown")
	}
}

// TestTopUpReadyIsNoOpWithoutARequirement: with no provider asking for proxies
// the pass must do no work, so an unconfigured gateway does not dial anything.
func TestTopUpReadyIsNoOpWithoutARequirement(t *testing.T) {
	live, stop := startListener(t)
	defer stop()

	svc := NewProxyService("", time.Minute)
	p := proxyInfoAt(t, live)
	svc.pool.set([]*ProxyInfo{p})
	svc.SetRequiredReady(0)

	svc.topUpReady(context.Background())

	if got := svc.ReadyCount(); got != 0 {
		t.Errorf("ready count = %d, want 0 (nothing should have been probed)", got)
	}
}

// TestTopUpReadyStopsAtTheRequirement keeps the pass cheap: once the ready pool
// is big enough it must stop probing, not walk the rest of the pool.
func TestTopUpReadyStopsAtTheRequirement(t *testing.T) {
	var addrs []string
	for i := 0; i < 5; i++ {
		addr, stop := startListener(t)
		defer stop()
		addrs = append(addrs, addr)
	}

	svc := NewProxyService("", time.Minute)
	var list []*ProxyInfo
	for _, a := range addrs {
		list = append(list, proxyInfoAt(t, a))
	}
	svc.pool.set(list)
	svc.SetRequiredReady(1)

	svc.topUpReady(context.Background())

	if got := svc.ReadyCount(); got != 1 {
		t.Errorf("ready count = %d, want exactly 1 (probing must stop at the requirement)", got)
	}
}

// --- requirement derivation ---

// TestRequiredProxyTriesSumsProvidersAndEndpoints pins the user-facing rule:
// two providers configured with proxy: 3 means at least six proxies must be
// ready at once, because requests for either provider may each spend a budget
// of three distinct proxies.
func TestRequiredProxyTriesSumsProvidersAndEndpoints(t *testing.T) {
	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"a":       {Proxy: 3},
			"b":       {Proxy: 3},
			"direct":  {Proxy: 0},
			"negativ": {Proxy: -1},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{
				{Provider: "a", Model: "m1"},
				{Provider: "b", Model: "m2", Proxy: 2},
			}},
		},
	}

	p := NewProxy(cfg)
	if got := p.requiredProxyTries(); got != 8 {
		t.Errorf("requiredProxyTries = %d, want 8 (3+3 providers + 2 endpoint override)", got)
	}
}

// TestRequiredProxyTriesWithNoProxiesIsZero: a config that never uses a proxy
// must not demand a ready pool.
func TestRequiredProxyTriesWithNoProxiesIsZero(t *testing.T) {
	cfg := &Config{
		Providers: map[string]ProviderConfig{"a": {Proxy: 0}},
		Models:    map[string]ModelConfig{"smart": {Chain: []ModelEndpoint{{Provider: "a", Model: "m1"}}}},
	}
	if got := NewProxy(cfg).requiredProxyTries(); got != 0 {
		t.Errorf("requiredProxyTries = %d, want 0", got)
	}
}

// TestUpdateRequiredReadyPushesIntoTheService is the wiring check: computing the
// requirement is useless if it never reaches the service that enforces it, and
// a nil service must not panic (tests build bare Proxies).
func TestUpdateRequiredReadyPushesIntoTheService(t *testing.T) {
	cfg := &Config{
		Providers: map[string]ProviderConfig{"a": {Proxy: 3}, "b": {Proxy: 3}},
		Models:    map[string]ModelConfig{"smart": {Chain: []ModelEndpoint{{Provider: "a", Model: "m1"}}}},
	}
	p := NewProxy(cfg)

	// No service yet: must be a no-op, not a panic.
	p.UpdateRequiredReady()

	svc := NewProxyService("", time.Minute)
	p.SetProxyService(svc)
	p.UpdateRequiredReady()

	if got := svc.RequiredReady(); got != 6 {
		t.Errorf("service requirement = %d, want 6", got)
	}
}

// TestConfigReloadUpdatesTheRequirement: the requirement follows the config, so
// enabling a proxy on a provider at runtime must raise it without a restart.
func TestConfigReloadUpdatesTheRequirement(t *testing.T) {
	before := &Config{
		Providers: map[string]ProviderConfig{"a": {Proxy: 3}},
		Models:    map[string]ModelConfig{"smart": {Chain: []ModelEndpoint{{Provider: "a", Model: "m1"}}}},
	}
	router := NewRouter(before, "")
	proxy := NewProxy(before)
	svc := NewProxyService("", time.Minute)
	proxy.SetProxyService(svc)
	proxy.UpdateRequiredReady()

	if got := svc.RequiredReady(); got != 3 {
		t.Fatalf("initial requirement = %d, want 3", got)
	}

	after := &Config{
		Providers: map[string]ProviderConfig{"a": {Proxy: 3}, "b": {Proxy: 4}},
		Models:    map[string]ModelConfig{"smart": {Chain: []ModelEndpoint{{Provider: "a", Model: "m1"}}}},
	}
	g := NewGatewayContext(router, proxy, after, "", "", true)
	g.ReloadConfig(after)

	if got := svc.RequiredReady(); got != 7 {
		t.Errorf("requirement after reload = %d, want 7", got)
	}
}

// TestParseAnyTimestamp covers both spellings the live sources use, and the
// garbage case, since these pages are third-party input.
func TestParseAnyTimestamp(t *testing.T) {
	if got, ok := parseAnyTimestamp("2026-10-19T01:06:10.861Z", ""); !ok || !got.Equal(time.Date(2026, 10, 19, 1, 6, 10, 861000000, time.UTC)) {
		t.Errorf("rfc3339: got %v ok=%v", got, ok)
	}
	if got, ok := parseAnyTimestamp("", "1792374798"); !ok || got.Unix() != 1792374798 {
		t.Errorf("epoch: got %v ok=%v", got, ok)
	}
	for _, bad := range []struct{ a, b string }{{"", ""}, {"not-a-date", ""}, {"", "-5"}, {"2026-13-45T99:99:99Z", "abc"}} {
		if got, ok := parseAnyTimestamp(bad.a, bad.b); ok {
			t.Errorf("parseAnyTimestamp(%q,%q) = %v, want not-ok", bad.a, bad.b, got)
		}
	}
}

// TestJsonPortString covers the numeric-port coercion, including the values
// that must NOT be accepted as a port: a JSON list is the only place a
// non-numeric or out-of-range port could reach the pool, and an unusable entry
// must be skipped rather than turned into a proxy that can never connect.
func TestJsonPortString(t *testing.T) {
	cases := []struct{ in, want string }{
		{`"8080"`, "8080"},
		{`8080`, "8080"},
		{` 3128 `, "3128"},
		{`null`, ""},
		{`""`, ""},
		{`"abc"`, ""},
		{`{}`, ""},
		{`0`, ""},
		{`-1`, ""},
		{`65536`, ""},
		{`65535`, "65535"},
		{`"1080.5"`, ""},
	}
	for _, tc := range cases {
		if got := jsonPortString([]byte(tc.in)); got != tc.want {
			t.Errorf("jsonPortString(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// relayProxy is a real HTTP proxy in the client's proxy settings: it takes a
// request in absolute-form, replays it at the origin in origin-form, and relays
// the response back. It counts the requests it carries so a test can prove the
// traffic actually went through it rather than bypassing it.
//
// Go only builds a CONNECT tunnel for https targets. For an http:// origin it
// relays the request to the proxy in absolute-form, so this is the shape a test
// gets without needing TLS between two local servers.
type relayProxy struct {
	srv *httptest.Server
	// hits counts proxied requests successfully carried.
	hits atomic.Int32
	// failed counts attempts the relay could not complete, which a test can
	// use to make sure a proxy was offered the request and botched it.
	failed atomic.Int32
}

func newRelayProxy(t *testing.T) *relayProxy {
	t.Helper()
	rp := &relayProxy{}
	rp.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Counted on receipt, not after the tunnel copy: the caller only
		// reads the status line and rarely drains the body, so a
		// completion-time count races with a goroutine that has not
		// finished and silently reports 0 on a request that truly went
		// through the proxy.
		rp.hits.Add(1)
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("relay needs a hijackable connection")
			return
		}
		clientConn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("relay: hijack: %v", err)
			return
		}
		defer clientConn.Close()

		reqBody, err := io.ReadAll(r.Body)
		if err != nil {
			rp.failed.Add(1)
			t.Errorf("relay: read request body: %v", err)
			return
		}
		backConn, err := net.DialTimeout("tcp", r.Host, 5*time.Second)
		if err != nil {
			rp.failed.Add(1)
			t.Errorf("relay: dial origin %s: %v", r.Host, err)
			return
		}
		defer backConn.Close()

		// The origin needs origin-form plus a restored Content-Length, or it
		// blocks waiting for a body that never arrives.
		if _, err := fmt.Fprintf(backConn, "%s %s HTTP/1.1\r\nHost: %s\r\nContent-Length: %d\r\nContent-Type: %s\r\n\r\n",
			r.Method, r.URL.RequestURI(), r.Host, len(reqBody), r.Header.Get("Content-Type")); err != nil {
			rp.failed.Add(1)
			t.Errorf("relay: write request line: %v", err)
			return
		}
		if _, err := backConn.Write(reqBody); err != nil {
			rp.failed.Add(1)
			t.Errorf("relay: forward body: %v", err)
			return
		}

		done := make(chan struct{})
		go func() {
			io.Copy(clientConn, backConn)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			rp.failed.Add(1)
			t.Error("relay: response copy timed out")
			return
		}
		rp.hits.Add(1)
	}))
	t.Cleanup(rp.srv.Close)
	return rp
}

// TestAPoolProxyIsActuallyUsed closes the gap that made the whole readiness
// mechanism unverifiable: nothing tested that a request really dials through a
// pool proxy. The existing TestProviderProxyForward builds no proxy service at
// all, so it passes identically whether the proxy path works or is missing.
//
// This asserts the whole chain: the provider's proxy budget makes the attempt
// proxied, pick() returns a pool proxy, TransportFor wires it into the client,
// the request arrives at the origin only via the relay, and a success through
// it renews the readiness mark.
func TestAPoolProxyIsActuallyUsed(t *testing.T) {
	var gotBody string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"x","object":"chat.completion","created":1,"model":"gpt-4",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"via-proxy"}}]}`)
	}))
	defer backend.Close()

	relay := newRelayProxy(t)

	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"test": {URL: backend.URL, APIKeyEnv: "TEST_API_KEY", Proxy: 2},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{{Provider: "test", Model: "gpt-4"}}},
		},
	}
	t.Setenv("TEST_API_KEY", "test-key-123")

	p := NewProxy(cfg)
	svc := NewProxyService("", time.Minute)
	// The relay is a plain HTTP CONNECT endpoint, so the pool entry must say so:
	// the protocol is what chooses the tunnel shape (CONNECT vs SOCKS5), and an
	// https URL against a plain HTTP relay fails the handshake and silently
	// sends the request direct -- which would make this test prove nothing.
	svc.pool.set([]*ProxyInfo{{URL: relay.srv.URL, IP: "127.0.0.1", Port: "0", Protocol: "https"}})
	p.SetProxyService(svc)
	svc.SetRequiredReady(1)

	router := NewRouter(cfg, "")
	ep, wait := router.SelectEndpoint("smart", "proxy-used", false, nil)
	if ep == nil {
		t.Fatalf("no endpoint (wait %v)", wait)
	}

	body := `{"model":"smart","messages":[{"role":"user","content":"hi"}]}`
	resp, err := p.Forward(context.Background(), []byte(body), *ep, "")
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// The traffic must have gone through the relay. If it did not, the proxy
	// path is silently unused and every readiness test is proved on nothing.
	if got := relay.hits.Load(); got != 1 {
		t.Errorf("relay carried %d requests, want 1 (the request must go through the pool proxy)", got)
	}
	if got := relay.failed.Load(); got != 0 {
		t.Errorf("relay failed %d times, want 0", got)
	}
	if !strings.Contains(gotBody, "gpt-4") {
		t.Errorf("origin did not receive the proxied body: %q", gotBody)
	}
	// A success through the proxy must renew its readiness mark, because that
	// is the only high-quality evidence of usability the system ever gets.
	if !svc.isReady(relay.srv.URL) {
		t.Error("a request that succeeded through a proxy did not mark it ready")
	}
	// ...and the endpoint must be untouched: the proxy carried the traffic.
	for _, cd := range router.GetAllCooldowns() {
		t.Errorf("endpoint penalised for a proxied success: %+v", cd)
	}
}

// TestPoolProxyFailureChargesTheProxyNotTheEndpoint is the attribution half of
// the same setup with a relay that cannot complete: the request must fall back,
// the proxy must be marked down, and the endpoint must not be cooled down.
func TestPoolProxyFailureChargesTheProxyNotTheEndpoint(t *testing.T) {
	// A port that nothing is listening on: bind it, release it, so the dial is
	// refused rather than hanging.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadURL := "http://" + l.Addr().String()
	l.Close()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"x","object":"chat.completion","created":1,"model":"gpt-4",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"direct"}}]}`)
	}))
	defer backend.Close()

	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"test": {URL: backend.URL, APIKeyEnv: "TEST_API_KEY", Proxy: 2},
		},
		Models: map[string]ModelConfig{
			"smart": {Chain: []ModelEndpoint{{Provider: "test", Model: "gpt-4"}}},
		},
	}
	t.Setenv("TEST_API_KEY", "test-key-123")

	deadPort := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	deadInfo := &ProxyInfo{URL: deadURL, IP: "127.0.0.1", Port: deadPort, Protocol: "https"}

	p := NewProxy(cfg)
	svc := NewProxyService("", time.Minute)
	svc.pool.set([]*ProxyInfo{deadInfo})
	p.SetProxyService(svc)
	svc.SetRequiredReady(1)

	router := NewRouter(cfg, "")
	t.Cleanup(router.Close)
	gw := NewGatewayContext(router, p, cfg, "", "test-gw-key")

	body := `{"model":"smart","messages":[{"role":"user","content":"hi"}]}`

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer test-gw-key")
	gw.HandleChatCompletions(rec, req)

	// The proxy is dead, so pick() returns nil on the retry and the attempt
	// proceeds direct; the request must still succeed end to end.
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	if !svc.isDown(deadURL) {
		t.Error("the dead proxy was not marked down")
	}
	if svc.isReady(deadURL) {
		t.Error("the dead proxy was marked ready")
	}
	for _, cd := range router.GetAllCooldowns() {
		t.Errorf("endpoint penalised for a proxy failure: %+v", cd)
	}
}
