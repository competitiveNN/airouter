package main

// Regression tests for the OpenCode client-attribution headers.
//
// WHY THIS FILE IS SEPARATE FROM main_test.go
//
// The live harness (scripts/opencode-routing-check.sh) cannot cover this. It
// runs against `opencode/space-bunny-free`, and that model answers HTTP 200
// with OR without the attribution headers -- verified 2026-09-30 against
// opencode.ai/zen/v1. So a live green tells you the route works, and nothing
// about whether the headers were emitted at all. Only an assertion on the
// emitted header map can fail here, which is what these tests do.
//
// WHY IT MATTS AT ALL
//
// The other OpenCode free models answer
// `403 FreeTierError: OpenCode's free tier can only be used from within
// OpenCode` (oh-my-pi#12306), so dropping or corrupting these headers takes
// out a whole provider class. That failure is invisible to a status-code
// check on the one model that happens to be entitled, and invisible to a
// test that only asserts a header is *present* -- a wrong value, a stale
// constant session id, or headers leaking onto every provider would all pass.
// So: exact key set, exact static values, format of the generated ids,
// freshness across calls, and absence everywhere else.

import (
	"context"
	"regexp"
	"strings"
	"testing"
)

// opencodeHeaderKeys is the exact set of headers buildRequest must synthesize
// for an OpenCode provider, spelled the way opencodeRequestHeaders authors
// them. Compared as a set (not a count) so a header being added or dropped is
// named in the failure.
//
// These names are all lower case and compared case-insensitively (see
// assertOpencodeHeaderMap), because net/http canonicalizes header names on the
// way onto the wire: "x-opencode-client" is stored as "X-Opencode-Client". A
// test that indexed req.Header with mixed-case literals would report every
// attribution header as missing while the request was in fact correct.
var opencodeHeaderKeys = []string{
	"user-agent",
	"x-opencode-client",
	"x-opencode-project",
	"x-opencode-request",
	"x-opencode-session",
}

// attributionProviders are the provider names in opencodeProviderPrefixes.
// Every one of them must receive the headers.
var attributionProviders = []string{"opencode", "opencode-go", "opencode-zen"}

// nonAttributionProviders must receive none of them. "opencode-router" and
// "my-opencode" are here on purpose: isOpencodeProvider matches EXACTLY, so
// these two are NOT OpenCode gateways. The variable is called
// opencodeProviderPrefixes but the comparison is `p == provider`, so a new
// near-miss provider name has to be added to that list deliberately rather
// than being swept up by a substring match. If someone changes the match to
// a real prefix match, this test fails and says why.
var nonAttributionProviders = []string{
	"nvidia",
	"kilocode",
	"openai",
	"opencode-router",
	"my-opencode",
	"opencode2",
}

var (
	// sesIDPattern is the canonical OpenCode session shape, `ses_<12 hex><14 base62>`.
	//
	// This is load-bearing, not cosmetic: measured live against
	// opencode.ai/zen/v1 on 2026-10-01, a session id in any other shape --
	// including the `ses_`+32hex this file used to accept, and the `ctx:<hex>`
	// airouter uses internally -- is answered with `403 FreeTierError` even when
	// every other free-tier condition holds. A green test against the old
	// pattern meant the request would have been rejected in production.
	sesIDPattern = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
	msgIDPattern = regexp.MustCompile(`^msg_[0-9a-f]{32}$`)
)

// lowerKeys canonicalizes a header map's keys to lower case so a map authored
// with "x-opencode-client" can be compared against a map net/http stored as
// "X-Opencode-Client". Values are left untouched; the last write wins, which is
// what http.Header.Get does for a duplicated name.
func lowerKeys(h map[string]string) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[strings.ToLower(k)] = v
	}
	return out
}

