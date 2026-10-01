package main

// Tests for how a failure with NO HTTP status is classified for cooldowns.
//
// WHY THIS FILE EXISTS
//
// Reported on 2026-10-01: commandcode:stealth/space-bunny-alpha carried
//
//     status_code: 0
//     error_count: 3
//     last_error: "SSE error event: {\"message\":\"JSON error injected into SSE stream\", ...}"
//     expiry: 24 hours later
//
// A 24-hour ban, from an error the upstream explicitly injected as a fault. Two
// compounding causes, both fixed in router.go / config.go:
//
//  1. ApplyCooldownFromErrorForSession left statusCode at 0 for any error that is
//     not a *ProviderError, and cooldownForError's transient branch only matched
//     429 and 5xx. Zero therefore fell through to the "permanent-looking" soft
//     ban: 3 occurrences -> 24h, 10 -> 7 days. Plain `net/http: timeout awaiting
//     response headers` -- the most transient failure there is -- was serving a
//     week-long ban. Live: 15 of 23 cooldown entries carried status_code 0.
//
//  2. The /v1/responses path already wrapped an upstream SSE error event in a
//     ProviderError{502} via statusForStreamError; the chat path did not. The
//     same upstream fault was therefore a bounded 30s->30min on one endpoint and
//     an unbounded 24h on the other, purely by which API surface the client used.
//
// The theme: "I don't know the status" was treated as "this is probably
// permanent". It is the opposite. Every error without a status is a transport
// error, a parse failure, or an upstream protocol error -- all transient by
// definition, and all of them should escalate exactly like a 5xx.

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// A failure that carries no HTTP status at all, like a transport timeout.
type bareTransportError struct{ msg string }

func (e *bareTransportError) Error() string { return e.msg }

// TestUnknownStatusEscalatesLikeATransientError is the core regression: three
// consecutive statusless failures must NOT produce a 24-hour ban.
func TestUnknownStatusEscalatesLikeATransientError(t *testing.T) {
	r := NewRouter(&Config{}, "")

	cases := []struct {
		name    string
		errors  int
		wantMax time.Duration
	}{
		{"first failure", 1, 30 * time.Second},
		{"third failure", 3, 30 * time.Minute},
		{"tenth failure", 10, 30 * time.Minute},
		{"twentieth failure", 20, 30 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := r.cooldownForError(0, tc.errors)
			if got > tc.wantMax {
				t.Fatalf("status_code 0 with %d consecutive failures gave %v, "+
					"want <= %v: a failure with no HTTP status is transient and must "+
					"not escalate past the transient ceiling",
					tc.errors, got, tc.wantMax)
			}
			if got < time.Second {
				t.Fatalf("got %v, want a real backoff rather than none", got)
			}
		})
	}
}

// TestUnknownStatusIsNeverTheLongSoftBan pins the specific symptom: before the
// fix, 3 and 10 errors landed on 24h and 7 days.
func TestUnknownStatusIsNeverTheLongSoftBan(t *testing.T) {
	r := NewRouter(&Config{}, "")
	for _, n := range []int{3, 4, 9, 10, 50} {
		got := r.cooldownForError(0, n)
		if got >= 24*time.Hour {
			t.Errorf("status_code 0 with %d failures -> %v; a statusless failure "+
				"must never reach the permanent-soft-ban branch", n, got)
		}
	}
}

// TestSSEErrorIsClassifiedAsBadGateway makes the chat path agree with the
// /v1/responses path, which already used 502 via statusForStreamError.
func TestSSEErrorIsClassifiedAsBadGateway(t *testing.T) {
	sse := &SSEError{Data: `{"message":"JSON error injected into SSE stream","type":"upstream_error"}`}
	if got := sse.Status(); got != http.StatusBadGateway {
		t.Errorf("SSEError.Status() = %d, want %d to match statusForStreamError's "+
			"default for an upstream protocol error", got, http.StatusBadGateway)
	}
}

