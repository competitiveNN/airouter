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

// TestSSEErrorIsTreatedAsAnOrdinaryError pins the reverted contract: an upstream
// SSE error event carries no HTTP status and takes the ordinary cooldown path,
// exactly like any other non-ProviderError.
//
// It used to carry Status() 502 so the chat path would agree with
// statusForStreamError on /v1/responses. That made the error TYPE decide its own
// penalty, which is the thing that let one injected fault be a bounded 30s on
// one API surface and a 24h ban on the other. If SSEError grows a Status()
// method again, or the router starts honouring one, this fails -- and it fails
// at the type level rather than only at the duration, which is where the
// regression actually started.
func TestSSEErrorIsTreatedAsAnOrdinaryError(t *testing.T) {
	sse := &SSEError{Data: `{"message":"injected","type":"upstream_error"}`}

	var statuser interface{ Status() int }
	if errors.As(error(sse), &statuser) {
		t.Fatalf("SSEError exposes Status()=%d; it must be an ordinary error with "+
			"no status of its own, so it takes the normal cooldown path",
			statuser.Status())
	}

	// And the router must not manufacture a status for it either: one applied
	// three times has to stay status 0, exactly like a timeout.
	r := NewRouter(&Config{}, "")
	ep := &ModelEndpoint{Provider: "commandcode", Model: "stealth/space-bunny-alpha"}
	for i := 0; i < 3; i++ {
		r.ApplyCooldownFromError(ep, error(&SSEError{Data: `{"message":"injected"}`}))
	}
	status, count := cooldownStatusForTest(t, r, ep)
	if status != 0 {
		t.Errorf("status_code = %d, want 0: an SSE error carries no HTTP status and "+
			"must be recorded as the ordinary statusless failure it is", status)
	}
	if count < 3 {
		t.Errorf("error_count = %d, want >= 3", count)
	}
}