// assertOpencodeHeaderMap checks the map opencodeRequestHeaders returns against
// the full contract: exactly the five attribution headers, no more, no fewer,
// each with the right value. This is the exact-set check, so it is only valid
// for that map -- a built request legitimately carries Content-Type and
// Authorization as well, which is why assertAttributionPresent exists.
func assertOpencodeHeaderMap(t *testing.T, where string, h map[string]string) {
	t.Helper()
	h = lowerKeys(h)

	if len(h) != len(opencodeHeaderKeys) {
		t.Errorf("%s: got %d headers, want exactly %d (%v)", where, len(h), len(opencodeHeaderKeys), h)
	}
	for k := range h {
		found := false
		for _, want := range opencodeHeaderKeys {
			if k == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s: unexpected extra header %q=%q", where, k, h[k])
		}
	}
	assertAttributionPresent(t, where, h)
}

// assertAttributionPresent checks that a header map carries all five
// attribution headers with correct values, without constraining the rest of the
// map. Used for the outgoing request, which also carries Content-Type and
// Authorization, and for a map whose session header has been deliberately
// overridden to a non-generated value.
func assertAttributionPresent(t *testing.T, where string, h map[string]string) {
	t.Helper()
	h = lowerKeys(h)

	for _, k := range opencodeHeaderKeys {
		if _, ok := h[k]; !ok {
			t.Errorf("%s: missing required header %q", where, k)
		}
	}

	// The static values are load-bearing: the gateway identifies the client
	// by them. A blank or a placeholder would look present to a presence-only
	// assertion while being useless upstream.
	if got, want := h["x-opencode-client"], "cli"; got != want {
		t.Errorf("%s: x-opencode-client = %q, want %q", where, got, want)
	}
	if got, want := h["x-opencode-project"], "default"; got != want {
		t.Errorf("%s: x-opencode-project = %q, want %q", where, got, want)
	}
	if got := h["user-agent"]; got == "" {
		t.Errorf("%s: User-Agent is empty; the free tier rejects anonymous clients", where)
	} else if !strings.HasPrefix(got, "opencode/") {
		t.Errorf("%s: User-Agent = %q, want an opencode/... CLI identity", where, got)
	} else if !strings.HasSuffix(got, "/cli") {
		t.Errorf("%s: User-Agent = %q, want a .../cli suffix", where, got)
	}

	// The two generated ids must be well-formed AND distinct from each other.
	// Sharing a value would collapse every request into one upstream identity,
	// which is the fingerprinting the per-call generation exists to prevent.
	// The session id is always the canonical form, whether generated or derived
	// from the gateway session, so there is no exemption to carve out here any
	// more: buildRequest maps the `ctx:<hex>` gateway id through
	// opencodeSessionFor because the gateway rejects that shape outright.
	if got := h["x-opencode-session"]; got != "" {
		if !sesIDPattern.MatchString(got) {
			t.Errorf("%s: x-opencode-session = %q, want ses_<12hex><14base62>", where, got)
		}
		if h["x-opencode-session"] == h["x-opencode-request"] {
			t.Errorf("%s: session and request ids are identical (%q); they must be independent",
				where, h["x-opencode-session"])
		}
	}
	if got := h["x-opencode-request"]; !msgIDPattern.MatchString(got) {
		t.Errorf("%s: x-opencode-request = %q, want msg_ + 32 hex chars", where, got)
	}
}

// TestOpencodeRequestHeadersExactMap pins the map opencodeRequestHeaders
// itself returns, independent of any HTTP plumbing.
func TestOpencodeRequestHeadersExactMap(t *testing.T) {
	assertOpencodeHeaderMap(t, "opencodeRequestHeaders()", opencodeRequestHeaders())
}