// TestASSEErrorTakesTheTransientPath is the end-to-end statement of the bug:
// the reported error, applied three times, must not ban the endpoint for a day.
func TestASSEErrorTakesTheTransientPath(t *testing.T) {
	r := NewRouter(&Config{}, "")
	ep := &ModelEndpoint{Provider: "commandcode", Model: "stealth/space-bunny-alpha"}

	err := error(&SSEError{Data: `{"message":"JSON error injected into SSE stream","type":"upstream_error"}`})
	for i := 0; i < 3; i++ {
		r.ApplyCooldownFromError(ep, err)
	}

	status, count := cooldownStatusForTest(t, r, ep)
	if status == 0 {
		t.Errorf("an SSE error event recorded status_code 0; it must be classified "+
			"as %d so the cooldown logic has something to work with",
			http.StatusBadGateway)
	}
	if status != http.StatusBadGateway {
		t.Errorf("status_code = %d, want %d", status, http.StatusBadGateway)
	}
	if count < 3 {
		t.Errorf("error_count = %d, want >= 3", count)
	}
	if d := r.cooldownForError(status, count); d >= 24*time.Hour {
		t.Errorf("three SSE error events produce a %v cooldown; the chat path and "+
			"the /v1/responses path must agree", d)
	}
}

// TestATransportTimeoutIsNotAPermanentFailure is the same defect seen in the
// wild: `net/http: timeout awaiting response headers` was serving 24h bans.
func TestATransportTimeoutIsNotAPermanentFailure(t *testing.T) {
	r := NewRouter(&Config{}, "")
	ep := &ModelEndpoint{Provider: "nvidia", Model: "z-ai/glm-5.3"}

	for i := 0; i < 3; i++ {
		r.ApplyCooldownFromError(ep, &bareTransportError{
			msg: `Post "https://integrate.api.nvidia.com/v1/chat/completions": net/http: timeout awaiting response headers`,
		})
	}
	if d := r.cooldownForError(0, 3); d >= 24*time.Hour {
		t.Errorf("three network timeouts give a %v cooldown; a timeout is the most "+
			"transient failure there is", d)
	}
}

// TestStatusBearingErrorsAreUnaffected guards the fix from over-reaching: 429,
// 5xx, 404 and 401/403 must keep their existing classifications.
func TestStatusBearingErrorsAreUnaffected(t *testing.T) {
	r := NewRouter(&Config{}, "")
	if got := r.cooldownForError(429, 3); got <= 30*time.Second {
		t.Errorf("429 with 3 errors = %v, want escalation above the 30s base", got)
	}
	// 404 is the 7-day misconfiguration ban on FIRST sight; from three
	// consecutive failures the soft-ban branch deliberately takes over with 24h.
	// That predates this change and is intentional, so pin both.
	if got := r.cooldownForError(404, 1); got != 7*24*time.Hour {
		t.Errorf("404 first failure = %v, want the 7-day misconfiguration cooldown", got)
	}
	if got := r.cooldownForError(404, 3); got != 24*time.Hour {
		t.Errorf("404 with 3 failures = %v, want the 24h soft ban", got)
	}
	if got := r.cooldownForError(401, 2); got != 30*time.Minute {
		t.Errorf("401 = %v, want 30m", got)
	}
	if got := r.cooldownForError(403, 1); got != 30*time.Minute {
		t.Errorf("403 = %v, want 30m", got)
	}
}

// cooldownStatusForTest reads back what the router recorded, so the assertions
// are about persisted state rather than about the classifier in isolation.
func cooldownStatusForTest(t *testing.T, r *Router, ep *ModelEndpoint) (int, int) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	cd, ok := r.cooldowns[ep.Key()]
	if !ok {
		t.Fatalf("no cooldown recorded for %s", ep.Key())
	}
	return cd.StatusCode, cd.ErrorCount
}

// TestProviderErrorStatusStillWins checks the new interface branch cannot
// override an explicit ProviderError status.
func TestProviderErrorStatusStillWins(t *testing.T) {
	r := NewRouter(&Config{}, "")
	ep := &ModelEndpoint{Provider: "kilocode", Model: "poolside/laguna-s-2.1:free"}
	err := fmt.Errorf("wrapped: %w", &ProviderError{StatusCode: 429, Body: []byte("slow down")})
	r.ApplyCooldownFromError(ep, err)
	status, _ := cooldownStatusForTest(t, r, ep)
	if status != 429 {
		t.Errorf("status_code = %d, want 429 to survive the new Status() branch", status)
	}
	if errors.Is(err, err) == false { // keep the errors import honest
		t.Fatal("unreachable")
	}
}

