package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// loadSpysFixture reads one captured spys.one body. The bodies live in
// testdata/proxies/ so a decode regression is pinned against the real bytes the
// site served on 2026-10-19, not a hand-simplified snippet.
func loadSpysFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "proxies", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return body
}

// spysTestRow renders one spys.one proxy row around the given IP and port terms
// so edge-case tests can build small bodies without a 45 KB fixture.
func spysTestRow(ip string, terms ...string) string {
	var b strings.Builder
	b.WriteString(`<tr><td><font class=spy14>`)
	b.WriteString(ip)
	b.WriteString(`<script type="text/javascript">document.write("<font class=spy2>:<\/font>"`)
	for _, term := range terms {
		b.WriteString("+(" + term + ")")
	}
	b.WriteString(`)</script></font></td></tr>`)
	return b.String()
}

func TestSpysOneFixture(t *testing.T) {
	// Each fixture carries exactly 30 proxy rows. That count was measured by
	// grepping the captured bodies for the IP-then-document.write row shape
	// (`grep -o 'document\.write(' | wc -l` -> 30 for both). The bodies also
	// embed other addresses (EndIP=... markers, non-row IPs), so the count is
	// the number of port-writing rows, not every dotted quad on the page.
	// The document.write cross-check below keeps this honest: a decode bug that
	// drops rows makes Proxies shorter than the number of write calls.
	tests := []struct {
		name      string
		file      string
		protocol  string
		wantCount int
		wantAsOf  time.Time
		wantPorts map[string]string
	}{
		{
			name:      "anonymous",
			file:      "spys-one-anonymous.html",
			protocol:  "https",
			wantCount: 30,
			wantAsOf:  time.Date(2026, time.October, 19, 6, 51, 0, 0, time.UTC),
			wantPorts: map[string]string{
				"112.169.99.71":   "8044",
				"202.169.229.139": "53281",
				"69.48.201.94":    "80",
				"136.113.23.93":   "443",
				"85.90.207.232":   "3128",
				"125.209.80.146":  "8080",
			},
		},
		{
			name:      "socks",
			file:      "spys-one-socks.html",
			protocol:  "socks5",
			wantCount: 30,
			wantAsOf:  time.Date(2026, time.October, 19, 5, 57, 0, 0, time.UTC),
			wantPorts: map[string]string{
				"78.109.137.53":  "1080",
				"93.184.8.22":    "1080",
				"85.121.183.248": "1089",
				"72.207.109.5":   "4145",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := loadSpysFixture(t, tt.file)
			res := extractorSpysOne(tt.protocol)(body)

			if len(res.Proxies) != tt.wantCount {
				t.Fatalf("got %d proxies, want %d", len(res.Proxies), tt.wantCount)
			}
			if writes := bytes.Count(body, []byte("document.write(")); len(res.Proxies) != writes {
				t.Errorf("extracted %d proxies but body has %d port-writing rows", len(res.Proxies), writes)
			}
			if !res.AsOf.Equal(tt.wantAsOf) {
				t.Errorf("AsOf = %s, want %s", res.AsOf.UTC(), tt.wantAsOf.UTC())
			}

			byIP := make(map[string]*ProxyInfo, len(res.Proxies))
			for _, p := range res.Proxies {
				byIP[p.IP] = p

				// The registry stamps SourceURL/SourceIndex after extraction;
				// the extractor must leave them unset.
				if p.SourceURL != "" || p.SourceIndex != 0 {
					t.Errorf("%s: extractor stamped SourceURL=%q SourceIndex=%d", p.IP, p.SourceURL, p.SourceIndex)
				}
				if p.Protocol != tt.protocol {
					t.Errorf("%s: protocol = %q, want %q", p.IP, p.Protocol, tt.protocol)
				}
				if p.URL != proxyURLFor(tt.protocol, p.IP, p.Port) {
					t.Errorf("%s: URL = %q, want %q", p.IP, p.URL, proxyURLFor(tt.protocol, p.IP, p.Port))
				}
				if len(p.Port) < 1 || len(p.Port) > 5 {
					t.Errorf("%s: port %q is not 1..5 digits", p.IP, p.Port)
				}
				if n, err := strconv.Atoi(p.Port); err != nil || n > 65535 {
					t.Errorf("%s: port %q does not fit a uint16", p.IP, p.Port)
				}
			}

			for ip, port := range tt.wantPorts {
				p, ok := byIP[ip]
				if !ok {
					t.Errorf("missing expected proxy %s", ip)
					continue
				}
				if p.Port != port {
					t.Errorf("%s: decoded port %q, want %q", ip, p.Port, port)
				}
			}
		})
	}
}

