package main

// Tests for the steeper cooldown a TIMEOUT earns.
//
// WHY THIS FILE EXISTS
//
// Reported on 2026-10-05, from the live daemon's own log:
//
//	cooldown nvidia3/deepseek-ai/deepseek-v4.1-flash status=0 errors=4 for 4m0s:
//	  Post "https://integrate.api.nvidia.com/v1/chat/completions":
//	  net/http: timeout awaiting response headers
//
// Four consecutive timeouts, and the endpoint was back in rotation after four
// minutes -- because a timeout was sharing the 30s-doubling curve with rate
// limits (cooldownForError's transient branch, base 30s: 30s, 1m, 2m, 4m).
//
// The curve is right for a 429 and wrong for this. A 429 comes back in
// milliseconds, so a 30s base costs the caller nothing and the provider has told
// us its window anyway. A response-header timeout has already cost the caller the
// ENTIRE wait before it returns: proxy.go sets ResponseHeaderTimeout to 30s, so
// every one of these failures burns 30 seconds of the caller's latency and
// delivers nothing. Retrying the endpoint a minute later gets the same 30 seconds
// back -- the upstream is not slow to respond, it is not responding.
//
// What the client actually paid before the fix: 30s of stall, then 30s, then 60s,
// then 30s -- four full timeouts inside a session whose fallback chain exists so
// that it keeps moving. Four consecutive failures and the model was still only
// benched for four minutes.
//
// WHAT THIS DOES NOT CHANGE
//
// The ceiling. A statusless failure stays bounded at maxCooldown and is never
// soft-banned -- that is a load-bearing decision with its own regression tests
// (cooldown_classification_test.go), and 2026-10-01 is what happens when a
// statusless failure is treated as "probably permanent": three timeouts took an
// endpoint out for 24 hours. A steeper ramp to the SAME bound is not that. The
// circuit breaker, not this number, is what retires an endpoint that never
// answers at all.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// timeoutError is a deadline whose MESSAGE says nothing about timing.
//
// net/http's own timeout messages are recognisable by text, which is exactly why
// the cooldown path must not rely on text: `context deadline exceeded` is also
// what every other expired context says, and a provider body can say anything.
// This type carries the only signal that is actually a property of the failure --
// net.Error.Timeout() -- with a message chosen so no text match can find it.
type timeoutError struct{}

func (timeoutError) Error() string   { return "upstream went quiet" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// notATimeoutError is a transport failure with a net.Error type and no deadline.
type notATimeoutError struct{}

func (notATimeoutError) Error() string   { return "connection reset by peer" }
func (notATimeoutError) Timeout() bool   { return false }
func (notATimeoutError) Temporary() bool { return true }

// realHeaderTimeout returns the error net/http actually produces when an upstream
// accepts the connection and then fails to send response headers in time -- the
// failure from the reported log line, produced by the standard library rather
// than by a stub, so the classification is proved against the real error value
// and not against a hand-written imitation of one.
func realHeaderTimeout(t *testing.T) error {
	t.Helper()
	// The handler outlives the client on purpose, and is short enough that
	// httptest's Close does not make the suite slow.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
	}))
	defer srv.Close()
	client := &http.Client{Timeout: 40 * time.Millisecond}
	resp, err := client.Get(srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("the stalling upstream answered; this test can prove nothing")
	}
	if !isTimeoutFailure(err) {
		t.Fatalf("net/http produced %v, which isTimeoutFailure does not recognise; "+
			"the test is no longer exercising a real timeout", err)
	}
	return err
}

// recordedCooldown reads back how long the endpoint was actually benched for, so
// the assertions are about persisted state and not about a classifier in
// isolation.
func recordedCooldown(t *testing.T, r *Router, ep *ModelEndpoint) time.Duration {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	cd, ok := r.cooldowns[ep.Key()]
	if !ok {
		t.Fatalf("no cooldown recorded for %s", ep.Key())
	}
	got := time.Until(cd.Expiry)
	// A client error records a ZERO cooldown: the entry exists, its expiry is
	// already now, and the read-back is a few microseconds negative. Anything
	// meaningfully negative means the test read back state that no longer exists.
	if got < -readbackSlack {
		t.Fatalf("cooldown for %s expired %v ago; the test read back state that no "+
			"longer exists", ep.Key(), -got)
	}
	if got < 0 {
		return 0
	}
	return got
}