// ── A client error must not penalise the endpoint ─────────────────────────────
//
// 2026-10-01, opencode/space-bunny-free:
//
//	{"error":{"type":"invalid_request_error",
//	          "message":"Upstream request failed: [invalid_request_error] invalid request"}}
//	-> cooldown opencode/space-bunny-free status=400 errors=3 for 24h0m0s
//
// Three client-side rejections escalated the endpoint into a day-long ban, so even
// a corrected request kept failing and the model stayed dark for every other
// caller. A retry of an unacceptable request is byte-identical and is rejected
// identically, so a cooldown cannot help; it can only remove a working model
// from everyone else's rotation.

func TestAClientErrorGetsNoCooldown(t *testing.T) {
	r := NewRouter(&Config{}, "")
	ep := &ModelEndpoint{Provider: "opencode", Model: "space-bunny-free"}

	for i := 0; i < 5; i++ {
		got := r.ApplyCooldownForSession(ep, 400,
			`{"error":{"type":"invalid_request_error","message":"Upstream request failed: [invalid_request_error] invalid request"}}`,
			"", 0)
		if got != 0 {
			t.Fatalf("400 returned a %v cooldown on attempt %d; a request error must not "+
				"cool the endpoint at all", got, i+1)
		}
	}

	// The endpoint must still be selectable.
	if wait := r.minCooldownWait([]ModelEndpoint{*ep}, false); wait > 0 {
		t.Errorf("endpoint is held out of rotation for %v after only client errors", wait)
	}
}

func TestAClientErrorNeverReachesTheSoftBan(t *testing.T) {
	r := NewRouter(&Config{}, "")
	for _, code := range []int{400, 413, 422} {
		for n := 1; n <= 20; n++ {
			if d := r.cooldownForError(code, n); d != 0 {
				t.Fatalf("status %d with %d errors gave %v, want 0", code, n, d)
			}
		}
	}
}

func TestAClientErrorIgnoresRetryAfter(t *testing.T) {
	// A provider sending Retry-After on a 400 must not buy itself a cooldown.
	r := NewRouter(&Config{}, "")
	ep := &ModelEndpoint{Provider: "opencode", Model: "space-bunny-free"}
	got := r.ApplyCooldownForSession(ep, 400, "invalid request", "", 5*time.Minute)
	if got != 0 {
		t.Errorf("Retry-After on a client error produced a %v cooldown; it must be ignored", got)
	}
}

func TestEndpointErrorsStillCoolDown(t *testing.T) {
	// The fix must not over-reach: 404 and 401/403 describe the ENDPOINT.
	if isClientRequestError(404) {
		t.Error("404 is a misconfigured endpoint, not a bad request")
	}
	if isClientRequestError(401) || isClientRequestError(403) {
		t.Error("401/403 are credential/plan problems, not bad requests")
	}
	r := NewRouter(&Config{}, "")
	if d := r.cooldownForError(404, 3); d == 0 {
		t.Error("404 stopped cooling the endpoint; a misconfigured model must still be held out")
	}
	if d := r.cooldownForError(429, 3); d == 0 {
		t.Error("429 stopped cooling the endpoint")
	}
	if d := r.cooldownForError(503, 3); d == 0 {
		t.Error("503 stopped cooling the endpoint")
	}
}

// The recorded error must stay visible even when it earns no cooldown.
func TestAClientErrorIsStillRecorded(t *testing.T) {
	r := NewRouter(&Config{}, "")
	ep := &ModelEndpoint{Provider: "opencode", Model: "space-bunny-free"}
	msg := `{"error":{"type":"invalid_request_error","message":"invalid request"}}`
	r.ApplyCooldownForSession(ep, 400, msg, "", 0)

	status, count := cooldownStatusForTest(t, r, ep)
	if status != 400 {
		t.Errorf("status_code = %d, want 400 recorded for diagnosis", status)
	}
	if count < 1 {
		t.Errorf("error_count = %d, want it recorded", count)
	}
	r.mu.Lock()
	last := r.cooldowns[ep.Key()].LastError
	r.mu.Unlock()
	if last == "" {
		t.Error("last_error is empty; the diagnostic value of the entry is lost")
	}
}