// TestOpencodeRequestHeadersFreshPerCall proves the ids are generated per call
// rather than computed once from a package-level variable. A shared, cached map
// would make every request look like one client.
func TestOpencodeRequestHeadersFreshPerCall(t *testing.T) {
	const calls = 8
	seenSession := make(map[string]int, calls)
	seenRequest := make(map[string]int, calls)

	for i := 0; i < calls; i++ {
		h := opencodeRequestHeaders()
		assertOpencodeHeaderMap(t, "call", h)
		seenSession[h["x-opencode-session"]]++
		seenRequest[h["x-opencode-request"]]++
	}

	if len(seenSession) != calls {
		t.Errorf("x-opencode-session repeated across %d calls: %d distinct values, want %d",
			calls, len(seenSession), calls)
	}
	if len(seenRequest) != calls {
		t.Errorf("x-opencode-request repeated across %d calls: %d distinct values, want %d",
			calls, len(seenRequest), calls)
	}
}

// TestOpencodeAttributionHeadersReachTheWire is the assertion the live harness
// cannot make: what actually lands on the outgoing request, per provider.
func TestOpencodeAttributionHeadersReachTheWire(t *testing.T) {
	ctx := context.Background()
	body := []byte(`{"model":"smart","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`)

	build := func(t *testing.T, provider string, extra map[string]string, sessionID string) map[string]string {
		t.Helper()
		pc := ProviderConfig{
			URL:       "https://opencode.ai/zen/v1",
			APIKeyEnv: "OPENCODE_API_KEY",
			Headers:   extra,
		}
		// Give the key a real value so the Authorization header is present too;
		// it is not what is under test but its absence would change nothing.
		t.Setenv("OPENCODE_API_KEY", "test-key")
		proxy := NewProxy(&Config{Providers: map[string]ProviderConfig{"p": pc}})
		req, err := proxy.buildRequest(ctx, body,
			ModelEndpoint{Provider: provider, Model: "space-bunny-free"},
			&pc, false, sessionID)
		if err != nil {
			t.Fatalf("%s: buildRequest: %v", provider, err)
		}
		out := make(map[string]string, len(req.Header))
		for k := range req.Header {
			out[k] = req.Header.Get(k)
		}
		// Lower-cased so the assertions below can index it with the same
		// literals used everywhere else in this file.
		return lowerKeys(out)
	}

	t.Run("opencode providers get the full header set", func(t *testing.T) {
		for _, provider := range attributionProviders {
			t.Run(provider, func(t *testing.T) {
				h := build(t, provider, nil, "")
				// The session header is overridden below when a session id is
				// supplied; with none supplied it is the generated one.
				assertAttributionPresent(t, provider, h)
			})
		}
	})

	t.Run("non-opencode providers get none of them", func(t *testing.T) {
		for _, provider := range nonAttributionProviders {
			t.Run(provider, func(t *testing.T) {
				h := build(t, provider, nil, "")
				for _, k := range opencodeHeaderKeys {
					if v, ok := h[k]; ok {
						t.Errorf("%s: leaked %s: %q to a non-opencode provider", provider, k, v)
					}
				}
				// A bare User-Agent is set by net/http's default transport
				// path, so only the x-opencode-* family and an explicit
				// opencode/ identity are meaningful leaks here.
				if ua := h["User-Agent"]; strings.HasPrefix(ua, "opencode/") {
					t.Errorf("%s: leaked opencode User-Agent %q to a non-opencode provider", provider, ua)
				}
			})
		}
	})

	t.Run("session id is pinned to the gateway session", func(t *testing.T) {
		// Sticky routing is only cache-effective upstream if the same session
		// presents the same id on every request. The gateway id itself is
		// `ctx:<hex>`, which the gateway answers with 403, so it is mapped onto
		// the canonical form rather than forwarded.
		const sid = "ctx:abcdef0123456789"
		h := build(t, "opencode", nil, sid)
		want := opencodeSessionFor(sid)
		if got := h["x-opencode-session"]; got != want {
			t.Errorf("x-opencode-session = %q, want %q", got, want)
		}
		if h["x-opencode-session"] == sid {
			t.Errorf("x-opencode-session = %q, want the ctx: id mapped, not forwarded raw",
				h["x-opencode-session"])
		}
		// Still well-formed for the request id, which is always generated.
		if got := h["x-opencode-request"]; !msgIDPattern.MatchString(got) {
			t.Errorf("x-opencode-request = %q, want msg_ + 32 hex chars", got)
		}
		assertAttributionPresent(t, "pinned session", h)
	})

	t.Run("same gateway session always maps to the same opencode session", func(t *testing.T) {
		// The whole point of pinning is prompt-cache affinity, which breaks if
		// the id is re-minted per request.
		const sid = "ctx:deadbeefcafebabe"
		first := build(t, "opencode", nil, sid)["x-opencode-session"]
		for i := 0; i < 5; i++ {
			if got := build(t, "opencode", nil, sid)["x-opencode-session"]; got != first {
				t.Fatalf("call %d: x-opencode-session = %q, want the stable %q", i, got, first)
			}
		}
		other := build(t, "opencode", nil, "ctx:0000000000000000")["x-opencode-session"]
		if other == first {
			t.Errorf("two different gateway sessions both mapped to %q", first)
		}
	})

	t.Run("config headers override the synthesized ones", func(t *testing.T) {
		// Documented precedence in proxy.go: an explicit provider `headers:`
		// entry is the operator's escape hatch and must win.
		h := build(t, "opencode", map[string]string{
			"x-opencode-client": "custom-client",
			"X-Custom":          "yes",
		}, "")
		if got := h["x-opencode-client"]; got != "custom-client" {
			t.Errorf("x-opencode-client = %q, want the config override %q", got, "custom-client")
		}
		if got := h["x-custom"]; got != "yes" {
			t.Errorf("X-Custom = %q, want %q from the config headers", got, "yes")
		}
	})
}