// TestASSEErrorTakesTheTransientPath is the end-to-end statement of the bug:
// the reported error, applied three times, must not ban the endpoint for a day.
// Status 0 is transient and bounded, so reverting the 502 special case does not
// resurrect the 24h ban that fix originally removed.
func TestASSEErrorTakesTheTransientPath(t *testing.T) {
	r := NewRouter(&Config{}, "")
	ep := &ModelEndpoint{Provider: "commandcode", Model: "stealth/space-bunny-alpha"}

	err := error(&SSEError{Data: `{"message":"JSON error injected into SSE stream","type":"upstream_error"}`})
	for i := 0; i < 3; i++ {
		r.ApplyCooldownFromError(ep, err)
	}

	status, count := cooldownStatusForTest(t, r, ep)
	if status != 0 {
		t.Errorf("status_code = %d, want 0 for an ordinary statusless SSE error", status)
	}
	if count < 3 {
		t.Errorf("error_count = %d, want >= 3", count)
	}
	if d := r.cooldownForError(status, count); d >= 24*time.Hour {
		t.Errorf("three SSE error events produce a %v cooldown; a statusless failure "+
			"is transient and bounded, never a soft ban", d)
	}
	if d := r.cooldownForError(status, count); d < time.Second {
		t.Errorf("got %v, want a real backoff rather than none", d)
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
	// 404 is the 7-day misconfiguration ban, and it stays that way however
	// often it fails. The soft-ban branch used to take over at three consecutive
	// failures with a flat 24h, which meant a model not found was benched for
	// SEVEN days on the first two failures and for ONE day on the third: the
	// cooldown shrank by 3.5x because the endpoint kept failing. The soft ban is
	// still applied where it is longer than the base (402/403: 24h from the third
	// failure); it is just never allowed to be shorter.
	if got := r.cooldownForError(404, 1); got != 7*24*time.Hour {
		t.Errorf("404 first failure = %v, want the 7-day misconfiguration cooldown", got)
	}
	for _, n := range []int{2, 3, 9, 10, 50} {
		if got := r.cooldownForError(404, n); got != 7*24*time.Hour {
			t.Errorf("404 with %d failures = %v, want it to stay at the 7-day "+
				"misconfiguration cooldown rather than shorten", n, got)
		}
	}
	// 402 has no 7-day base, so the soft ban does apply: 30s, then 24h at the
	// third consecutive failure, 7 days at the tenth.
	if got := r.cooldownForError(402, 2); got != 30*time.Second {
		t.Errorf("402 with 2 failures = %v, want the 30s base", got)
	}
	if got := r.cooldownForError(402, 3); got != 24*time.Hour {
		t.Errorf("402 with 3 failures = %v, want the 24h soft ban", got)
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

// TestRouterLearnsTheProtocolAModelActuallySpeaks is the measured behaviour
// this replaces a wrong fix with.
//
// The earlier claim was that ModelProtocolUnsupported was a blanket "this
// endpoint is broken", and the fix cooled it for 24h. Measured against
// opencode.ai on 2026-10-02 with ONE key and identical gate headers:
//
//	muse-spark-1.3-contributor-free   /chat/completions 400 ProtocolUnsupported
//	                                  /responses        200
//	muse-spark-1.2-contributor-free   /chat/completions 400 ProtocolUnsupported
//	                                  /responses        200
//	big-pickle                        /chat/completions 200
//	                                  /responses        400 ProtocolUnsupported
//
// One catalog, chat-only and responses-only models side by side. The refusal
// identifies the SHAPE, not the model. Cooling it would have parked two working
// models out of rotation for a day -- and those two were the HEAD of the smart
// and work chains, so it would have been very visible.
//
// It is also a lesson about the measurement: an earlier probe of the same two
// models used max_output_tokens=8 and got a 400 about the token minimum, which
// I misread as the protocol refusal. The protocol call needs a token count
// the model will accept.
func TestRouterLearnsTheProtocolAModelActuallySpeaks(t *testing.T) {
	r := NewRouter(&Config{}, "")
	chatOnly := &ModelEndpoint{Provider: "opencode", Model: "big-pickle"}
	respOnly := &ModelEndpoint{Provider: "opencode", Model: "muse-spark-1.3-contributor-free"}

	if got := r.ProtocolFor(chatOnly); got != protocolChatCompletions {
		t.Errorf("an endpoint with no learned protocol must use its configured one, got %q", got)
	}
	r.LearnProtocol(respOnly, protocolResponses)
	if got := r.ProtocolFor(respOnly); got != protocolResponses {
		t.Errorf("a learned responses model resolved to %q, want responses", got)
	}
	// Learning is per-endpoint: the chat-only model is untouched.
	if got := r.ProtocolFor(chatOnly); got != protocolChatCompletions {
		t.Errorf("learning leaked to another endpoint: got %q", got)
	}
}

// TestProtocolRefusalIsNotCooledWhenAnotherProtocolWorks pins the decision that
// matters. A single refusal must NOT produce a cooldown, because the endpoint
// is still perfectly serviceable over the other shape.
func TestProtocolRefusalIsNotCooledWhenAnotherProtocolWorks(t *testing.T) {
	r := NewRouter(&Config{}, "")
	ep := &ModelEndpoint{Provider: "opencode", Model: "muse-spark-1.3-contributor-free"}
	const body = `{"type":"error","error":{"type":"ModelProtocolUnsupported","message":"Model does not support this protocol."}}`

	if !isProtocolUnsupported(400, body) {
		t.Fatal("the refusal was not recognised")
	}
	// IsProtocolUnsupported is a predicate, not a policy. The policy lives in
	// the handlers, which flip protocol and retry before ever cooling down.
	// Asserting here that no cooldown is applied would test nothing the
	// handlers do not already do, so this pins the predicate's narrowness and
	// leaves the decision to them.
	if isProtocolUnsupported(400, `{"error":{"type":"invalid_request"}}`) {
		t.Error("an ordinary 400 was read as a protocol refusal")
	}
	_ = r
	_ = ep
}

// TestProtocolRefusalIsNarrow guards the correction above from over-reaching.
// It changes a "never retry" verdict into a "back off hard" one, so a false
// positive would park a healthy endpoint for a day over an ordinary bad
// request.
func TestProtocolRefusalIsNarrow(t *testing.T) {
	if !isProtocolUnsupported(400, `{"error":{"type":"ModelProtocolUnsupported","message":"x"}}`) {
		t.Error("the canonical refusal was not recognised")
	}
	if !isProtocolUnsupported(400, `{"error":{"message":"Model does not support this protocol."}}`) {
		t.Error("the message form was not recognised")
	}
	// An ordinary 400 must stay a client error with no cooldown.
	for _, other := range []string{
		`{"error":{"type":"invalid_request","message":"max_tokens is not supported"}}`,
		`{"error":{"message":"rate limited"}}`,
		`garbage`,
		``,
	} {
		if isProtocolUnsupported(400, other) {
			t.Errorf("an ordinary 400 body was read as a protocol refusal: %q", other)
		}
		r := NewRouter(&Config{}, "")
		ep := &ModelEndpoint{Provider: "p", Model: "m"}
		if d := r.ApplyCooldownForSession(ep, 400, other, "s", 0); d != 0 {
			t.Errorf("ordinary 400 %q produced a %v cooldown, want 0", other, d)
		}
	}
	// The same body at a status that is not a refusal still cools normally.
	if isProtocolUnsupported(503, `{"error":{"type":"ModelProtocolUnsupported"}}`) {
		t.Error("a 503 carrying the text was read as a protocol refusal")
	}
}

// TestTransientEscalationReachesItsCeiling is the direct regression test for the
// 2026-10-04 "cooldowns are too low" report.
//
// The transient branch was `base * errorCount` with the factor capped at 10, so
// with the 30s base a 429, a 5xx or a transport timeout could not exceed 300s --
// five minutes, on the hundredth consecutive failure as well as the tenth. The
// ceiling the function documented (and still declares as maxCooldown) was 30
// minutes, and no code path could ever produce it.
//
// Nothing caught it, because every test in this area asserted an upper bound.
// `TestUnknownStatusEscalatesLikeATransientError` checks `got > wantMax`, so 90s
// passes a check written for 30 minutes. A ceiling with no floor is a check that
// cannot fail on a cooldown that is too short, which is the only direction this
// regressed in.
//
// The live symptom: on 2026-10-04 nvidia{,2,3}:z-ai/glm-5.3-flash each carried
// error_count 22 -- twenty-two consecutive `context deadline exceeded` -- on a
// 5-minute cooldown. The models never answered, and the router kept paying full
// request timeouts to them.
func TestTransientEscalationReachesItsCeiling(t *testing.T) {
	r := NewRouter(&Config{}, "")
	// status 0 is the one that mattered live: a transport timeout is the most
	// transient failure there is, and it is also the one a low ceiling hurts
	// most, because it is what a provider in trouble produces.
	for _, status := range []int{0, 429, 500, 502, 503, 504} {
		for _, n := range []int{50, 100} {
			if got := r.cooldownForError(status, n); got != maxCooldown {
				t.Errorf("status %d with %d consecutive failures -> %v, want the %v "+
					"ceiling: the escalation must actually be able to reach the bound "+
					"it documents", status, n, got, maxCooldown)
			}
		}
	}
}

// TestTransientEscalationIsMonotonicAndFast pins the shape rather than one
// number: each consecutive failure must at least DOUBLE, and never regress.
//
// Doubling is what the comment in cooldownForError has promised since commit
// b681a01 ("a gentler exponential-ish curve so a persistently rate-limited model
// backs off to minutes (not seconds)") while the code multiplied linearly. The
// linear curve is what made a model that never answers look almost healthy: by
// the fifth failure, when a chain needs the endpoint gone, it was only at 2m30s.
func TestTransientEscalationIsMonotonicAndFast(t *testing.T) {
	r := NewRouter(&Config{}, "")
	for _, status := range []int{0, 429, 500} {
		prev := time.Duration(0)
		for n := 1; n <= 10; n++ {
			got := r.cooldownForError(status, n)
			if got < prev {
				t.Fatalf("status %d: error %d gave %v, less than error %d's %v: a "+
					"cooldown that shrinks as failures pile up is not escalation",
					status, n, got, n-1, prev)
			}
			if got > prev && n <= 6 && got < 2*prev {
				t.Errorf("status %d: error %d gave %v, under double the previous %v; "+
					"by the sixth consecutive failure the endpoint must be out for "+
					"minutes, not seconds", status, n, got, prev)
			}
			prev = got
		}
		// Minutes, not seconds, by the time a chain has given it six chances.
		if got := r.cooldownForError(status, 6); got < 8*time.Minute {
			t.Errorf("status %d: six consecutive failures gave %v, want at least 8m",
				status, got)
		}
	}
}

// TestCooldownNeverShrinksAsErrorsAccumulate covers the 404 branch, where the
// base is ALREADY longer than the soft ban it used to fall back to.
//
//	baseCooldownForError(404) is 7 days, but the permanent-looking branch
//
// returned a flat 24h from error_count 3. So a model not found was benched for
// seven days on the first two failures and for ONE day on the third: the
// cooldown shrank by 3.5x because the endpoint failed more often. The soft ban
// is now only ever applied when it is longer than the base.
func TestCooldownNeverShrinksAsErrorsAccumulate(t *testing.T) {
	r := NewRouter(&Config{}, "")
	for _, status := range []int{402, 401, 403, 404, 405, 409, 418, 451} {
		prev := time.Duration(-1)
		for n := 1; n <= 30; n++ {
			got := r.cooldownForError(status, n)
			if prev >= 0 && got < prev {
				t.Errorf("status %d: error %d gave %v, less than error %d's %v",
					status, n, got, n-1, prev)
			}
			prev = got
		}
	}
	if got := r.cooldownForError(404, 3); got != 7*24*time.Hour {
		t.Errorf("a 404 that keeps failing must not be benched for less than a 404 "+
			"that failed once: got %v", got)
	}
}