func TestSpysOneSkipsUndecodableRowsWithoutLosingOthers(t *testing.T) {
	// The middle row references `zz9`, which the table never defines. It must be
	// dropped while the row AFTER it still parses, proving one bad row does not
	// abort the scan. Table: k1=5000, k2=6000, d1=2^k1, d2=3^k2, so `(d1^k1)`
	// expands to (2^5000)^5000 = 2 and `(d2^k2)` to 3.
	body := []byte(`<script type="text/javascript">k1=5000;k2=6000;d1=2^k1;d2=3^k2;</script>` +
		spysTestRow("1.2.3.4", "d1^k1", "d2^k2") +
		spysTestRow("9.9.9.9", "d1^k1", "zz9^k2") +
		spysTestRow("5.6.7.8", "d1^k1", "d2^k2"))

	res := extractorSpysOne("https")(body)
	if len(res.Proxies) != 2 {
		t.Fatalf("got %d proxies, want 2 (bad row in the middle must not stop the scan)", len(res.Proxies))
	}
	got := map[string]string{}
	for _, p := range res.Proxies {
		got[p.IP] = p.Port
	}
	want := map[string]string{"1.2.3.4": "23", "5.6.7.8": "23"}
	for ip, port := range want {
		if got[ip] != port {
			t.Errorf("%s: port = %q, want %q", ip, got[ip], port)
		}
	}
	if _, ok := got["9.9.9.9"]; ok {
		t.Errorf("row with undefined variable zz9 should have been skipped")
	}
}

func TestSpysOneDeduplicatesIdenticalIPPort(t *testing.T) {
	body := []byte(`<script type="text/javascript">k1=5000;d1=7^k1;</script>` +
		spysTestRow("1.2.3.4", "d1^k1") +
		spysTestRow("1.2.3.4", "d1^k1"))

	res := extractorSpysOne("https")(body)
	if len(res.Proxies) != 1 {
		t.Fatalf("got %d proxies, want 1 after dedupe", len(res.Proxies))
	}
	if res.Proxies[0].Port != "7" {
		t.Errorf("port = %q, want 7", res.Proxies[0].Port)
	}
}

func TestSpysOneGarbageBodyIsSafe(t *testing.T) {
	binary := make([]byte, 2048)
	for i := range binary {
		binary[i] = 0xff
	}
	// A malformed date must be ignored (AsOf stays zero) rather than panicking
	// or rolling over into a neighbouring day/month.
	badDate := []byte(`<font class=spy14>31-feb-2026</font> 25:99 </font>`)

	bodies := map[string][]byte{
		"nil":              nil,
		"empty":            {},
		"plain text":       []byte("not html at all"),
		"truncated html":   []byte(`<html><body><table><tr><td>garbage`),
		"truncated row":    []byte(`1.2.3.4<script type="text/javascript">document.write(`),
		"repeated ip text": []byte(strings.Repeat("<td>1.2.3.4</td>", 100)),
		"binary":           binary,
		"bad date":         badDate,
	}

	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			res := extractorSpysOne("https")(body)
			if len(res.Proxies) != 0 {
				t.Errorf("got %d proxies from garbage, want 0", len(res.Proxies))
			}
			if !res.AsOf.IsZero() {
				t.Errorf("AsOf = %s, want zero", res.AsOf)
			}
		})
	}
}