// readbackSlack is how much SHORTER a recorded cooldown is than the duration the
// router computed: Expiry is a wall-clock timestamp, so time.Until it lands a few
// microseconds short. Every comparison between two recorded durations needs it --
// without it the ceiling, which every run lands on from the fifth timeout, reads
// as a shrink. Five seconds is far below the smallest step on either ramp (60s).
const readbackSlack = 5 * time.Second

// withinSlack compares a recorded cooldown against the duration the router
// intended.
func withinSlack(t *testing.T, got, want time.Duration, what string) {
	t.Helper()
	if got < want-readbackSlack || got > want+readbackSlack {
		t.Fatalf("%s: recorded %v, want %v", what, got, want)
	}
}

// TestTheStaleRampWouldHaveCalledFourMinutesEnough is the regression for the
// reported log line, stated as the report stated it.
//
// It asserts a bound rather than a number: whatever the exact ramp becomes, four
// consecutive timeouts must buy MORE than the 4m0s the router recorded at
// errors=4 on 2026-10-05. Asserting 16m here instead would make a later retune
// of the base look like a regression; asserting only "more than 4m" cannot be
// satisfied by the curve this file exists to replace.
func TestTheStaleRampWouldHaveCalledFourMinutesEnough(t *testing.T) {
	r := NewRouter(&Config{}, "")
	ep := &ModelEndpoint{Provider: "nvidia3", Model: "deepseek-ai/deepseek-v4.1-flash"}
	timeout := realHeaderTimeout(t)

	var got time.Duration
	for i := 0; i < 4; i++ {
		r.ApplyCooldownFromError(ep, timeout)
		got = recordedCooldown(t, r, ep)
	}

	status, count := cooldownStatusForTest(t, r, ep)
	if status != 0 {
		t.Errorf("status_code = %d, want 0: a transport timeout carries no HTTP status", status)
	}
	if count != 4 {
		t.Errorf("error_count = %d, want 4", count)
	}
	if got <= 4*time.Minute {
		t.Fatalf("four consecutive response-header timeouts benched the endpoint for "+
			"only %v. That is the 2026-10-05 number (status=0 errors=4 for 4m0s) and "+
			"it is four full %v stalls for a caller whose fallback chain should have "+
			"moved on after the first", got, timeoutWait)
	}
}

// timeoutWait is the per-attempt wall-clock cost of the failure under test, from
// proxy.go's ResponseHeaderTimeout. Used in failure messages so the numbers in
// them are the ones that matter to a caller.
const timeoutWait = 30 * time.Second

// TestATimeoutRampsSteeperThanTheGenericTransientCurve pins the shape and the
// exact values: a 2m base doubling to the same 30m ceiling, reaching the ceiling
// on the fifth consecutive timeout where the generic curve needs seven.
//
// The ceiling is part of the assertion, not an accident of the arithmetic. A
// steeper base that also raised the bound would be the soft ban that
// cooldown_classification_test.go exists to prevent.
func TestATimeoutRampsSteeperThanTheGenericTransientCurve(t *testing.T) {
	r := NewRouter(&Config{}, "")
	ep := &ModelEndpoint{Provider: "nvidia3", Model: "deepseek-ai/deepseek-v4.1-flash"}
	timeout := realHeaderTimeout(t)

	want := []time.Duration{
		2 * time.Minute,
		4 * time.Minute,
		8 * time.Minute,
		16 * time.Minute,
		maxCooldown,
		maxCooldown,
		maxCooldown,
	}
	for i, w := range want {
		r.ApplyCooldownFromError(ep, timeout)
		withinSlack(t, recordedCooldown(t, r, ep), w,
			fmt.Sprintf("timeout %d", i+1))
	}

	// And it is genuinely steeper, not merely different: at every error count
	// from 1 to the ceiling, a timeout must wait longer than the generic status-0
	// curve for the same count.
	for n := 1; n <= 12; n++ {
		r2 := NewRouter(&Config{}, "")
		ep2 := &ModelEndpoint{Provider: "p", Model: "m"}
		for i := 0; i < n; i++ {
			r2.ApplyCooldownFromError(ep2, timeout)
		}
		got := recordedCooldown(t, r2, ep2)
		// "No gentler", not "strictly harder": once both curves are pinned to the
		// shared ceiling they are equal, and that is the intended end state.
		if generic := r2.cooldownForError(0, n); got+readbackSlack <= generic {
			t.Errorf("timeout %d benched the endpoint for %v, the generic status-0 "+
				"curve for %v: a timeout must back off at least as hard as a rate "+
				"limit, and harder below the ceiling", n, got, generic)
		}
	}
}