// TestOpencodeSessionIDShape pins the generated and derived session ids to the
// one shape the gateway accepts.
//
// This is the single highest-impact assertion in the file. Measured live
// against opencode.ai/zen/v1 on 2026-10-01: a session id in any other shape is
// answered `403 FreeTierError` even when every other free-tier condition is met.
// The two shapes this code previously emitted -- `ses_`+32hex from
// opencodeRequestHeaders, and the raw `ctx:<hex>` that buildRequest forwarded --
// were both measured 403, so both were production outages that the old
// `^ses_[0-9a-f]{32}$` pattern happily passed.
func TestOpencodeSessionIDShape(t *testing.T) {
	t.Run("generated ids are canonical and unique", func(t *testing.T) {
		seen := make(map[string]bool, 64)
		for i := 0; i < 64; i++ {
			id := opencodeSessionID()
			if !sesIDPattern.MatchString(id) {
				t.Fatalf("opencodeSessionID() = %q, want ses_<12hex><14base62>", id)
			}
			if seen[id] {
				t.Fatalf("opencodeSessionID() repeated %q", id)
			}
			seen[id] = true
		}
	})

	t.Run("derived ids are canonical, deterministic and collision-free", func(t *testing.T) {
		for _, sid := range []string{
			"ctx:abcdef0123456789",
			"ctx:",
			"",
			"not-a-session",
		} {
			got := opencodeSessionFor(sid)
			if !sesIDPattern.MatchString(got) {
				t.Errorf("opencodeSessionFor(%q) = %q, want ses_<12hex><14base62>", sid, got)
			}
			if again := opencodeSessionFor(sid); again != got {
				t.Errorf("opencodeSessionFor(%q) is not deterministic: %q then %q", sid, got, again)
			}
			// The whole point of deriving rather than forwarding: the gateway id
			// must not reach the gateway, because it is a rejected shape.
			if got == sid {
				t.Errorf("opencodeSessionFor(%q) returned the input unchanged", sid)
			}
		}
		if a, b := opencodeSessionFor("ctx:a"), opencodeSessionFor("ctx:b"); a == b {
			t.Errorf("distinct gateway sessions collided on %q", a)
		}
	})
}