// TestATimeoutIsStillBoundedAndNeverSoftBanned guards the fix from over-reaching
// in the direction that has already bitten this router once.
//
// 2026-10-01: three consecutive statusless failures produced a 24-hour cooldown,
// because "no HTTP status" was treated as "probably permanent", and live 15 of 23
// cooldown entries carried status_code 0. A steeper ramp must not turn back into
// a ban: the bound is maxCooldown, forever, at any error count.
func TestATimeoutIsStillBoundedAndNeverSoftBanned(t *testing.T) {
	r := NewRouter(&Config{}, "")
	timeout := realHeaderTimeout(t)
	prev := time.Duration(0)
	for n := 1; n <= 100; n++ {
		ep := &ModelEndpoint{Provider: "p", Model: "m"}
		for i := 0; i < n; i++ {
			r.ApplyCooldownFromError(ep, timeout)
		}
		got := recordedCooldown(t, r, ep)
		if got > maxCooldown+time.Minute {
			t.Fatalf("timeout %d: benched for %v, past the %v ceiling", n, got, maxCooldown)
		}
		if got >= 24*time.Hour {
			t.Fatalf("timeout %d: benched for %v. A statusless failure must never "+
				"reach the permanent-soft-ban branch", n, got)
		}
		if got < prev-readbackSlack {
			t.Errorf("timeout %d: benched for %v, less than timeout %d's %v: a "+
				"cooldown that shrinks as failures pile up is not escalation",
				n, got, n-1, prev)
		}
		prev = got
	}
}

// TestANonTimeoutFailureKeepsTheGenericCurve guards the other direction: only a
// real deadline may take the steeper ramp.
//
// Statusless failures are not all timeouts. A truncated body, a malformed SSE
// event and a reset connection are the same status 0 and none of them has burnt
// a 30-second wait, so they keep the generic base. And a provider that answers
// WITH a status keeps that status's curve even when its body says "timeout" --
// otherwise a rate limiter whose message contains the word would be benched on a
// deadline curve it never triggered.
func TestANonTimeoutFailureKeepsTheGenericCurve(t *testing.T) {
	// timeoutWording says whether the message WOULD be recognised as a deadline
	// if it arrived without a status. Both kinds of case are here, and each
	// asserts its own expectation about isTimeoutMessage first, because a case
	// whose premise is false proves nothing: only a case with timeoutWording ==
	// true can hold the statusCode == 0 gate responsible for keeping a provider
	// body off the deadline ramp.
	cases := []struct {
		name           string
		status         int
		err            error
		timeoutWording bool
	}{
		{"truncated body", 0, &bareTransportError{msg: "unexpected EOF"}, false},
		{"reset connection", 0, &bareTransportError{msg: "read tcp 10.0.0.1:443: connection reset by peer"}, false},
		{"upstream sse error", 0, &SSEError{Data: `{"message":"upstream_error"}`}, false},
		{"bare word timeout, no status", 0, &bareTransportError{msg: "upstream timeout"}, false},
		// The three below carry a status AND deadline wording. Without the
		// statusCode == 0 gate these would be benched on the timeout ramp: a
		// provider that answers "i/o timeout" in a 429 body would get the
		// endpoint taken out for minutes over a rate limit it did report
		// honestly.
		{"rate limit body mentioning i/o timeout", 429, &ProviderError{StatusCode: 429, Body: []byte(`{"error":{"message":"rate limit: i/o timeout"}}`)}, true},
		{"server error body mentioning a deadline", 500, &ProviderError{StatusCode: 500, Body: []byte(`{"error":{"message":"upstream read deadline exceeded"}}`)}, true},
		{"client error body mentioning a deadline", 400, &ProviderError{StatusCode: 400, Body: []byte(`{"error":{"message":"context deadline exceeded"}}`)}, true},
		// Statuses with their own base keep it.
		{"gateway timeout status", 504, &ProviderError{StatusCode: 504, Body: []byte(`{"error":{"message":"gateway timeout"}}`)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTimeoutMessage(tc.err.Error()); got != tc.timeoutWording {
				t.Fatalf("the setup is wrong: isTimeoutMessage(%q) = %v, want %v",
					tc.err.Error(), got, tc.timeoutWording)
			}
			r := NewRouter(&Config{}, "")
			ep := &ModelEndpoint{Provider: "p", Model: "m"}
			r.ApplyCooldownFromError(ep, tc.err)
			got := recordedCooldown(t, r, ep)
			want := r.cooldownForError(tc.status, 1)
			withinSlack(t, got, want, tc.name)
			if tc.timeoutWording && got >= timeoutCooldownBase-readbackSlack {
				t.Fatalf("%s: %v, which is the deadline ramp. A failure that arrived "+
					"WITH an HTTP status keeps that status's classification; the message "+
					"is not allowed to promote it", tc.name, got)
			}
		})
	}

	// And after four of them the generic curve is still in charge, which is the
	// number the stale log line was actually reporting.
	r := NewRouter(&Config{}, "")
	ep := &ModelEndpoint{Provider: "p", Model: "m"}
	for i := 0; i < 4; i++ {
		r.ApplyCooldownFromError(ep, &bareTransportError{msg: "unexpected EOF"})
	}
	withinSlack(t, recordedCooldown(t, r, ep), 4*time.Minute, "four non-timeout failures")
}

// TestATimeoutIsRecognisedByTypeAndNotOnlyByText splits the two inputs to the
// classification, because only one of them survives the trip to the cooldown path
// in every case.
//
// isTimeoutFailure reads the error's type. isTimeoutMessage reads its text, and
// exists only for a deadline that arrived as nothing but a string. Neither may
// stand in for the other: if the typed check were dropped, every timeout whose
// message is unrecognisable would silently fall back to the gentle curve; if the
// text check claimed to be a type check, a provider could talk its way onto the
// steeper ramp.
func TestATimeoutIsRecognisedByTypeAndNotOnlyByText(t *testing.T) {
	if !isTimeoutFailure(timeoutError{}) {
		t.Error("a net.Error with Timeout() true was not recognised as a deadline")
	}
	if isTimeoutFailure(notATimeoutError{}) {
		t.Error("a net.Error with Timeout() false was read as a deadline")
	}
	if isTimeoutFailure(nil) {
		t.Error("a nil error was read as a deadline")
	}
	// A connection reset really is a net.Error, and really is not a timeout.
	var opErr error = &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}
	if isTimeoutFailure(opErr) {
		t.Error("a connection reset was read as a deadline")
	}
	// Context expiry carries no timeout-shaped type of its own beyond net.Error,
	// and its message is shared with unrelated failures -- which is precisely why
	// the type is the primary signal.
	if !isTimeoutFailure(fmt.Errorf("sending request: %w", context.DeadlineExceeded)) {
		t.Error("an expired context was not recognised as a deadline")
	}
	// Text alone is not a type: a bare error carrying a timeout's wording has no
	// deadline in it, and the text fallback is the only thing that can see it.
	bare := errors.New(`Post "https://integrate.api.nvidia.com/v1/chat/completions": net/http: timeout awaiting response headers`)
	if isTimeoutFailure(bare) {
		t.Error("a string was read as a deadline: isTimeoutFailure must not be a text match")
	}
	if !isTimeoutMessage(bare.Error()) {
		t.Errorf("the canonical header-timeout message was not recognised as a timeout")
	}

	// End to end, on the type path: a deadline that no text match can find still
	// takes the steeper ramp, through the ordinary production entry point.
	r := NewRouter(&Config{}, "")
	ep := &ModelEndpoint{Provider: "nvidia3", Model: "deepseek-ai/deepseek-v4.1-flash"}
	r.ApplyCooldownFromError(ep, timeoutError{})
	withinSlack(t, recordedCooldown(t, r, ep), 2*time.Minute, "unrecognisable-message timeout")

	// And the same holds once the type is gone and only the message survives,
	// which is what the reported log line is.
	r2 := NewRouter(&Config{}, "")
	ep2 := &ModelEndpoint{Provider: "nvidia3", Model: "deepseek-ai/deepseek-v4.1-flash"}
	r2.ApplyCooldownFromError(ep2, bare)
	withinSlack(t, recordedCooldown(t, r2, ep2), 2*time.Minute, "message-only timeout")
}
