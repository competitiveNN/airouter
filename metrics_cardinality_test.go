package main

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// epKey is the key the attempt maps store an endpoint under. Tests that poke at
// the registry directly have to use it too: the keys are pre-rendered label
// sets, not bare values, which is the invariant labelKey exists to enforce.
func epKey(endpoint string) labelKey { return singleLabelKey("endpoint", endpoint) }

// The metrics registry is keyed by label values that come from configuration,
// and configuration is not fixed for the process lifetime: HandleAdminConfig
// accepts an arbitrary new config and watchConfig hot-reloads the file on a 3 s
// poll. Nothing evicted a series, and each endpoint cost a counter set plus a
// 12-bucket histogram, so a config churn loop (or a model sync that keeps
// inventing model ids) grew the process until it was OOM-killed. These tests
// pin the bound so that regression cannot ship silently.

// TestMetricsAttemptCardinalityIsBounded pushes far more distinct endpoint
// labels than the cap allows and asserts every backing map stops growing at the
// cap. It checks the maps directly rather than only the rendered output,
// because output-only truncation would still consume the memory.
func TestMetricsAttemptCardinalityIsBounded(t *testing.T) {
	m := NewMetrics()
	const flood = 5000
	for i := 0; i < flood; i++ {
		m.Attempt(fmt.Sprintf("churn:model-%d", i), time.Millisecond, i%2 == 0)
	}

	if got := len(m.attemptsByEndpoint); got > defaultMaxLabelValues {
		t.Errorf("attemptsByEndpoint grew to %d, above the cap of %d", got, defaultMaxLabelValues)
	}
	if got := len(m.attemptFailuresByEnd); got > defaultMaxLabelValues {
		t.Errorf("attemptFailuresByEnd grew to %d, above the cap of %d", got, defaultMaxLabelValues)
	}
	if got := len(m.attemptLatencyNSByEnd); got > defaultMaxLabelValues {
		t.Errorf("attemptLatencyNSByEnd grew to %d, above the cap of %d", got, defaultMaxLabelValues)
	}
	// The histogram is the expensive one (a slice per entry), so it is the
	// reason the cap matters at all.
	if got := len(m.attemptHistByEnd); got > defaultMaxLabelValues {
		t.Errorf("attemptHistByEnd grew to %d, above the cap of %d", got, defaultMaxLabelValues)
	}

	// Every observation must still be accounted for somewhere. Collapsing or
	// evicting is only acceptable if the counts stay truthful, otherwise a
	// churning provider would render as a perfectly healthy endpoint.
	if total := m.attemptsTotalLocked(); total != flood {
		t.Errorf("attempt totals do not add up: got %d, want %d — recycling must not drop observations", total, flood)
	}
	// Under the eviction policy a flood of new labels is admitted by recycling
	// older ones, so the overflow counter legitimately stays at 0 and the
	// eviction counter is what moves. Asserting the wrong one here would push a
	// future maintainer toward a policy that silently aggregates instead.
	if m.labelsEvicted.Load() == 0 {
		t.Error("labelsEvicted is 0 after flooding past the cap; the window is not actually recycling")
	}
	if m.labelsOverflowed.Load() != 0 {
		t.Errorf("labelsOverflowed = %d; a healthy recycling window should never need to collapse", m.labelsOverflowed.Load())
	}
}

// TestMetricsCardinalityBoundaryIsExact walks the cap boundary at exactly the
// interesting counts and asserts, at every one of them, that all four attempt
// maps agree on the key set and that the cap holds.
//
// This is the bug class the tests already caught once: bounding each map
// independently let the maps disagree at the boundary, producing an endpoint
// with attempt counts but no latency histogram. Asserting the key sets are
// identical — not just "under the cap" — is what makes that impossible to
// reintroduce. The counts are chosen to straddle the reserved overflow slot and
// the recycling threshold, since that is exactly where an off-by-one hides.
func TestMetricsCardinalityBoundaryIsExact(t *testing.T) {
	cap := defaultMaxLabelValues
	for _, distinct := range []int{0, 1, 2, cap - 2, cap - 1, cap, cap + 1, cap * 2} {
		t.Run(fmt.Sprintf("distinct=%d", distinct), func(t *testing.T) {
			m := NewMetrics()
			for i := 0; i < distinct; i++ {
				m.Attempt(fmt.Sprintf("ep:%d", i), time.Duration(i+1)*time.Millisecond, i%3 == 0)
			}

			if got := len(m.attemptsByEndpoint); got > cap {
				t.Errorf("attemptsByEndpoint = %d, above the cap of %d", got, cap)
			}

			// The invariant: every key present in the counter maps has a
			// histogram, and vice versa. A key with a histogram but no counter
			// would be invisible in the output; a key with a counter but no
			// histogram would print a zeroed duration.
			for ep := range m.attemptsByEndpoint {
				if _, ok := m.attemptHistByEnd[ep]; !ok {
					t.Errorf("endpoint %q has a counter but no histogram", ep)
				}
			}
			for ep := range m.attemptHistByEnd {
				if _, ok := m.attemptsByEndpoint[ep]; !ok {
					t.Errorf("endpoint %q has a histogram but no counter", ep)
				}
			}
			// Failures only exist for endpoints that had a failure, and the
			// latency sum for any endpoint with an attempt, so they are a subset
			// rather than an equality — but never a superset.
			for ep := range m.attemptFailuresByEnd {
				if _, ok := m.attemptsByEndpoint[ep]; !ok {
					t.Errorf("endpoint %q has a failure counter but no attempt counter", ep)
				}
			}
			for ep := range m.attemptLatencyNSByEnd {
				if _, ok := m.attemptsByEndpoint[ep]; !ok {
					t.Errorf("endpoint %q has a latency sum but no attempt counter", ep)
				}
			}

			// Every observation is still accounted for exactly once, either on
			// its own series or in the bucket.
			var total int64
			for _, c := range m.attemptsByEndpoint {
				total += c.Load()
			}
			if total != int64(distinct) {
				t.Errorf("attempt total = %d, want %d — an observation was dropped", total, distinct)
			}

			// The ring and the map must not disagree, or the next eviction would
			// recycle a slot for a label that is not actually in the map. The
			// overflow bucket is deliberately NOT in the ring (it is where
			// recycled labels go, so it can never itself be recycled), so the
			// expected ring size is len(map) minus one when the bucket is live.
			ringWanted := len(m.attemptsByEndpoint)
			if _, hasBucket := m.attemptsByEndpoint[overflowLabelKey]; hasBucket {
				ringWanted--
			}
			if len(m.attemptIndex) != ringWanted {
				t.Errorf("admission index has %d entries, want %d (map size %d)", len(m.attemptIndex), ringWanted, len(m.attemptsByEndpoint))
			}
			if len(m.attemptRing) != ringWanted {
				t.Errorf("admission ring has %d entries, want %d", len(m.attemptRing), ringWanted)
			}
			for ep := range m.attemptIndex {
				if _, ok := m.attemptsByEndpoint[ep]; !ok {
					t.Errorf("ring tracks %q but the map does not", ep)
				}
				if ep == overflowLabelKey {
					t.Error("the overflow bucket is in the recycling ring; it must be pinned outside it")
				}
			}
		})
	}
}

// TestMetricsEvictionPreventsLabelStarvation is the test for the reason the
// window is FIFO rather than "first come, forever".
//
// With a never-releasing registry, a burst of labels from a config that is no
// longer live holds its slots permanently, and a genuinely hot endpoint that
// first appears afterwards is collapsed into __overflow__ for the rest of the
// process's life. The gateway would then report a broken upstream as "no data"
// indefinitely — the worst possible failure for the series that exist to answer
// "is this upstream healthy?".
func TestMetricsEvictionPreventsLabelStarvation(t *testing.T) {
	m := NewMetrics()
	cap := defaultMaxLabelValues

	// Phase 1: a stale config fills the window with labels, then goes dead.
	// These are the ones that must NOT hold their slots forever.
	//
	// Note the eviction count here: admitting cap labels into a cap-sized
	// window necessarily evicts one, because the reserved overflow slot means
	// only cap-1 real labels fit before the first eviction is forced. The
	// assertion is therefore about the delta, not about zero.
	for i := 0; i < cap; i++ {
		m.Attempt(fmt.Sprintf("stale:%d", i), time.Millisecond, false)
	}
	evictedAfterFill := m.labelsEvicted.Load()
	if evictedAfterFill != 1 {
		t.Errorf("filling the window evicted %d labels, want exactly 1 (the reserved overflow slot)", evictedAfterFill)
	}

	// Phase 2: the config is replaced and the real hot endpoint appears.
	m.Attempt("hot:primary", 5*time.Millisecond, false)

	if _, ok := m.attemptsByEndpoint[epKey("hot:primary")]; !ok {
		t.Fatal("a newly appearing hot endpoint was collapsed instead of admitted — it would report as no data forever")
	}
	if got := m.labelsEvicted.Load(); got != evictedAfterFill+1 {
		t.Errorf("labelsEvicted = %d, want %d (the oldest stale label should have been recycled)", got, evictedAfterFill+1)
	}

	// The recycled labels' history must be rolled into the bucket, not dropped.
	// Two labels have been evicted by now: one during the fill (the reserved
	// overflow slot) and one when hot:primary was admitted.
	bucket := m.attemptsByEndpoint[overflowLabelKey]
	if bucket == nil {
		t.Fatal("no overflow bucket: the evicted label's history was dropped")
	}
	if got := bucket.Load(); got != 2 {
		t.Errorf("overflow bucket = %d, want 2 (the two evicted stale labels' attempts)", got)
	}
	// Their failure counters and latency sums must move with them, not vanish.
	if got := m.attemptFailuresByEnd[overflowLabelKey]; got == nil || got.Load() != 2 {
		t.Errorf("evicted labels' failure count was not rolled up: got %v, want 2", got)
	}
	if got := m.attemptLatencyNSByEnd[overflowLabelKey]; got == nil || got.Load() != 2*int64(time.Millisecond) {
		t.Errorf("evicted labels' latency sum was not rolled up: got %v, want 2ms", got)
	}
	// And a stale series that was displaced is gone, not merely hidden.
	if _, ok := m.attemptsByEndpoint[epKey("stale:0")]; ok {
		t.Error("stale:0 still has a series after being recycled")
	}
	// Crucially, a stale label that was NOT the one displaced still has its own
	// series, so the operator still sees the rest of the old config.
	if _, ok := m.attemptsByEndpoint[epKey("stale:100")]; !ok {
		t.Error("stale:100 lost its series; only the oldest label should have been recycled")
	}
}

// TestMetricsEvictionIsFIFONotRandom pins the eviction order, because "some
// other label" is not a policy an operator can reason about, and because the
// policy is documented as FIFO-by-admission rather than LRU.
//
// It is explicitly NOT least-recently-used. Re-touching an already-admitted
// label does not move it to the back of the window: doing that would need a
// read-lock-and-bump on every observation, which is a global write lock on the
// hot path. Admission order is a good enough proxy, and this test is what tells
// a future maintainer they changed documented behaviour if they "fix" it to
// LRU.
func TestMetricsEvictionIsFIFONotRandom(t *testing.T) {
	m := NewMetrics()
	m.SetLabelCap(minLabelValues) // 16, so the test stays fast and readable

	// Fill with cap-1 real labels, leaving the reserved slot free so no eviction
	// is triggered by the fill itself.
	for i := 0; i < minLabelValues-1; i++ {
		m.Attempt(fmt.Sprintf("admitted:%02d", i), time.Millisecond, true)
	}
	if m.labelsEvicted.Load() != 0 {
		t.Fatalf("filling cap-1 labels evicted %d; the test setup is wrong", m.labelsEvicted.Load())
	}
	if _, ok := m.attemptsByEndpoint[epKey("admitted:00")]; !ok {
		t.Fatal("admitted:00 missing after the fill")
	}

	// Re-touch the oldest label, then force exactly one eviction.
	m.Attempt("admitted:00", time.Millisecond, true)
	m.Attempt("newcomer", time.Millisecond, true)

	// FIFO-by-admission: admitted:00 is still the oldest, so it goes first.
	// (Under LRU it would survive and admitted:01 would be evicted instead.)
	if _, ok := m.attemptsByEndpoint[epKey("admitted:00")]; ok {
		t.Error("admitted:00 survived eviction after being re-touched; the policy is no longer FIFO-by-admission")
	}
	if _, ok := m.attemptsByEndpoint[epKey("admitted:01")]; !ok {
		t.Error("admitted:01 was evicted instead of admitted:00; eviction is not FIFO")
	}
	if _, ok := m.attemptsByEndpoint[epKey("newcomer")]; !ok {
		t.Error("the newcomer that triggered eviction has no series")
	}
}

// TestMetricsEvictionNeverDisplacesTheOverflowBucket verifies the one label that
// must never be recycled. Everything displaced goes INTO that bucket, so losing
// it would drop observations and could give the same label both a real series
// and a bucket role.
func TestMetricsEvictionNeverDisplacesTheOverflowBucket(t *testing.T) {
	m := NewMetrics()
	m.SetLabelCap(minLabelValues)

	// Overflow past the cap so the bucket definitely exists and is old.
	for i := 0; i < minLabelValues*3; i++ {
		m.Attempt(fmt.Sprintf("churn:%d", i), time.Millisecond, true)
	}
	bucketBefore := m.attemptsByEndpoint[overflowLabelKey]
	if bucketBefore == nil {
		t.Fatal("no overflow bucket after flooding")
	}
	before := bucketBefore.Load()

	// Keep forcing evictions.
	for i := 0; i < minLabelValues*2; i++ {
		m.Attempt(fmt.Sprintf("more:%d", i), time.Millisecond, true)
	}
	after := m.attemptsByEndpoint[overflowLabelKey]
	if after == nil {
		t.Fatal("the overflow bucket was evicted")
	}
	if after.Load() < before {
		t.Errorf("overflow bucket went backwards: %d -> %d (its history was lost)", before, after.Load())
	}
	// The bucket's series must be the real one, not a fresh empty counter left
	// behind by a recycling bug.
	if after != bucketBefore {
		t.Error("the overflow bucket was replaced rather than preserved")
	}
}

// TestMetricsEvictedCounterIsExported asserts the eviction condition is visible
// on the endpoint. A steady climb here is the operator's signal that the cap is
// too small for the deployment.
func TestMetricsEvictedCounterIsExported(t *testing.T) {
	m := NewMetrics()
	m.SetLabelCap(minLabelValues)
	for i := 0; i < minLabelValues*3; i++ {
		m.Attempt(fmt.Sprintf("ep:%d", i), time.Millisecond, true)
	}
	body := renderMetricsBody(m).String()
	if !strings.Contains(body, "airouter_metrics_label_evictions_total") {
		t.Errorf("eviction counter is not exported:\n%s", firstLines(body, 25))
	}
	if m.labelsEvicted.Load() == 0 {
		t.Error("eviction counter is 0 despite forcing many evictions")
	}
}

// TestMetricsSetLabelCap covers the configurable cap: it takes effect, it is
// enforced, and out-of-range values are ignored rather than silently clamped
// (a cap that is quietly reinterpreted looks like a setting that does not work).
func TestMetricsSetLabelCap(t *testing.T) {
	t.Run("applies and is enforced", func(t *testing.T) {
		m := NewMetrics()
		m.SetLabelCap(64)
		if got := m.LabelCap(); got != 64 {
			t.Fatalf("LabelCap() = %d, want 64", got)
		}
		for i := 0; i < 500; i++ {
			m.Attempt(fmt.Sprintf("ep:%d", i), time.Millisecond, true)
		}
		if got := len(m.attemptsByEndpoint); got > 64 {
			t.Errorf("attemptsByEndpoint = %d, above the configured cap of 64", got)
		}
	})

	t.Run("ignores out of range", func(t *testing.T) {
		for _, bad := range []int{0, 1, minLabelValues - 1, maxAllowedLabels + 1, -100} {
			m := NewMetrics()
			m.SetLabelCap(bad)
			if got := m.LabelCap(); got != defaultMaxLabelValues {
				t.Errorf("SetLabelCap(%d) was applied as %d; out-of-range values must be ignored", bad, got)
			}
		}
	})

	t.Run("shrinking keeps totals", func(t *testing.T) {
		m := NewMetrics()
		for i := 0; i < 100; i++ {
			m.Attempt(fmt.Sprintf("ep:%d", i), time.Millisecond, true)
		}
		before := m.attemptsTotalLocked()
		m.SetLabelCap(32)
		// Shrinking must not retroactively delete series or lose counts; the
		// window converges on the new size as new labels arrive.
		if got := m.attemptsTotalLocked(); got != before {
			t.Errorf("shrinking the cap lost observations: %d -> %d", before, got)
		}
		if got := m.LabelCap(); got != 32 {
			t.Errorf("LabelCap() = %d after shrink, want 32", got)
		}
		// And the smaller cap is enforced going forward.
		for i := 0; i < 500; i++ {
			m.Attempt(fmt.Sprintf("post:%d", i), time.Millisecond, true)
		}
		if got := len(m.attemptsByEndpoint); got > 32 {
			t.Errorf("attemptsByEndpoint = %d after shrink, above the new cap of 32", got)
		}
	})
}

// attemptsTotalLocked sums the attempt counters. Test helper.
func (m *Metrics) attemptsTotalLocked() int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var total int64
	for _, c := range m.attemptsByEndpoint {
		total += c.Load()
	}
	return total
}

// TestMetricsOverflowKeepsHotEndpointsDistinct verifies that a label already in
// the window keeps its own series, with its own history, while newer labels are
// recycled around it.
//
// This is the property that makes the per-endpoint series trustworthy under a
// churning config: the operator's live endpoints do not get diluted into a
// shared aggregate just because something else was admitted more recently.
func TestMetricsOverflowKeepsHotEndpointsDistinct(t *testing.T) {
	m := NewMetrics()
	cap := defaultMaxLabelValues

	// Fill the window with the endpoints we intend to keep visible. cap-1 real
	// labels fit without evicting anything, because one slot is reserved for
	// the overflow bucket.
	for i := 0; i < cap-1; i++ {
		m.Attempt(fmt.Sprintf("keep:%d", i), time.Millisecond, true)
	}
	// Force exactly one eviction with a brand-new label. FIFO-by-admission means
	// the victim is the OLDEST label in the window, so keep:0 is the one that
	// goes — a label that is still tracked keeps its distinct series only while
	// it remains in the window, which is the documented trade-off of
	// admission-order eviction over LRU.
	m.Attempt("newcomer", time.Millisecond, true)

	// A label in the middle of the window keeps its own series, with its own
	// history, undisturbed by someone else being admitted.
	if got := m.attemptsByEndpoint[epKey("keep:1")]; got == nil || got.Load() != 1 {
		t.Errorf("keep:1 lost its distinct series or its history: got %v, want count 1", got)
	}
	if h, ok := m.attemptHistByEnd[epKey("keep:1")]; !ok {
		t.Error("keep:1 has a counter but no histogram")
	} else {
		var sum int64
		for i := range h {
			sum += h[i].Load()
		}
		if sum != 1 {
			t.Errorf("keep:1 histogram holds %d observations, want 1", sum)
		}
	}
	// The newcomer displaced the oldest label, and that label's history went to
	// the bucket rather than being dropped.
	if _, ok := m.attemptsByEndpoint[epKey("newcomer")]; !ok {
		t.Error("the newcomer has no series")
	}
	if _, ok := m.attemptsByEndpoint[epKey("keep:0")]; ok {
		t.Error("keep:0 is the oldest label and should have been the one evicted")
	}
	bucket := m.attemptsByEndpoint[overflowLabelKey]
	if bucket == nil {
		t.Fatal("no overflow bucket; the displaced label's history was dropped")
	}
	if bucket.Load() != 1 {
		t.Errorf("overflow bucket = %d, want 1 (the displaced keep:0 observation)", bucket.Load())
	}

	body := renderMetricsBody(m).String()
	if !strings.Contains(body, `airouter_endpoint_attempts_total{endpoint="keep:1"} 1`) {
		t.Errorf("keep:1 missing or wrong in output:\n%s", firstLines(body, 40))
	}
	if !strings.Contains(body, fmt.Sprintf(`endpoint="%s"`, overflowLabelValue)) {
		t.Error("overflow bucket missing from /metrics output")
	}
}

// TestMetricsCardinalityBoundedAcrossAllLabelMaps covers the maps that are not
// reached through Attempt: the client-supplied model name and the per-endpoint
// fallback counter. The model label is the more interesting of the two, since
// the model string arrives from the request before validation on several paths.
func TestMetricsCardinalityBoundedAcrossAllLabelMaps(t *testing.T) {
	m := NewMetrics()
	const flood = 5000
	for i := 0; i < flood; i++ {
		m.Request(fmt.Sprintf("attacker-model-%d", i), 200, time.Millisecond)
		m.Fallback(fmt.Sprintf("churn:%d", i))
		m.CircuitTransition(CircuitClosed, CircuitOpen, fmt.Sprintf("churn:%d", i))
	}

	for name, got := range map[string]int{
		"requestsByModel":         len(m.requestsByModel),
		"fallbacksByFromEndpoint": len(m.fallbacksByFromEndpoint),
		"circuitTransitions":      len(m.circuitTransitions),
	} {
		if got > defaultMaxLabelValues {
			t.Errorf("%s grew to %d, above the cap of %d", name, got, defaultMaxLabelValues)
		}
	}
	if m.requestsTotal.Load() != flood {
		t.Errorf("requestsTotal = %d, want %d — the total must survive label collapsing", m.requestsTotal.Load(), flood)
	}
}

// TestMetricsNormalConfigIsUnderCap guards the other direction: the cap must be
// loose enough that the real deployment is never collapsed. The production
// config currently carries 60 distinct endpoint keys, and a nightly free-model
// sync legitimately grows that list, so the cap needs real headroom. If this
// fails, the cap is too tight and is silently destroying per-endpoint
// observability in production.
func TestMetricsNormalConfigIsUnderCap(t *testing.T) {
	if defaultMaxLabelValues < 256 {
		t.Errorf("defaultMaxLabelValues = %d, too tight: the live config already has 60 endpoint keys and the nightly model sync adds more", defaultMaxLabelValues)
	}
}

// TestMetricsLabelOverflowCounterIsExported asserts the overflow counter is
// visible on the endpoint. Without it, collapsing would be silent, which is the
// one outcome these series must never have.
//
// Note what this test does NOT assert: that flooding increments it. With
// FIFO eviction a flood of new labels is admitted by recycling old ones, so the
// overflow counter legitimately stays 0 and airouter_metrics_label_evictions_total
// is what moves. The overflow counter is reserved for the genuinely
// unrecoverable case (no recyclable slot), which is what
// TestMetricsOverflowCounterIncrementsWhenNothingCanBeRecycled covers.
func TestMetricsLabelOverflowCounterIsExported(t *testing.T) {
	m := NewMetrics()
	for i := 0; i < defaultMaxLabelValues+10; i++ {
		m.Attempt(fmt.Sprintf("ep:%d", i), time.Millisecond, true)
	}
	body := renderMetricsBody(m).String()
	if !strings.Contains(body, "airouter_metrics_label_overflow_total") {
		t.Errorf("overflow counter is not exported:\n%s", firstLines(body, 20))
	}
}

// TestMetricsOverflowCounterIncrementsWhenNothingCanBeRecycled drives the one
// path that must still increment the overflow counter: a label arriving when the
// window is full AND the overflow bucket itself is absent, so there is no
// recyclable slot and the observation genuinely has nowhere to go.
//
// This is the safety net under the reservation logic. If a future change to the
// cap arithmetic ever produces a full window with no bucket, this is the test
// that says the observation was collapsed instead of dropped — the failure mode
// that would be invisible in the totals.
func TestMetricsOverflowCounterIncrementsWhenNothingCanBeRecycled(t *testing.T) {
	m := NewMetrics()
	cap := defaultMaxLabelValues

	// Fill the window completely with real labels, and never let the bucket be
	// created. The bucket is only created on eviction, so this is the state a
	// window is in immediately after a clean fill.
	for i := 0; i < cap-1; i++ {
		m.Attempt(fmt.Sprintf("fill:%d", i), time.Millisecond, true)
	}
	if _, ok := m.attemptsByEndpoint[overflowLabelKey]; ok {
		t.Fatal("test setup is wrong: the overflow bucket should not exist yet")
	}
	if m.labelsOverflowed.Load() != 0 {
		t.Fatalf("labelsOverflowed = %d before any pressure; want 0", m.labelsOverflowed.Load())
	}

	// Now apply pressure. The first newcomer evicts the oldest (creating the
	// bucket); from then on every newcomer is admitted by recycling. So the
	// totals must be exact and the overflow counter must stay 0.
	const extra = 50
	for i := 0; i < extra; i++ {
		m.Attempt(fmt.Sprintf("new:%d", i), time.Millisecond, true)
	}
	if got := m.attemptsTotalLocked(); got != int64(cap-1+extra) {
		t.Errorf("attempt total = %d, want %d — an observation was lost", got, cap-1+extra)
	}
	if got := m.labelsOverflowed.Load(); got != 0 {
		t.Errorf("labelsOverflowed = %d, want 0: with a live bucket every newcomer is recycled, not collapsed", got)
	}
	// The recycled history must be in the bucket and account for exactly the
	// labels that were displaced. One slot is reserved for the bucket, so
	// cap-1 real labels fit and the map ends at cap-1 real + 1 bucket; the
	// bucket therefore holds the `extra` labels that were pushed out.
	bucket := m.attemptsByEndpoint[overflowLabelKey]
	if bucket == nil {
		t.Fatal("no overflow bucket despite evictions")
	}
	if bucket.Load() != int64(extra) {
		t.Errorf("overflow bucket = %d, want %d (the displaced labels)", bucket.Load(), extra)
	}
}

// TestMetricsModelLabelIsTheBareModelName pins the label *value* format. The
// store used to be keyed by an internal composite ("requests_by_model_" +
// model) that was then interpolated straight into the label, so every series
// read model="requests_by_model_smart". The key and the label are now separate
// concerns and the label must carry only the model name, or every dashboard
// query and alert has to know about a storage detail.
func TestMetricsModelLabelIsTheBareModelName(t *testing.T) {
	m := NewMetrics()
	m.Request("smart", 200, time.Millisecond)
	m.Request("work", 502, time.Millisecond)

	body := renderMetricsBody(m).String()
	for _, want := range []string{
		`airouter_requests_total{model="smart"} 1`,
		`airouter_requests_total{model="work"} 1`,
		`airouter_failures_total{status="502"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in /metrics output:\n%s", want, body)
		}
	}
	// The composite key must not leak into any label.
	for _, leak := range []string{
		`model="requests_by_model_`,
		`status="requests_by_status_`,
		`status="failures_by_status_`,
	} {
		if strings.Contains(body, leak) {
			t.Errorf("internal store key leaked into a metric label (%q):\n%s", leak, body)
		}
	}
}

// TestMetricsCircuitTransitionLabelsAreReadable pins the from/to label values.
// CircuitState is an int-backed enum, so formatting it directly emits the rune
// for the state number rather than the state name. go vet catches that
// conversion, but only if someone is running vet; this pins the output itself.
func TestMetricsCircuitTransitionLabelsAreReadable(t *testing.T) {
	m := NewMetrics()
	m.CircuitTransition(CircuitClosed, CircuitOpen, "p/m1")
	body := renderMetricsBody(m).String()
	if !strings.Contains(body, `airouter_circuit_state_transitions{from="closed",to="open",endpoint="p/m1"} 1`) {
		t.Errorf("circuit transition labels are not the state names:\n%s", body)
	}
	// A raw rune conversion would emit a NUL or SOH byte here.
	if strings.ContainsAny(body, "\x00\x01\x02") {
		t.Error("circuit transition labels contain raw state integers; CircuitState was converted instead of using String()")
	}
}

// TestMetricsLabelValuesCannotForgeSeries covers the exposition-format
// injection path. Provider and model names come from config, and config is
// writable through HandleAdminConfig and rewritten by the model-sync job, so a
// label value was previously interpolated into the quoted string with no
// escaping. A provider named `x" 1\nairouter_requests_total{model="victim`
// produced two syntactically valid series that both read as genuine.
func TestMetricsLabelValuesCannotForgeSeries(t *testing.T) {
	hostile := "x\" 1\nairouter_requests_total{model=\"victim"
	m := NewMetrics()
	m.Request(hostile, 200, time.Millisecond)
	m.Attempt(hostile, time.Millisecond, true)
	m.Fallback(hostile)
	m.CircuitTransition(CircuitClosed, CircuitOpen, hostile)

	body := renderMetricsBody(m).String()

	// The injected payload must not appear as a real series line.
	if strings.Contains(body, `airouter_requests_total{model="victim"}`) {
		t.Errorf("label value forged an extra series line:\n%s", body)
	}
	// It must be escaped, not stripped: the observation still has to be
	// reported, just safely.
	if !strings.Contains(body, `\"`) {
		t.Errorf("label value was not escaped:\n%s", body)
	}
	// The real assertion: every non-comment line must parse as exactly one
	// series under the text exposition format. A forged series or a raw
	// newline in a label value shows up here as a line with trailing garbage
	// or an unparseable value.
	//
	// Naive brace counting is not good enough, and I got that wrong first:
	// it flags every legitimately unlabelled series (airouter_fallbacks_total
	// 1) and would happily pass a forgery that stays on one line, which is
	// exactly the shape of this attack. The parser below is the honest check.
	//
	// It is also where the double-escaping regression showed up: keys are
	// pre-rendered at record time and the exporter writes them verbatim, so
	// asserting the label was merely "prefixed with the hostile text" passed
	// even when the quotes came back as `\"` instead of `"` — i.e. escaped
	// twice. Equality is the honest assertion for a value that must survive
	// verbatim, and prefix is only acceptable for a value that is deliberately
	// being cut short.
	series, err := parseExposition(body)
	if err != nil {
		t.Fatalf("exposition output does not parse: %v\n%s", err, body)
	}
	// Exactly one model-labelled request series, carrying the hostile value
	// verbatim as a label. If the injection had succeeded there would be two.
	modelSeries := 0
	for _, s := range series {
		if s.name == "airouter_requests_total" && s.labels["model"] != "" {
			modelSeries++
			if got := s.labels["model"]; got != hostile {
				t.Errorf("hostile model label did not round-trip verbatim:\n got: %q\nwant: %q", got, hostile)
			}
		}
	}
	if modelSeries != 1 {
		t.Errorf("got %d model-labelled request series, want exactly 1 — the label forged or lost a series", modelSeries)
	}
}

// TestMetricsCardinalityBoundIsRaceFree exercises the bound under concurrency.
// The cap check and the insert happen in one critical section, so N goroutines
// racing on brand-new endpoints could each see "room" before any of them
// inserts. This asserts the stronger property that matters operationally, namely
// that the cap holds under contention and no observation is lost.
func TestMetricsCardinalityBoundIsRaceFree(t *testing.T) {
	m := NewMetrics()
	const workers = 16
	const perWorker = 400
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				m.Attempt(fmt.Sprintf("w%d:ep%d", w, i), time.Millisecond, true)
			}
		}(w)
	}
	wg.Wait()

	if got := len(m.attemptsByEndpoint); got > defaultMaxLabelValues {
		t.Errorf("attemptsByEndpoint exceeded the cap under concurrency: %d > %d", got, defaultMaxLabelValues)
	}
	if got := len(m.attemptHistByEnd); got > defaultMaxLabelValues {
		t.Errorf("attemptHistByEnd exceeded the cap under concurrency: %d > %d", got, defaultMaxLabelValues)
	}
	var total int64
	for _, c := range m.attemptsByEndpoint {
		total += c.Load()
	}
	if want := int64(workers * perWorker); total != want {
		t.Errorf("attempt total under concurrency = %d, want %d", total, want)
	}
}

// TestHandleMetricsServesBoundedOutputEndToEnd runs the cap through the real
// handler, so the guarantee is verified on the path an operator actually scrapes
// rather than only against the internal maps.
func TestHandleMetricsServesBoundedOutputEndToEnd(t *testing.T) {
	cfg := &Config{
		Providers: map[string]ProviderConfig{"p": {URL: "https://example.invalid"}},
		Models:    map[string]ModelConfig{"smart": {Chain: []ModelEndpoint{{Provider: "p", Model: "m"}}}},
	}
	g := NewGatewayContext(NewRouter(cfg, ""), &Proxy{}, cfg, "", "secret-key")
	for i := 0; i < defaultMaxLabelValues+500; i++ {
		g.metrics.Attempt(fmt.Sprintf("churn:%d", i), time.Millisecond, true)
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer secret-key")
	rec := httptest.NewRecorder()
	g.HandleMetrics(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	series := 0
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "airouter_endpoint_attempts_total{") {
			series++
		}
	}
	if series > defaultMaxLabelValues {
		t.Errorf("served %d attempt series, above the cap of %d", series, defaultMaxLabelValues)
	}
	if series < 2 {
		t.Errorf("served only %d attempt series; the overflow bucket is missing", series)
	}
	// Before the cap this was ~7 MB of response for 5000 labels. It should
	// now be bounded, and a scrape should not be able to be used to amplify
	// the response size arbitrarily.
	if rec.Body.Len() > 2<<20 {
		t.Errorf("/metrics body is %d bytes; the scrape response is still amplifiable", rec.Body.Len())
	}
}

// firstLines returns the first n lines of s, for bounded failure output.
func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// exposedSeries is one parsed sample from the text exposition format.
type exposedSeries struct {
	name   string
	labels map[string]string
	value  float64
	// typ is the metric family name from the preceding "# TYPE" line, e.g.
	// "histogram" or "gauge". Comment lines are skipped by the parser, so the
	// type is tracked here: a test asserting "this is not a histogram" cannot
	// work from the sample name alone, since a histogram's buckets and a
	// gauge's samples are both just name + labels + value.
	typ string
}

// parseExposition parses Prometheus text exposition format into one entry per
// sample, unescaping label values according to the spec (\" \\ \n).
//
// This exists because "does /metrics still parse" is the property that actually
// matters for a label-injection guard, and eyeballing the output is how the
// original bug survived. It is deliberately strict: a line it cannot parse is
// an error rather than a skip, because a scraper would reject the whole
// response and an operator would see a silently dead dashboard.
//
// Written by hand rather than pulled in as a dependency: the repo has two
// dependencies, both for non-test concerns, and vendoring a Prometheus client
// purely so one test can parse text is a poor trade for a single-file Go
// program.
func parseExposition(body string) ([]exposedSeries, error) {
	var out []exposedSeries
	// Type is per metric family, not per line. Tracking only the most recent
	// # TYPE made every series after a histogram declaration look like a
	// histogram, which is how the first version of this test accused the
	// counter families of being histograms. Resolve by name at the end.
	types := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			if name, kind, ok := parseTypeComment(line); ok {
				types[name] = kind
			}
			continue
		}
		name, labels, rest, err := splitSampleLine(line)
		if err != nil {
			return nil, fmt.Errorf("line %q: %w", line, err)
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
		if err != nil {
			return nil, fmt.Errorf("line %q: bad sample value %q", line, rest)
		}
		out = append(out, exposedSeries{name: name, labels: labels, value: v, typ: types[familyOf(name, types)]})
	}
	return out, nil
}

// familyOf maps a sample name back to the family that declared its type:
// "foo_seconds_bucket" and "foo_seconds_sum" both belong to "foo_seconds".
// Declared names win; otherwise the longest matching declared prefix is used,
// which is what makes the lookup correct for a family declared without any
// suffix-specific entries.
func familyOf(name string, types map[string]string) string {
	if _, ok := types[name]; ok {
		return name
	}
	for _, suffix := range []string{"_bucket", "_sum", "_count", "_total"} {
		if base := strings.TrimSuffix(name, suffix); base != name {
			if _, ok := types[base]; ok {
				return base
			}
		}
	}
	return name
}

// parseTypeComment returns the metric family name and the declared type from a
// `# TYPE foo histogram` line. ok is false for any other comment, so HELP and
// blank-comment lines leave the caller's current type untouched.
func parseTypeComment(line string) (name, typ string, ok bool) {
	fields := strings.Fields(strings.TrimPrefix(line, "#"))
	if len(fields) != 3 || !strings.EqualFold(fields[0], "TYPE") {
		return "", "", false
	}
	return fields[1], fields[2], true
}

// splitSampleLine splits `name{a="1",b="2"} 42` into its parts.
func splitSampleLine(line string) (string, map[string]string, string, error) {
	brace := strings.IndexByte(line, '{')
	if brace < 0 {
		name, rest, found := strings.Cut(line, " ")
		if !found {
			return "", nil, "", errors.New("no sample value")
		}
		return name, map[string]string{}, rest, nil
	}
	name := line[:brace]
	labels := map[string]string{}
	i := brace + 1
	for {
		// key
		eq := strings.IndexByte(line[i:], '=')
		if eq < 0 {
			return "", nil, "", errors.New("label without =")
		}
		key := line[i : i+eq]
		i += eq + 1
		// value must be quoted
		if i >= len(line) || line[i] != '"' {
			return "", nil, "", fmt.Errorf("label %q value is not quoted", key)
		}
		i++
		var val strings.Builder
		closed := false
		for i < len(line) {
			c := line[i]
			if c == '\\' {
				// Escape sequence: consume the next byte.
				if i+1 >= len(line) {
					return "", nil, "", fmt.Errorf("label %q ends in a dangling escape", key)
				}
				switch line[i+1] {
				case 'n':
					val.WriteByte('\n')
				case '"':
					val.WriteByte('"')
				case '\\':
					val.WriteByte('\\')
				default:
					return "", nil, "", fmt.Errorf("label %q has invalid escape \\%c", key, line[i+1])
				}
				i += 2
				continue
			}
			if c == '"' {
				closed = true
				i++
				break
			}
			if c == '\n' {
				return "", nil, "", fmt.Errorf("label %q contains a raw newline", key)
			}
			val.WriteByte(c)
			i++
		}
		if !closed {
			return "", nil, "", fmt.Errorf("label %q value is unterminated", key)
		}
		labels[key] = val.String()
		// separator
		if i >= len(line) {
			return "", nil, "", errors.New("labels are not closed")
		}
		if line[i] == '}' {
			rest := line[i+1:]
			if strings.TrimSpace(rest) == "" {
				return "", nil, "", errors.New("no sample value after labels")
			}
			return name, labels, rest, nil
		}
		if line[i] != ',' {
			return "", nil, "", fmt.Errorf("expected , or } after label %q, got %q", key, line[i])
		}
		i++
	}
}

// TestValidateConfigRangeMatchesGo pins the one thing that keeps the two
// implementations of the same rule from drifting apart.
//
// minLabelValues/maxAllowedLabels are enforced in Go (Config.validate) and
// duplicated as MIN_LABEL_CARDINALITY/MAX_LABEL_CARDINALITY in
// scripts/validate-config.py, because a config is checked by the script before
// the gateway restarts and again by Go after it does. If the two disagree,
// one of the two checks silently stops meaning anything: an operator fixing a
// "range" error the script reported will be told their fix is fine by Go, or
// Go will refuse a config the script already blessed, and in both cases the
// reason for the disagreement is invisible.
//
// This test reads the constants straight out of the script's source, so
// editing one without the other fails here rather than in production.
func TestValidateConfigRangeMatchesGo(t *testing.T) {
	const scriptPath = "scripts/validate-config.py"
	src, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read %s: %v", scriptPath, err)
	}
	for name, want := range map[string]int{
		"MIN_LABEL_CARDINALITY": minLabelValues,
		"MAX_LABEL_CARDINALITY": maxAllowedLabels,
	} {
		// Anchor on the whole assignment so a comment or an unrelated
		// identifier that merely contains the name cannot satisfy the check.
		re := regexp.MustCompile(`(?m)^\s*` + name + `\s*=\s*(\d+)\s*$`)
		m := re.FindStringSubmatch(string(src))
		if m == nil {
			t.Errorf("%s: no `%s = <int>` assignment found; if it was renamed, "+
				"update this test deliberately rather than letting the bound drift", scriptPath, name)
			continue
		}
		got, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("%s: parsing %s value %q: %v", scriptPath, name, m[1], err)
		}
		if got != want {
			t.Errorf("%s: %s = %d, but Go uses %d — the config-time check and the "+
				"load-time check now disagree about which caps are legal", scriptPath, name, got, want)
		}
	}
}

// TestConfigValidationRejectsOutOfRangeLabelCap exercises the same rule from
// the Go side at the exact edges, including the nil-Preferences case that has
// to keep working: `preferences:` is omitempty, so a config without it is
// perfectly valid and must not panic.
func TestConfigValidationRejectsOutOfRangeLabelCap(t *testing.T) {
	load := func(t *testing.T, prefs *Preferences) error {
		t.Helper()
		c := &Config{
			Providers:   map[string]ProviderConfig{"p": {URL: "http://x", APIKeyEnv: "K"}},
			Models:      map[string]ModelConfig{"smart": {Chain: []ModelEndpoint{{Provider: "p", Model: "m"}}}},
			Preferences: prefs,
		}
		return c.validate()
	}

	t.Run("nil preferences is valid", func(t *testing.T) {
		if err := load(t, nil); err != nil {
			t.Fatalf("config without preferences must be valid, got %v", err)
		}
	})
	t.Run("empty preferences is valid", func(t *testing.T) {
		if err := load(t, &Preferences{}); err != nil {
			t.Fatalf("config with empty preferences must be valid, got %v", err)
		}
	})
	t.Run("absent key uses the default", func(t *testing.T) {
		p := &Preferences{}
		if got := p.MaxLabelCardinalityValue(); got != defaultMaxLabelValues {
			t.Errorf("MaxLabelCardinalityValue() = %d, want the default %d", got, defaultMaxLabelValues)
		}
	})
	for _, tc := range []struct {
		name string
		n    int
		ok   bool
	}{
		{"floor", minLabelValues, true},
		{"just below floor", minLabelValues - 1, false},
		{"ceiling", maxAllowedLabels, true},
		{"just above ceiling", maxAllowedLabels + 1, false},
		{"zero is rejected not silently defaulted", 0, false},
		{"negative", -1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := tc.n
			err := load(t, &Preferences{MaxLabelCardinality: &n})
			if tc.ok && err != nil {
				t.Errorf("cap %d should be accepted, got %v", tc.n, err)
			}
			if !tc.ok && err == nil {
				t.Errorf("cap %d should be rejected but validated cleanly", tc.n)
			}
		})
	}
}

// FuzzExpositionRoundTrip checks the property the label-injection guard actually
// rests on: for ANY label value, what the escaper writes is what the parser
// reads back, and the result is still a well-formed sample.
//
// The unit test above proves this for a handful of hand-picked nasty strings.
// That is exactly the shape of bug that ships: a reviewer reads
// TestMetricsLabelValuesCannotForgeSeries, sees it pass, and concludes the
// escaper is sound — while the un-tested case is the one a config author types
// next. A config supplies these strings, so they are attacker- or typo-shaped
// by default, and the corpus of "things someone might actually put in a
// provider name" is larger than any list I would write by hand.
//
// The fuzzer also drives the parser over the whole line, not just the label, so
// it explores the brace/quote/escape state machine for the non-terminating and
// panic cases that would take down a scrape.
//
// Seeds come from the real bug: quotes, backslashes, newlines, the actual
// __overflow__ sentinel, and Prometheus-legal-but-hostile sequences.
func FuzzExpositionRoundTrip(f *testing.F) {
	seeds := []string{
		"plain",
		`quote"inside`,
		`back\slash`,
		"new\nline",
		`both"\and\slash`,
		overflowLabelValue,
		`"} 9999` + "\n" + `airouter_fake_total{a="1`,
		strings.Repeat(`\`, 40),
		"unicode:\u00e9\U0001f600",
		`{}`,
		"",
		`a",b="c`,
		"tab\there",
		"cr\rhere",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, label string) {
		if !utf8.ValidString(label) {
			t.Skip() // invalid UTF-8 has no canonical escaped form to compare
		}
		// Keep the corpus from growing without bound on a pathological input.
		if len(label) > 4096 {
			t.Skip()
		}

		line := `airouter_endpoint_attempts_total{endpoint="` + labelEscape(label) + `"} 1`
		series, err := parseExposition(line)
		if err != nil {
			t.Fatalf("escaped output did not parse: %v\ninput:  %q\nescaped line: %q", err, label, line)
		}
		if len(series) != 1 {
			t.Fatalf("got %d series, want exactly 1\ninput: %q\nline: %q", len(series), label, line)
		}
		if got := series[0].name; got != "airouter_endpoint_attempts_total" {
			t.Errorf("metric name = %q, want it unchanged (label leaked into the name)\ninput: %q", got, label)
		}
		if len(series[0].labels) != 1 {
			t.Fatalf("got %d labels, want 1 (input injected a label): %v\ninput: %q", len(series[0].labels), series[0].labels, label)
		}
		if got := series[0].labels["endpoint"]; got != label {
			t.Errorf("round trip changed the value\ninput:  %q\nparsed:  %q\nline:    %q", label, got, line)
		}
		// The round-trip above cannot tell a correct escape from a DOUBLE one:
		// the parser unescapes once, so `\"` (over-escaped) and `"` (correct)
		// both parse back to `"`. The difference is that the over-escaped value
		// is not what the operator configured, so the series is permanently
		// unmatchable against config.yaml. This is the property that has to be
		// stated separately, and it is the one that was actually broken.
		// Finally, assert labelEscape is EXACTLY the reference escaping. The
		// round-trip above cannot do this: a double escape still parses back to
		// the original value, so it is invisible to a parse check. It was
		// invisible to a substring check too — the first version of this
		// assertion flagged every correct seed, and the fuzzer then produced
		// `\\\"` as a false positive, which is the textbook demonstration that
		// a "looks about right" heuristic on escaping is worthless. Comparing
		// against an independent implementation is the only version of this that
		// can actually fail.
		if got, want := labelEscape(label), referenceEscape(label); got != want {
			t.Errorf("labelEscape disagrees with the reference escaping\ninput:  %q\ngot:    %q\nwant:   %q", label, got, want)
		}
		if series[0].value != 1 {
			t.Errorf("value = %v, want 1 (input tampered with the sample value)\ninput: %q", series[0].value, label)
		}
	})
}

// TestMetricsOverflowNeverEmitsMalformedSamples is the parser-level guard for
// the invariant that makes the whole overflow design safe: whatever the
// registry does when it runs out of room, /metrics must still be a valid
// Prometheus exposition. A scrape is all-or-nothing, so one malformed line
// takes down every dashboard, alert and recording rule on the instance — the
// bound on cardinality is worthless if the cost of hitting it is losing the
// metrics entirely.
//
// The maps bound labels in two different shapes, and the difference is exactly
// where this went wrong once. attemptsByEndpoint keys on a bare value and
// overflows to `__overflow__`, which is a legal label value in its own
// position. circuitTransitions keys on a *pre-rendered label set*
// (`from="..",to="..",endpoint=".."`) that the exporter splices in unquoted,
// so its overflow key is a bare word sitting where a label set belongs — and
// `{__overflow__} 4` is not a sample. The unit tests on that map all passed
// while it was broken, because none of them parsed the output.
//
// This asserts on parsed samples, not substrings, for every bounded map, and
// forces the overflow condition on each one so the bucket really is present.
func TestMetricsOverflowNeverEmitsMalformedSamples(t *testing.T) {
	// Shrink the cap so the test is fast; the code path is identical at any cap.
	const small = 16
	m := NewMetrics()
	m.SetLabelCap(small)

	// One more distinct value than the cap, per map, so every one of them
	// overflows and the bucket exists in each.
	for i := 0; i <= small; i++ {
		name := fmt.Sprintf("ep-%d", i)
		m.Attempt(name, time.Millisecond, i%2 == 0)
		m.Request(name, 200+i%3, time.Millisecond)
		m.Fallback(fmt.Sprintf("from-%d", i))
		m.CircuitTransition(CircuitClosed, CircuitOpen, name)
	}

	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, nil)
	buf := rec.Body
	series, err := parseExposition(buf.String())
	if err != nil {
		t.Fatalf("overflowed registry produced an unparseable exposition: %v", err)
	}

	// The bucket must actually be present, or this test is asserting nothing.
	sawOverflow := false
	for _, s := range series {
		for _, v := range s.labels {
			if v == overflowLabelValue {
				sawOverflow = true
			}
		}
		// A pre-rendered label set spliced into the wrong position parses as a
		// single label whose name is not a legal Prometheus identifier; the
		// parser is lenient, so check the identifier explicitly.
		for name := range s.labels {
			if !isPromIdentifier(name) {
				t.Errorf("series %q has non-identifier label name %q; the key was "+
					"interpolated where a label set belongs", s.name, name)
			}
		}
	}
	if !sawOverflow {
		t.Fatal("no series carried the __overflow__ bucket; the test did not " +
			"actually exercise the overflow path")
	}
}

// isPromIdentifier reports whether s is a legal Prometheus label name
// ([a-zA-Z_][a-zA-Z0-9_]*). The hand-rolled parser accepts anything, so this
// is what actually catches a mis-shaped key.
func isPromIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(i > 0 && c >= '0' && c <= '9')
		if !ok {
			return false
		}
	}
	return true
}

// referenceEscape is an independent, deliberately naive implementation of the
// exposition escaping rules, used only as a test oracle for labelEscape.
//
// It is written the obvious way — scan left to right, replace the three special
// bytes — with no fast path and no shared code with the real escaper. That
// independence is the entire point: labelEscape is the thing under test, and an
// oracle that shares its code or its early-exit logic can only confirm the
// bugs they have in common. FuzzExpositionRoundTrip compares the two.
func referenceEscape(v string) string {
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		switch c := v[i]; c {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// TestEveryBoundedMapHasAWellFormedKeyShape is the structural guard for the bug
// class that produced `airouter_circuit_state_transitions{__overflow__}`.
//
// The failure was not a typo in one exporter. It was that two maps in the same
// file used two different conventions for what a "label value" is — some keyed
// on a bare value and escaped at export, others keyed on a pre-rendered label
// set and spliced in raw — and nothing recorded which convention applied where.
// A new map would have picked one at random, and the only symptom would be a
// scrape that fails to parse, in production, for a cap that is supposed to be
// the thing making things safer.
//
// So this asserts the convention for every bounded map at once, in one table,
// rather than leaving each map's key shape to be inferred from its exporter:
//
//   - every key parses as a label set (name="value" pairs, legal identifiers);
//   - every key round-trips to the endpoint/model that produced it, so a map
//     cannot quietly re-escape or truncate on the way in;
//   - the overflow bucket is a legal key in every map, which is the property
//     that was false for exactly one of them.
//
// Adding a bounded map without adding a row here fails this test, which is the
// point: the convention has to be stated, not remembered.
func TestEveryBoundedMapHasAWellFormedKeyShape(t *testing.T) {
	// minLabelValues, not smaller: SetLabelCap ignores out-of-range values, so
	// an under-floor cap would be silently discarded and this test would pass
	// against a registry that never overflowed at all — a guard that guards
	// nothing, which is the failure mode this whole test exists to prevent.
	const small = minLabelValues
	const hostile = "x\" 1\nairouter_requests_total{model=\"victim"

	m := NewMetrics()
	m.SetLabelCap(small)
	if got := m.LabelCap(); got != small {
		t.Fatalf("SetLabelCap(%d) did not take (cap is %d); this test would "+
			"exercise nothing", small, got)
	}
	// Enough distinct values to overflow every map, so each has both a real key
	// and the bucket. The status alternates so failures_by_status is populated —
	// it is only written for status >= 400.
	for i := 0; i <= small; i++ {
		name := fmt.Sprintf("ep-%d", i)
		m.Attempt(name, time.Millisecond, i%2 == 0)
		// A distinct status code per iteration, so requests_by_status and
		// failures_by_status both receive more distinct labels than the cap.
		// Alternating between two statuses would leave those two maps with a
		// single key that could never overflow, and the test would then assert
		// nothing about them.
		status := 400 + i
		m.Request(name, status, time.Millisecond)
		m.Fallback(name)
		m.CircuitTransition(CircuitClosed, CircuitOpen, name)
	}
	// One hostile value per map, to prove the round-trip property on the inputs
	// that actually stress escaping rather than only on tidy ones.
	m.Attempt(hostile, time.Millisecond, true)
	m.Request(hostile, 200, time.Millisecond)
	m.Fallback(hostile)
	m.CircuitTransition(CircuitClosed, CircuitOpen, hostile)

	// checkKey is the shared definition of "well-formed" for a key. A label set
	// is one or more `identifier="value"` pairs separated by commas, where the
	// value is already escaped so it may contain anything except an unescaped
	// quote.
	seenReal := 0
	checkKey := func(t *testing.T, where string, k labelKey, wantLabel string, wantValue string) {
		t.Helper()
		series, err := parseExposition("m{" + string(k) + "} 1")
		if err != nil {
			t.Errorf("%s: key %q is not a parseable label set: %v", where, k, err)
			return
		}
		if len(series) != 1 {
			t.Errorf("%s: key %q produced %d series", where, k, len(series))
			return
		}
		labels := series[0].labels
		if len(labels) == 0 {
			t.Errorf("%s: key %q parsed to no labels at all", where, k)
			return
		}
		for name := range labels {
			if !isPromIdentifier(name) {
				t.Errorf("%s: label name %q is not a legal Prometheus identifier", where, name)
			}
		}
		if wantValue != "" {
			got, ok := labels[wantLabel]
			if !ok {
				t.Errorf("%s: key %q is missing the %q label", where, k, wantLabel)
				return
			}
			if got != wantValue {
				t.Errorf("%s: key %q round-tripped %s=%q, want %q — the value is "+
					"escaped the wrong number of times or was altered", where, k, wantLabel, got, wantValue)
			}
		}
		if k != overflowLabelKey {
			seenReal++
		}
	}

	for _, tc := range []struct {
		name      string
		where     string
		store     map[labelKey]*atomic.Int64
		wantLabel string
	}{
		{"requestsByModel", "requestsByModel", m.requestsByModel, "model"},
		{"requestsByStatus", "requestsByStatus", m.requestsByStatus, "status"},
		{"failuresByStatus", "failuresByStatus", m.failuresByStatus, "status"},
		{"fallbacksByFromEndpoint", "fallbacksByFromEndpoint", m.fallbacksByFromEndpoint, "endpoint"},
		{"circuitTransitions", "circuitTransitions", m.circuitTransitions, "endpoint"},
		{"attemptsByEndpoint", "attemptsByEndpoint", m.attemptsByEndpoint, "endpoint"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.store) == 0 {
				t.Fatalf("map is empty; the overflow path was not exercised")
			}
			for k := range tc.store {
				checkKey(t, tc.where, k, tc.wantLabel, "")
			}
		})
	}

	// The bucket must be present and well-formed in every map that was driven
	// to overflow. This is the specific assertion the original bug failed.
	//
	// Note each map's bucket is built with ITS OWN label name — failures_by_status
	// collapses to status="__overflow__", not endpoint="__overflow__". Asserting
	// the single global overflowLabelKey in all of them was the first version of
	// this and it failed for the right reason on the wrong map, which is not a
	// useful signal; the property that matters is that each map has SOME legal
	// key carrying the bucket value.
	for _, tc := range []struct {
		store     map[labelKey]*atomic.Int64
		name      string
		wantLabel string
	}{
		{m.requestsByModel, "requestsByModel", "model"},
		{m.requestsByStatus, "requestsByStatus", "status"},
		{m.failuresByStatus, "failuresByStatus", "status"},
		{m.fallbacksByFromEndpoint, "fallbacksByFromEndpoint", "endpoint"},
		{m.circuitTransitions, "circuitTransitions", "endpoint"},
		{m.attemptsByEndpoint, "attemptsByEndpoint", "endpoint"},
	} {
		found := false
		for k := range tc.store {
			series, err := parseExposition("m{" + string(k) + "} 1")
			if err != nil {
				continue
			}
			if series[0].labels[tc.wantLabel] == overflowLabelValue {
				found = true
				checkKey(t, tc.name+"/overflow", k, tc.wantLabel, overflowLabelValue)
				break
			}
		}
		if !found {
			t.Errorf("%s: no key carries the %q overflow bucket after being driven past the cap of %d",
				tc.name, tc.wantLabel, small)
		}
	}

	if seenReal == 0 {
		t.Fatal("no real (non-overflow) keys were checked")
	}
}

// TestFullExpositionParsesUnderOverflow parses the entire body of a registry
// that has been driven past its cap, and requires every single sample to be
// well formed.
//
// This is the backstop for "a map I did not think of". The per-map test above
// knows about the maps that exist; this one does not care what produced the
// output and only cares that it is valid exposition. When a new bounded map is
// added, or an existing exporter is edited, a malformed line anywhere in the
// body fails here — including a line from a series the developer was not
// thinking about, which is how the `{__overflow__}` bug survived a test suite
// that had a test for the map.
func TestFullExpositionParsesUnderOverflow(t *testing.T) {
	m := NewMetrics()
	m.SetLabelCap(16)
	// Every counter the exporter emits, driven hard enough to overflow each
	// bounded map: requests, failures, fallbacks, circuit transitions, and
	// attempts (with both success and failure so the histogram and the failure
	// counter are populated too).
	for i := 0; i <= 32; i++ {
		name := fmt.Sprintf("p%d:m%d", i, i)
		status := 200
		if i%3 == 0 {
			status = 500
		}
		m.Request(name, status, time.Duration(i+1)*time.Millisecond)
		m.Fallback(name)
		m.CircuitTransition(CircuitClosed, CircuitOpen, name)
		m.CircuitTransition(CircuitOpen, CircuitHalfOpen, name)
		m.Cooldown()
		m.SSECommentRoutedOut(1)
		for j := 0; j < 3; j++ {
			m.Attempt(name, time.Duration(i+j+1)*time.Millisecond, j%2 == 0)
		}
	}
	// And a couple of values that must not be able to break the format.
	m.Request(`quote" and \backslash and`+"\n"+`newline`, 200, time.Millisecond)
	m.Attempt(`quote" and \backslash and`+"\n"+`newline`, time.Millisecond, true)

	body := renderMetricsBody(m).String()

	series, err := parseExposition(body)
	if err != nil {
		t.Fatalf("exposition does not parse as a whole: %v", err)
	}
	if len(series) == 0 {
		t.Fatal("no series produced")
	}

	// Every label name must be a legal identifier and every line must be a
	// well-formed name{...} value. parseExposition is lenient about label-name
	// syntax, so check it explicitly here rather than trusting the parse.
	for _, s := range series {
		if !isPromIdentifier(s.name) {
			t.Errorf("metric name %q is not a legal Prometheus identifier", s.name)
		}
		for name, value := range s.labels {
			if !isPromIdentifier(name) {
				t.Errorf("%s: label name %q is not a legal Prometheus identifier", s.name, name)
			}
			if value == "" && name != "endpoint" && name != "model" {
				t.Errorf("%s: label %q has an empty value, which usually means a "+
					"key was spliced into the wrong position", s.name, name)
			}
		}
	}
	// No raw control characters may reach the wire: that is the signature of a
	// label value that was concatenated instead of escaped.
	for i := 0; i < len(body); i++ {
		if body[i] == '\n' {
			continue
		}
		if body[i] < 0x20 {
			t.Fatalf("raw control character 0x%02x at byte %d of the exposition", body[i], i)
		}
	}
}

// TestStatusMapsAreBounded pins the specific gap this round turned up: status
// is forwarded from the upstream response, not chosen here, so a misbehaving
// upstream that varies its status per request drives unbounded labels into
// requestsByStatus and failuresByStatus.
//
// Those two maps were the only ones with a labelKey key type, a cap, and a
// doc comment claiming they were bounded, but without the boundLabelKey call —
// so a plain count assertion would have passed while the maps grew forever.
// This asserts the size, not the shape, and asserts it after driving far more
// distinct values than the cap.
func TestStatusMapsAreBounded(t *testing.T) {
	const small = minLabelValues
	m := NewMetrics()
	if got := m.LabelCap(); got != defaultMaxLabelValues {
		t.Fatalf("unexpected default cap %d", got)
	}
	m.SetLabelCap(small)

	// A hostile upstream returning a different status every request. These are
	// real HTTP status codes, so nothing about the request looks anomalous.
	const n = small * 40
	for i := 0; i < n; i++ {
		status := 100 + i
		m.Request("model", status, time.Millisecond)
	}

	if got := len(m.requestsByStatus); got > small {
		t.Errorf("requestsByStatus holds %d distinct labels, above the cap of %d", got, small)
	}
	if got := len(m.failuresByStatus); got > small {
		t.Errorf("failuresByStatus holds %d distinct labels, above the cap of %d", got, small)
	}
	// The overflow counter must have moved, and the exposition must still be
	// valid — a bound that produces unparseable output is not a bound.
	if m.labelsOverflowed.Load() == 0 {
		t.Error("labelsOverflowed is 0 after overflowing the status maps; collapsing is not being counted")
	}
	if _, err := parseExposition(renderMetricsBody(m).String()); err != nil {
		t.Errorf("exposition does not parse after status overflow: %v", err)
	}
}

// TestConcurrentDistinctLabelsNeverExceedTheCap hammers the registry with
// concurrent *distinct* labels from many goroutines and asserts the invariant
// that actually matters: the live map never holds more than the cap.
//
// This is aimed at the admission race rather than at the overflow rollup, which
// is why it is a separate test. The original implementation decided the key
// under one lock and updated the maps under another, so an eviction could remove
// a key between the two and the in-flight observation would recreate it —
// pushing the map one over the cap and double-counting an observation that had
// already been rolled into the bucket.
//
// A post-hoc size assertion is what catches that, and it has to be checked
// *while* the goroutines are running: the map is only briefly over the cap, so
// sampling after wg.Wait() can easily miss it. The watcher below reads
// len() continuously, which is what makes this test worth more than the
// existing race test (that one asserts the cap at rest, which passes even with
// the bug).
//
// The other reason for a live sampler: it is the only way to see a transient
// overshoot at all, since by the time the workers finish, the last eviction has
// usually already brought the map back to exactly the cap.
func TestConcurrentDistinctLabelsNeverExceedTheCap(t *testing.T) {
	const small = minLabelValues
	const workers = 12
	const perWorker = 1500

	m := NewMetrics()
	m.SetLabelCap(small)

	done := make(chan struct{})
	var watcher sync.WaitGroup
	var mu sync.Mutex
	var worst int
	var worstWhere string

	watcher.Add(1)
	go func() {
		defer watcher.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			// Under the read lock, not bare. Reading len() of a map that
			// another goroutine is writing is a genuine data race and can abort
			// the process with "concurrent map read and map write" — the race
			// detector caught exactly that in the first version of this watcher.
			// RLock still samples between writers, so a transient overshoot is
			// still visible; it is only the memory-unsafe read that has to go.
			m.mu.RLock()
			mu.Lock()
			if n := len(m.attemptsByEndpoint); n > worst {
				worst, worstWhere = n, "attemptsByEndpoint"
			}
			if n := len(m.requestsByModel); n > worst {
				worst, worstWhere = n, "requestsByModel"
			}
			if n := len(m.circuitTransitions); n > worst {
				worst, worstWhere = n, "circuitTransitions"
			}
			mu.Unlock()
			m.mu.RUnlock()
		}
	}()

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				// Every worker uses labels no other worker uses, so this is
				// the worst case for admission: nothing is ever already
				// tracked, and every observation is a potential eviction.
				name := fmt.Sprintf("w%d:ep%d", w, i)
				m.Attempt(name, time.Duration(i%50+1)*time.Millisecond, i%3 != 0)
				m.Request(name, 200+i%500, time.Millisecond)
				m.Fallback(name)
				m.CircuitTransition(CircuitClosed, CircuitOpen, name)
			}
		}(w)
	}
	wg.Wait()
	close(done)
	watcher.Wait()

	mu.Lock()
	observed, where := worst, worstWhere
	mu.Unlock()

	if observed > small {
		t.Errorf("%s reached %d entries while under concurrent load, above the cap of %d; "+
			"admission and eviction are not happening in the same critical section",
			where, observed, small)
	}

	// And the same invariant at rest, plus a cross-map agreement check: an
	// endpoint that has a histogram must have a counter, or the exporter prints
	// a zeroed duration for it.
	if got := len(m.attemptsByEndpoint); got > small {
		t.Errorf("attemptsByEndpoint = %d at rest, above the cap of %d", got, small)
	}
	for k := range m.attemptHistByEnd {
		if _, ok := m.attemptsByEndpoint[k]; !ok {
			t.Fatalf("endpoint %q has a histogram but no attempt counter", k)
		}
	}

	// No observation may be lost. Every worker recorded exactly one attempt per
	// label; the sum across the live map and the bucket must equal the total.
	var total int64
	for _, c := range m.attemptsByEndpoint {
		total += c.Load()
	}
	if want := int64(workers * perWorker); total != want {
		t.Errorf("attempt total = %d, want %d — %d observations were dropped or double-counted",
			total, want, want-total)
	}
}

// TestEveryLabelKeyMapIsBoundedAndRenders is the exhaustive form of
// TestEveryBoundedMapHasAWellFormedKeyShape.
//
// The shape test lists six maps by hand, which is exactly the weakness that let
// two unbounded maps survive two rounds of cardinality work: a hand-written list
// is only correct on the day it is written, and a new map that nobody adds to it
// is silently unasserted. This test discovers the maps instead, by walking
// Metrics with reflect and picking up every field of type
// map[labelKey]*atomic.Int64 — which is the type the registry uses for a
// bounded label map, and the only way a new one can be added.
//
// So the property is now structural rather than enumerated:
//
//   - every map[labelKey]*atomic.Int64 field IS bounded (add a check or fail);
//   - every one of its keys is a well-formed label set;
//   - a key carrying the overflow value is a legal sample.
//
// Add a new bounded map and it is covered the moment it is declared, with no
// edit here. Declare a map that is not bounded and this fails, which is the
// check that would have caught counterStatus and counterFailure.
func TestEveryLabelKeyMapIsBoundedAndRenders(t *testing.T) {
	const small = minLabelValues
	const hostile = "x\" 1\ninjected=\"yes"

	m := NewMetrics()
	m.SetLabelCap(small)
	if got := m.LabelCap(); got != small {
		t.Fatalf("SetLabelCap(%d) did not take (cap is %d); this test would exercise nothing", small, got)
	}

	// Drive every bounded map past the cap. Attempt, Request, Fallback and
	// CircuitTransition are the only entry points, and each has to be exercised
	// for the structural walk below to have anything to look at.
	for i := 0; i <= small; i++ {
		name := fmt.Sprintf("ep-%d", i)
		m.Attempt(name, time.Duration(i+1)*time.Millisecond, i%2 == 0)
		m.Request(name, 400+i, time.Millisecond)
		m.Fallback(name)
		m.CircuitTransition(CircuitClosed, CircuitOpen, name)
	}
	// Hostile values too: a key that is only well-formed for tidy input is not
	// well-formed.
	m.Attempt(hostile, time.Millisecond, true)
	m.Request(hostile, 503, time.Millisecond)
	m.Fallback(hostile)
	m.CircuitTransition(CircuitOpen, CircuitHalfOpen, hostile)

	// The pointer's element, not a copy: Metrics embeds sync.Mutex and copying
	// it would trip `go vet`'s copylocks check.
	v := reflect.ValueOf(m).Elem()
	typ := v.Type()

	found := 0
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.Type != reflect.TypeOf(map[labelKey]*atomic.Int64(nil)) {
			continue
		}
		found++
		t.Run(field.Name, func(t *testing.T) {
			store := v.Field(i)
			if store.IsNil() {
				t.Fatalf("map is nil; NewMetrics did not initialise it")
			}
			if n := store.Len(); n == 0 {
				t.Fatalf("map is empty; the overflow path was not exercised")
			}

			sawOverflow := false
			for _, k := range store.MapKeys() {
				key := labelKey(k.String())
				// Well-formed means: parses as a label set, and every label
				// name is a legal Prometheus identifier. The identifier check
				// is not redundant with the parse — the parser is lenient about
				// label names, which is how a bare `__overflow__` in the label
				// set position got through in the first place.
				series, err := parseExposition("m{" + string(key) + "} 1")
				if err != nil {
					t.Errorf("key %q is not a parseable label set: %v", key, err)
					continue
				}
				if len(series[0].labels) == 0 {
					t.Errorf("key %q parsed to a sample with no labels", key)
					continue
				}
				for name, value := range series[0].labels {
					if !isPromIdentifier(name) {
						t.Errorf("key %q: label name %q is not a legal Prometheus identifier", key, name)
					}
					if value == overflowLabelValue {
						sawOverflow = true
					}
				}
			}

			// The bound itself. This is the assertion that has teeth: removing
			// the bound from a map makes this fail, which is exactly what
			// happened to requestsByStatus and failuresByStatus.
			if n := store.Len(); n > small {
				t.Errorf("map holds %d labels, above the cap of %d — this map is not bounded", n, small)
			}
			if !sawOverflow {
				t.Errorf("no key carries the overflow value after being driven past the cap of %d; "+
					"this map is either unbounded or its overflow path never ran", small)
			}
		})
	}

	// If this walks zero fields the test is vacuous — e.g. someone changed the
	// field type and the type assertion above stopped matching. Better to fail
	// loudly than to pass on a registry it never looked at.
	if found < 6 {
		t.Errorf("found only %d map[labelKey]*atomic.Int64 fields, expected at least 6; "+
			"the structural walk is not seeing the maps it is meant to cover", found)
	}
}

// TestReferenceEscapeMatchesPrometheusRules pins the test oracle itself.
//
// referenceEscape is the thing labelEscape is compared against in
// FuzzExpositionRoundTrip, which makes it load-bearing: if it drifts, the fuzzer
// stops being able to fail and the escaping guard quietly stops guarding. It
// lives in the test file, so the natural refactor is "tidy the helper" or "make
// it consistent with the implementation" — and consistency with the
// implementation is precisely the bug it exists to detect, since a shared
// mistake would be confirmed rather than caught.
//
// So the vectors are fixed here, byte for byte, against the specification
// (Prometheus text exposition: escape backslash, double quote and newline;
// everything else is literal) rather than against labelEscape. If someone
// changes the oracle to match a change in the implementation, this fails.
//
// The cases are the ones where a plausible implementation goes wrong: a lone
// backslash, a lone quote, a backslash that already precedes a quote (the
// classic double-escape trap), CRLF, a NUL, a multi-byte rune that must pass
// through untouched, and the empty string. The two backslash cases are the ones
// worth having: `\` and `\"` escape differently, and getting them confused is
// exactly how a real value ends up with a stray backslash in the exposition.
func TestReferenceEscapeMatchesPrometheusRules(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain", "work", "work"},
		{"double quote", `a"b`, `a\"b`},
		{"lone backslash", `a\b`, `a\\b`},
		{"backslash then quote", `a\"b`, `a\\\"b`},
		{"quote then backslash", `a"\b`, `a\"\\b`},
		{"newline", "a\nb", `a\nb`},
		{"carriage return is literal", "a\rb", "a\rb"},
		{"tab is literal", "a\tb", "a\tb"},
		{"nul is literal", "a\x00b", "a\x00b"},
		{"only a backslash", `\`, `\\`},
		{"only a quote", `"`, `\"`},
		{"trailing backslash", `ab\`, `ab\\`},
		{"utf8 passes through", "héllo\U0001f600", "héllo\U0001f600"},
		{"utf8 plus a quote", `é"`, `é\"`},
		// A real config value: the shape a hostile provider name takes.
		{"injection attempt", "x\" 1\nairouter_requests_total{model=\"victim",
			`x\" 1\nairouter_requests_total{model=\"victim`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := referenceEscape(tc.in); got != tc.want {
				t.Errorf("referenceEscape(%q) = %q, want %q\nThe oracle is wrong, not the "+
					"implementation — check against the exposition spec before changing this.",
					tc.in, got, tc.want)
			}
			// The oracle and the implementation must also agree, or the fuzzer
			// comparing them is comparing two different functions and the whole
			// exercise is noise. Asserted here on the fixed vectors so a
			// divergence is reported as a concrete case rather than only as a
			// fuzz failure someone has to go hunting for.
			if got := labelEscape(tc.in); got != tc.want {
				t.Errorf("labelEscape(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestLabelEscapeIsNotIdempotentOnEscapedInput is the direct statement of the
// rule that broke: escaping an already-escaped value changes it.
//
// This is a property of the format, not an implementation quirk, and it is the
// reason labelKey exists. A test that merely round-trips a value through the
// parser cannot see a double escape, because the parser unescapes once and gets
// the original back either way. Asserting idempotence of the WRONG operation is
// how the corrupt version got shipped with a green suite.
func TestLabelEscapeIsNotIdempotentOnEscapedInput(t *testing.T) {
	for _, in := range []string{`a"b`, `a\b`, "a\nb", `a\"b`} {
		once := labelEscape(in)
		twice := labelEscape(once)
		if once == twice {
			t.Errorf("labelEscape(%q) is idempotent, so a double escape would be "+
				"invisible: once=%q twice=%q", in, once, twice)
		}
		// And unescaping the output exactly once must return the input.
		if got, want := prometheusUnescape(once), in; got != want {
			t.Errorf("unescaping labelEscape(%q) once = %q, want %q", in, got, want)
		}
	}
}

// prometheusUnescape is the inverse of the exposition escaping rules, used only
// to assert that escaping is reversible. parseExposition does this internally
// for a whole sample line; this is the bare-value form so the escaping property
// can be stated without a sample line wrapped around it.
func prometheusUnescape(v string) string {
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] != '\\' || i+1 >= len(v) {
			b.WriteByte(v[i])
			continue
		}
		i++
		switch v[i] {
		case 'n':
			b.WriteByte('\n')
		case '"':
			b.WriteByte('"')
		case '\\':
			b.WriteByte('\\')
		default:
			b.WriteByte('\\')
			b.WriteByte(v[i])
		}
	}
	return b.String()
}

// renderMetrics drives the exposition through a real http.ResponseWriter,
// which is what /metrics does, rather than writing into a buffer directly. A
// bytes.Buffer is not a ResponseWriter, and a stub would risk testing the stub.
func renderMetrics(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics returned %d: %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// TestOverflowBucketIsNotExposedAsHistogram pins the mitigation for the
// corrupt-output bug described in serveAttemptMetrics.
//
// The pre-fix output parsed, declared histogram, and was silently wrong, so a
// "does it parse" assertion is not enough. What is asserted is the specific
// thing that was wrong: a histogram_quantile-shaped input is no longer offered
// to histogram_quantile at all, and the observations the bucket carries are
// still accounted for exactly once, under names that are not a histogram.
func TestOverflowBucketIsNotExposedAsHistogram(t *testing.T) {
	m := NewMetrics()
	m.SetLabelCap(minLabelValues)
	// Distinct labels well past the cap, at a latency far too small to fall in
	// any high band. That mismatch is what made the old output so confidently
	// wrong: 34 observations averaging 10ms were reported as all being above
	// 45 seconds.
	const attempts = minLabelValues * 3
	const each = 10 * time.Millisecond
	for i := 0; i < attempts; i++ {
		m.Attempt(fmt.Sprintf("ep-%d", i), each, false)
	}
	// One live label admitted last, so the "does the loop still emit anything
	// after the bucket?" bug — a `break` where a `continue` was meant — is
	// caught rather than masked by the bucket sorting last.
	const liveEach = 3 * time.Millisecond
	m.Attempt("live", liveEach, false)

	series, err := parseExposition(renderMetrics(t, m))
	if err != nil {
		t.Fatalf("exposition does not parse: %v", err)
	}

	// 1. No histogram series for the overflow label, at any bucket bound, and
	//    under no metric family declared histogram. The pre-fix output had
	//    _bucket{le="0.25"} 0 ... _bucket{le="+Inf"} 34.
	const histFamily = "airouter_endpoint_attempt_duration_seconds"
	for _, x := range series {
		if x.labels["endpoint"] != overflowLabelValue {
			continue
		}
		if x.typ == "histogram" || x.name == histFamily || x.name == histFamily+"_bucket" {
			t.Errorf("overflow label is exposed as histogram series %q (le=%q): its "+
				"distribution is unknowable and histogram_quantile over it returns nonsense",
				x.name, x.labels["le"])
		}
	}

	// 2. The replacement gauges exist, are typed gauge, and are emitted for
	//    every endpoint — so the series does not appear and disappear as the
	//    cap is hit, and a "total minus live" query needs no special case.
	var sumAll, countAll, evSum, evCount, liveSum, liveCount float64
	gauges := 0
	for _, x := range series {
		switch x.name {
		case "airouter_evicted_attempts_latency_seconds_sum":
			if x.typ != "gauge" {
				t.Errorf("%s declared %q, want gauge", x.name, x.typ)
			}
			gauges++
			sumAll += x.value
			if x.labels["endpoint"] == overflowLabelValue {
				evSum += x.value
			}
		case "airouter_evicted_attempts_latency_seconds_count":
			if x.typ != "gauge" {
				t.Errorf("%s declared %q, want gauge", x.name, x.typ)
			}
			gauges++
			countAll += x.value
			if x.labels["endpoint"] == overflowLabelValue {
				evCount += x.value
			}
		case histFamily + "_sum":
			liveSum += x.value
		case histFamily + "_count":
			liveCount += x.value
		}
	}
	if gauges == 0 {
		t.Fatal("no replacement gauges in the exposition; the overflow data is unreported, not merely unhistogrammed")
	}
	if evSum == 0 || evCount == 0 {
		t.Fatalf("the overflow gauges carry no data (sum=%v count=%v) after %d labels were "+
			"driven past a cap of %d; the eviction is not rolling anything into the bucket",
			evSum, evCount, attempts, minLabelValues)
	}
	// Only the overflow label has a non-zero gauge; every live label is zero.
	if math.Abs(sumAll-evSum) > 1e-12 || math.Abs(countAll-evCount) > 1e-12 {
		t.Errorf("live endpoints report non-zero evicted latency (sum %v vs %v, count %v vs %v); "+
			"only the bucket should carry evicted observations", sumAll, evSum, countAll, evCount)
	}

	// 3. Nothing was lost and nothing was counted twice. Every attempt is
	//    either still in a live histogram or folded into the gauges, exactly
	//    once. Asserted as an identity over the output rather than a recomputed
	//    constant, so it stays true for any cap and any mix of labels.
	if got, want := liveCount+evCount, float64(attempts+1); got != want {
		t.Errorf("histogram counts plus gauge count = %v, want %v: observations were lost or double-counted",
			got, want)
	}
	wantSecs := float64(attempts)*each.Seconds() + liveEach.Seconds()
	if got := liveSum + evSum; math.Abs(got-wantSecs) > 1e-9 {
		t.Errorf("histogram sums plus gauge sum = %v, want %v: latency was lost or double-counted",
			got, wantSecs)
	}

	// 4. The live label still gets a real histogram, and it is a valid one.
	//    Without this the fix would also pass by breaking histograms everywhere.
	sawLive := false
	for _, x := range series {
		if x.typ == "histogram" && x.labels["endpoint"] == "live" {
			sawLive = true
		}
	}
	if !sawLive {
		t.Error("the live endpoint lost its histogram; the overflow branch must skip only the bucket")
	}
}

// TestEveryExportedHistogramIsWellFormed asserts the Prometheus histogram
// invariants across the WHOLE exposition, for every family and every label set.
//
// This is the generalisation of the overflow-bucket bug, and it is deliberately
// not written against `airouter_endpoint_attempt_duration_seconds`. That bug was
// found by hand, in one family, after three rounds of metrics work; the reason it
// survived is that every test named the specific series it cared about. A guard
// that walks the parsed output cannot be defeated by adding a new histogram,
// renaming one, or overflowing a map nobody thought to test.
//
// The invariants themselves are in checkHistogramInvariants, which returns
// violations rather than failing, so each one can be negative-controlled
// individually — see TestEachHistogramInvariantHasTeeth. A guard whose untested
// branches never fire is a guard you cannot trust to fire at all.
func TestEveryExportedHistogramIsWellFormed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(*Metrics)
	}{
		{"single endpoint", func(m *Metrics) {
			m.Attempt("a", 10*time.Millisecond, false)
		}},
		// A full map, so the overflow bucket exists and must be absent from
		// every histogram. This is the case that was broken.
		{"overflow bucket", func(m *Metrics) {
			m.SetLabelCap(minLabelValues)
			for i := 0; i < minLabelValues*3; i++ {
				m.Attempt(fmt.Sprintf("ep-%d", i), 10*time.Millisecond, false)
			}
		}},
		// Hostile label values, including one that looks like it is forging a
		// `le` label. A guard that builds its key by string surgery on raw
		// output would be defeated by this; one that parses first is not.
		{"hostile labels", func(m *Metrics) {
			for _, v := range []string{
				`a"b`, "a\\b", "a\nb", `",le="1"`, "__overflow__",
			} {
				m.Attempt(v, 10*time.Millisecond, false)
				m.Request(v, 200, time.Millisecond)
			}
		}},
		// Observations in several bands, so the monotonicity assertions run
		// against a non-degenerate distribution rather than trivially passing
		// on a series of all-zero buckets.
		{"every latency band", func(m *Metrics) {
			for _, d := range []time.Duration{
				time.Millisecond, 100 * time.Millisecond, 400 * time.Millisecond,
				1500 * time.Millisecond, 4 * time.Second, 25 * time.Second,
			} {
				m.Attempt("spread", d, false)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMetrics()
			tc.build(m)

			series, err := parseExposition(renderMetrics(t, m))
			if err != nil {
				t.Fatalf("exposition does not parse: %v", err)
			}
			if n := countHistogramBuckets(series); n == 0 {
				t.Fatal("no histogram buckets parsed; the guard is not looking at anything")
			}
			for _, v := range checkHistogramInvariants(series) {
				t.Error(v)
			}
		})
	}
}

// histBucket is one _bucket sample: a latency bound and the cumulative
// observation count at or below it.
type histBucket struct {
	le    string
	value float64
}

// histogramIndex groups the parsed exposition by metric family and then by
// label set, which is the level at which every invariant is defined: monotonicity
// is a property of one histogram's buckets, not of a family as a whole.
func histogramIndex(series []exposedSeries) (map[string]map[string][]histBucket, map[string]map[string]float64, map[string]map[string]float64) {
	buckets := map[string]map[string][]histBucket{}
	counts := map[string]map[string]float64{}
	sums := map[string]map[string]float64{}
	for _, x := range series {
		if x.typ != "histogram" {
			continue
		}
		if !strings.HasSuffix(x.name, "_bucket") {
			continue
		}
		le, ok := x.labels["le"]
		if !ok {
			// Reported by the caller as a violation; recorded as an empty
			// bucket so it is not silently skipped.
			continue
		}
		base := strings.TrimSuffix(x.name, "_bucket")
		set := renderLabelSet(x.labels, "le")
		if buckets[base] == nil {
			buckets[base] = map[string][]histBucket{}
		}
		buckets[base][set] = append(buckets[base][set], histBucket{le: le, value: x.value})
	}
	for _, x := range series {
		if !strings.HasSuffix(x.name, "_count") {
			continue
		}
		base := strings.TrimSuffix(x.name, "_count")
		set := renderLabelSet(x.labels, "")
		if counts[base] == nil {
			counts[base] = map[string]float64{}
		}
		counts[base][set] = x.value
	}
	for _, x := range series {
		if !strings.HasSuffix(x.name, "_sum") {
			continue
		}
		base := strings.TrimSuffix(x.name, "_sum")
		set := renderLabelSet(x.labels, "")
		if sums[base] == nil {
			sums[base] = map[string]float64{}
		}
		sums[base][set] = x.value
	}
	return buckets, counts, sums
}

// countHistogramBuckets is the guard's own liveness check: a well-formedness
// test that has silently stopped finding histograms is indistinguishable from
// one that is passing.
func countHistogramBuckets(series []exposedSeries) int {
	n := 0
	for _, x := range series {
		if x.typ == "histogram" && strings.HasSuffix(x.name, "_bucket") {
			n++
		}
	}
	return n
}

// checkHistogramInvariants returns one string per violation found. Returning
// rather than t.Error is what lets TestEachHistogramInvariantHasTeeth feed it
// deliberately-broken input.
//
// The invariants, per the exposition spec:
//
//  1. every _bucket sample carries an `le` label;
//  2. `le` bounds are strictly increasing;
//  3. exactly one `+Inf` bucket, and it is last;
//  4. cumulative counts never decrease, and `+Inf` is the maximum;
//  5. `+Inf` equals the family's `_count` for that label set.
//
// (4) is the one the overflow bucket actually violated: every finite bucket 0
// with `+Inf` = N claims all N observations exceeded the top bound while its own
// `_sum` said they were nearly instant.
func checkHistogramInvariants(series []exposedSeries) []string {
	var out []string
	bad := func(f string, a ...any) { out = append(out, fmt.Sprintf(f, a...)) }

	// (1) A bucket without `le` cannot be placed in the ordering at all, so it
	// is caught before anything tries to sort by it.
	for _, x := range series {
		if x.typ == "histogram" && strings.HasSuffix(x.name, "_bucket") {
			if _, ok := x.labels["le"]; !ok {
				bad("histogram sample %s has no `le` label: %v", x.name, x.labels)
			}
		}
	}

	buckets, counts, sums := histogramIndex(series)
	for family, sets := range buckets {
		for set, bs := range sets {
			where := family + "{" + set + "}"

			// (3) +Inf terminates the series, exactly once. Without it the
			// observation count is unrecoverable and quantile functions are
			// undefined, which is why a histogram missing it is not a histogram.
			nInf := 0
			for _, b := range bs {
				if b.le == "+Inf" {
					nInf++
				}
			}
			if nInf != 1 {
				bad("%s has %d +Inf buckets, want exactly 1", where, nInf)
				continue
			}
			if last := bs[len(bs)-1]; last.le != "+Inf" {
				bad("%s last bucket is le=%q, want +Inf; +Inf must terminate the series",
					where, last.le)
				continue
			}

			// (2) Strictly increasing bounds. A repeat or a decrease means the
			// buckets are not cumulative, so ordering them by value and by
			// emission would disagree.
			for i := 1; i < len(bs); i++ {
				prev, cur := bs[i-1].le, bs[i].le
				if cur == "+Inf" {
					break
				}
				lo, err1 := strconv.ParseFloat(prev, 64)
				hi, err2 := strconv.ParseFloat(cur, 64)
				if err1 != nil || err2 != nil {
					bad("%s: unparseable le bound %q or %q", where, prev, cur)
					break
				}
				if hi <= lo {
					bad("%s: le bounds are not strictly increasing at le=%q after le=%q",
						where, cur, prev)
					break
				}
			}

			// (4) Cumulative counts never decrease. The invalid-histogram
			// signature: a series reported as a distribution it does not have.
			for i := 1; i < len(bs); i++ {
				if bs[i].value < bs[i-1].value {
					bad("%s: bucket le=%q (%v) is LOWER than le=%q (%v); a cumulative "+
						"histogram cannot decrease", where, bs[i].le, bs[i].value,
						bs[i-1].le, bs[i-1].value)
					break
				}
			}
			if inf := bs[len(bs)-1].value; inf < bs[0].value {
				bad("%s: +Inf (%v) is below the first bucket (%v)", where, inf, bs[0].value)
			}

			// (5) +Inf and _count are two renderings of one number.
			if c, ok := counts[family][set]; ok {
				if inf := bs[len(bs)-1].value; inf != c {
					bad("%s: +Inf bucket is %v but _count is %v; observations are being "+
						"counted twice or not at all", where, inf, c)
				}
			}

			// (6) The buckets must be consistent with the family's own _sum.
			//
			// This is the invariant that actually catches the overflow bug, and
			// it is not redundant with (4). The overflow output was
			//
			//     le=0.25 ... le=45  all 0;  le=+Inf  11;  _sum  0.11
			//
			// Every consecutive pair is non-decreasing, so (4) passes. But the
			// buckets claim all 11 observations exceeded 45s, so a real _sum
			// would be at least 11*45 = 495 — not 0.11. The buckets and the sum
			// describe different worlds.
			//
			// Generally: if `above` observations are attributed to the open band
			// above the top finite bound, each of them is > that bound, so
			// _sum >= above * topBound.
			//
			// WHY THIS THRESHOLD, precisely, because a guard that is merely
			// "sound" is not enough — it also has to be *tight enough to fire* and
			// *loose enough not to false-positive*, and the choice is a
			// deliberate one:
			//
			//   - topBound is the largest FINITE le, i.e. the second-to-last
			//     bucket. The top finite band is (topBound, +Inf], so its
			//     population is exactly the +Inf bucket minus the top finite
			//     bucket. It is a lower bound on each member, never an equality:
			//     observations just above the bound barely contribute. That makes
			//     the test deliberately loose — it will not catch a mildly skewed
			//     distribution, and that is accepted. Catching that needs the real
			//     observations, which the exposition does not contain. What it
			//     DOES catch is the categorical case, where the buckets and the
			//     sum are off by orders of magnitude, which is what went wrong.
			//   - The comparison is strict `<`. A sum exactly equal to
			//     above*topBound is legal (all observations sitting precisely on
			//     the bound, as a single observation exactly at le=... would be),
			//     so equality must not be flagged.
			//   - `above > 0` guards the case where nothing is attributed above
			//     the top bound; then minSum is 0 and any sum is consistent.
			//   - `sum > 0` guards a missing or zero sum, which is not evidence
			//     of anything and would otherwise flag every series whose _sum
			//     has not been written yet.
			//   - `topBound > 0` guards a non-numeric or zero top bound, which
			//     makes the bound vacuous; (2) already reports those.
			//
			// Each of those four guards is pinned by a case in
			// TestHistogramInvariantSixBoundaries, because an unpinned guard is a
			// guess about behaviour rather than a fact about it.
			if sum, ok := sums[family][set]; ok && sum > 0 && len(bs) >= 2 {
				topBound, boundErr := strconv.ParseFloat(bs[len(bs)-2].le, 64)
				if boundErr != nil {
					// Non-numeric bounds: (2) already reports these.
					topBound = -1
				}
				if topBound > 0 {
					above := bs[len(bs)-1].value - bs[len(bs)-2].value
					if above > 0 {
						if minSum := above * topBound; sum < minSum {
							bad("%s: %v observations are attributed to the band above "+
								"le=%g, so _sum must be at least %g, but it is %g. The "+
								"buckets and the sum describe different distributions: the "+
								"series is being reported as a latency distribution it does "+
								"not have", where, above, topBound, minSum, sum)
						}
					}
				}
			}
		}
	}
	return out
}

// renderLabelSet renders a parsed sample's labels back into a stable key, so
// buckets and the _count they must agree with can be matched. When skip is
// non-empty that label is omitted — a bucket's `le` is part of what makes it
// that bucket, not part of the series it belongs to.
//
// Sorted so the key does not depend on Go's map iteration order.
func renderLabelSet(labels map[string]string, skip string) string {
	if len(labels) == 0 {
		return ""
	}
	names := make([]string, 0, len(labels))
	for n := range labels {
		if n != skip {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%s=%q", n, labels[n]))
	}
	return strings.Join(parts, ",")
}

// TestEachHistogramInvariantHasTeeth feeds checkHistogramInvariants a
// deliberately broken exposition, one violation at a time.
//
// The guard above only proves it accepts correct output. That is half the
// question: a well-formedness check whose branches never fire is one that will
// not fire when a real regression reaches it, and it looks identical from the
// outside. This is the part that is easy to skip and expensive to discover
// later, because the failure mode is a green test suite over corrupt metrics.
//
// The synthetic input is the wire format directly, so the violations are
// exactly the shapes seen in production — the first of these is the real
// pre-fix overflow bucket output, transcribed.
func TestEachHistogramInvariantHasTeeth(t *testing.T) {
	const fam = "airouter_endpoint_attempt_duration_seconds"

	// helper renders a labelled sample. Histograms under test are well-formed
	// apart from the single intended violation, so anything the guard reports
	// beyond the expected message is itself a finding.
	render := func(name, labels, value string) string {
		if labels == "" {
			return name + " " + value + "\n"
		}
		return name + "{" + labels + "} " + value + "\n"
	}
	wellFormed := func() []exposedSeries {
		body := "# TYPE " + fam + " histogram\n"
		// Cumulative: 3 at or below 0.5, 7 at or below 1, 9 total. The 2 above
		// le=1 contribute at least 2.0 to the sum, so _sum=7.1 is consistent.
		for _, b := range []struct{ le, v string }{{"0.5", "3"}, {"1", "7"}, {"+Inf", "9"}} {
			body += render(fam+"_bucket", `endpoint="a",le="`+b.le+`"`, b.v)
		}
		body += render(fam+"_sum", `endpoint="a"`, "7.1")
		body += render(fam+"_count", `endpoint="a"`, "9")
		series, err := parseExposition(body)
		if err != nil {
			t.Fatalf("synthetic well-formed input does not parse: %v", err)
		}
		return series
	}

	t.Run("baseline is clean", func(t *testing.T) {
		if v := checkHistogramInvariants(wellFormed()); len(v) != 0 {
			t.Fatalf("well-formed input reported violations: %v", v)
		}
	})

	for _, tc := range []struct {
		name   string
		mutate func([]exposedSeries) []exposedSeries
		want   string
	}{
		{
			// The real bug, transcribed: every finite bucket 0, +Inf = 9, and a
			// _sum of 1.5 for 9 observations that the buckets all place above
			// le=1. Consecutive buckets are non-decreasing, so this is invisible
			// to the "cannot decrease" check — it is caught by the buckets-vs-_sum
			// consistency invariant, which is the one that matters here.
			name: "zeroed finite buckets with a populated +Inf",
			mutate: func(s []exposedSeries) []exposedSeries {
				for i := range s {
					if s[i].name == fam+"_bucket" && s[i].labels["le"] != "+Inf" {
						s[i].value = 0
					}
				}
				return s
			},
			want: "different distributions",
		},
		{
			name: "missing +Inf",
			mutate: func(s []exposedSeries) []exposedSeries {
				out := s[:0]
				for _, x := range s {
					if x.name == fam+"_bucket" && x.labels["le"] == "+Inf" {
						continue
					}
					out = append(out, x)
				}
				return out
			},
			want: "want exactly 1",
		},
		{
			name: "+Inf is not last",
			mutate: func(s []exposedSeries) []exposedSeries {
				// Reorder only: move the +Inf bucket to the front, so the series
				// still has exactly one +Inf but it no longer terminates.
				//
				// The first version of this case appended +Inf to the end, where
				// it already was — a no-op the guard correctly reported nothing
				// for, which is exactly what a test that only looks like coverage
				// does. The second tried to rebuild the slice and tripped over
				// its own index arithmetic. Reordering is the whole point, so do
				// exactly that.
				var inf *exposedSeries
				out := make([]exposedSeries, 0, len(s))
				for i := range s {
					if s[i].name == fam+"_bucket" && s[i].labels["le"] == "+Inf" {
						inf = &s[i]
						continue
					}
					out = append(out, s[i])
				}
				if inf != nil {
					out = append([]exposedSeries{*inf}, out...)
				}
				return out
			},
			want: "must terminate",
		},
		{
			name: "duplicate +Inf",
			mutate: func(s []exposedSeries) []exposedSeries {
				var dup exposedSeries
				for _, x := range s {
					if x.name == fam+"_bucket" && x.labels["le"] == "+Inf" {
						dup = x
						break
					}
				}
				return append(s, dup)
			},
			want: "want exactly 1",
		},
		{
			name: "non-increasing le bounds",
			mutate: func(s []exposedSeries) []exposedSeries {
				for i := range s {
					if s[i].name == fam+"_bucket" && s[i].labels["le"] == "1" {
						s[i].labels["le"] = "0.1"
					}
				}
				return s
			},
			want: "not strictly increasing",
		},
		{
			name: "bucket without an le label",
			mutate: func(s []exposedSeries) []exposedSeries {
				for i := range s {
					if s[i].name == fam+"_bucket" {
						delete(s[i].labels, "le")
						break
					}
				}
				return s
			},
			want: "no `le` label",
		},
		{
			// The other half of the real bug: a histogram that is internally
			// consistent but disagrees with its own _count.
			name: "+Inf disagrees with _count",
			mutate: func(s []exposedSeries) []exposedSeries {
				for i := range s {
					if s[i].name == fam+"_count" {
						s[i].value = 4
					}
				}
				return s
			},
			want: "counted twice or not at all",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// wellFormed() builds fresh maps on every call, so the mutation can
			// edit in place without leaking between cases.
			before := wellFormed()
			after := tc.mutate(wellFormed())

			// Assert the mutation actually did something.
			//
			// One case in this table was a no-op from the start: it moved `+Inf`
			// to the end of the slice, where it already was, so the guard
			// correctly reported nothing and the case still "passed" as far as
			// the assertion below was concerned. A mutation that does not mutate
			// is the most expensive kind of useless test, because in review it is
			// indistinguishable from a real one — the only thing that exposes it
			// is comparing before and after. This makes the class impossible to
			// reintroduce silently.
			if seriesFingerprint(before) == seriesFingerprint(after) {
				t.Fatalf("mutation for %q changed nothing; this case cannot fail and "+
					"is not a test. A no-op mutation looks identical to a real one "+
					"in review, which is why it is checked here rather than trusted",
					tc.name)
			}

			v := checkHistogramInvariants(after)
			if len(v) == 0 {
				t.Fatalf("guard reported no violation for %q; this invariant has no teeth", tc.name)
			}
			joined := strings.Join(v, "\n")
			if !strings.Contains(joined, tc.want) {
				t.Errorf("violation reported, but not the one intended:\n%s", joined)
			}
			t.Logf("guard said:\n%s", joined)
		})
	}
}

// seriesFingerprint renders a parsed exposition to a canonical string, so two
// versions of the same samples can be compared for equality.
//
// It includes the sample ORDER, not just the set. The `+Inf` is not last
// negative control is precisely a reordering, and a set-based fingerprint
// would call that unchanged — which is the bug this was written to catch.
func seriesFingerprint(s []exposedSeries) string {
	var b strings.Builder
	for _, x := range s {
		fmt.Fprintf(&b, "%s|%s|%s|%v\n", x.typ, x.name, renderLabelSet(x.labels, ""), x.value)
	}
	return b.String()
}

// TestHistogramInvariantSixBoundaries pins every guard and boundary in
// invariant (6), the buckets-vs-_sum check.
//
// The main table proves the invariant fires on a violation. This proves it stays
// quiet in the cases where it must, which is the other half and the half that
// decides whether a guard is usable: a well-formedness check that fires on
// legitimate data gets disabled, and then it protects nothing.
//
// The threshold `_sum >= above * topBound` has four guards (`sum > 0`,
// `topBound > 0`, `above > 0`, strict `<`) and two equality boundaries. Each is
// asserted here, and each case is checked to have actually mutated its input —
// the lesson of the no-op `+Inf` case from the main table applies to this table
// too.
func TestHistogramInvariantSixBoundaries(t *testing.T) {
	const fam = "airouter_endpoint_attempt_duration_seconds"

	// build renders an exposition from explicit bucket/sum/count triples.
	build := func(buckets [][2]string, sum, count string) []exposedSeries {
		var body strings.Builder
		fmt.Fprintf(&body, "# TYPE %s histogram\n", fam)
		for _, b := range buckets {
			fmt.Fprintf(&body, "%s_bucket{endpoint=\"a\",le=\"%s\"} %s\n", fam, b[0], b[1])
		}
		fmt.Fprintf(&body, "%s_sum{endpoint=\"a\"} %s\n", fam, sum)
		fmt.Fprintf(&body, "%s_count{endpoint=\"a\"} %s\n", fam, count)
		series, err := parseExposition(body.String())
		if err != nil {
			t.Fatalf("synthetic exposition does not parse: %v", err)
		}
		return series
	}

	// viol6 filters to just invariant (6)'s message, so a case intended to
	// exercise a boundary is not failed by an unrelated violation.
	viol6 := func(s []exposedSeries) string {
		var out []string
		for _, v := range checkHistogramInvariants(s) {
			if strings.Contains(v, "different distributions") {
				out = append(out, v)
			}
		}
		return strings.Join(out, "\n")
	}

	for _, tc := range []struct {
		name    string
		buckets [][2]string
		sum     string
		count   string
		fires   bool
		why     string
	}{
		{
			// 2 observations above le=1, so _sum must be >= 2.0. 1.5 is the
			// real overflow-bug shape: internally monotonic, externally false.
			name:    "sum below the implied minimum",
			buckets: [][2]string{{"0.5", "3"}, {"1", "7"}, {"+Inf", "9"}},
			sum:     "1.5", count: "9", fires: true,
			why: "9-7=2 observations above le=1 requires sum>=2",
		},
		{
			// The exact boundary. 2 observations exactly at le=1 is the most
			// permissive legal reading, and the comparison is strict `<`, so
			// this must be accepted.
			name:    "sum exactly at the minimum",
			buckets: [][2]string{{"0.5", "3"}, {"1", "7"}, {"+Inf", "9"}},
			sum:     "2.0", count: "9", fires: false,
			why: "equality is legal; observations may sit precisely on the bound",
		},
		{
			name:    "sum comfortably above the minimum",
			buckets: [][2]string{{"0.5", "3"}, {"1", "7"}, {"+Inf", "9"}},
			sum:     "900.0", count: "9", fires: false,
			why: "all 9 observations far above le=1",
		},
		{
			// above == 0: nothing is attributed above the top finite bound, so
			// there is no minimum and any sum is consistent. Without the
			// `above > 0` guard this is still fine (minSum would be 0), so the
			// case documents that it is reachable rather than testing a guard.
			name:    "nothing attributed above the top bound",
			buckets: [][2]string{{"0.5", "3"}, {"1", "7"}, {"+Inf", "7"}},
			sum:     "0.1", count: "7", fires: false,
			why: "above=0 implies minSum=0, so a small sum is consistent",
		},
		{
			// sum == 0: no evidence either way, must stay quiet rather than
			// flagging every series whose _sum is absent.
			name:    "zero sum with observations above the bound",
			buckets: [][2]string{{"0.5", "3"}, {"1", "7"}, {"+Inf", "9"}},
			sum:     "0", count: "9", fires: false,
			why: "a zero sum is absence of evidence, not evidence of absence",
		},
		{
			// Only one finite bucket, so there is no top finite bound to compare
			// against and the check has nothing to say.
			name:    "single finite bucket",
			buckets: [][2]string{{"+Inf", "9"}},
			sum:     "0.01", count: "9", fires: false,
			why: "len(bs) < 2, so no top finite bound exists",
		},
		{
			// Negative latency bounds are nonsense input; (2) reports the
			// ordering, and (6) must not compound it with a bogus threshold.
			name:    "top bound is zero",
			buckets: [][2]string{{"0", "3"}, {"+Inf", "9"}},
			sum:     "0.01", count: "9", fires: false,
			why: "topBound=0 makes the bound vacuous, so (6) is skipped",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			series := build(tc.buckets, tc.sum, tc.count)
			got := viol6(series)
			fired := got != ""
			if fired != tc.fires {
				if tc.fires {
					t.Fatalf("expected invariant (6) to fire, but it stayed quiet.\n%s\n%s",
						tc.why, got)
				}
				t.Fatalf("invariant (6) fired on legitimate data, so it would be wrong "+
					"to ship: %s\n%s", tc.why, got)
			}
		})
	}
}

// FuzzHistogramInvariantsSurviveGarbage asserts the guard degrades safely on
// arbitrary text: it never panics, never hangs, and never reports a violation
// for output that is actually fine.
//
// The reason this is worth fuzzing: the whole origin of this work is a metrics
// defect that survived three audit rounds because every test enumerated
// hand-picked cases. Seven hand-written negative controls are still seven
// hand-picked cases. Feeding the checker text it has never seen explores the
// shapes nobody thought to write down — including malformed series, duplicate
// buckets, hostile label values, and the panics a state-machine parser can
// hide behind an early return.
//
// The property asserted is the weak one, deliberately. "No false positives" is
// what makes the guard safe to ship; finding *new* violations is a bonus the
// corpus will report through t.Errorf if it happens.
func FuzzHistogramInvariantsSurviveGarbage(f *testing.F) {
	// Seeds: a valid histogram, the real pre-fix overflow output, the two
	// structural failures found in round 5, and plain garbage.
	seeds := []string{
		`# TYPE h histogram
h_bucket{endpoint="a",le="0.5"} 3
h_bucket{endpoint="a",le="1"} 7
h_bucket{endpoint="a",le="+Inf"} 9
h_sum{endpoint="a"} 7.1
h_count{endpoint="a"} 9
`,
		// The actual bug, verbatim in shape.
		`# TYPE h histogram
h_bucket{endpoint="__overflow__",le="0.25"} 0
h_bucket{endpoint="__overflow__",le="45"} 0
h_bucket{endpoint="__overflow__",le="+Inf"} 11
h_sum{endpoint="__overflow__"} 0.11
h_count{endpoint="__overflow__"} 11
`,
		`# TYPE h histogram
h_bucket{le="+Inf"} 1
`,
		`# TYPE h histogram
h_bucket{endpoint="a",le="1"} 5
h_bucket{endpoint="a",le="+Inf"} 5
h_count{endpoint="a"} 9
`,
		"",
		"not exposition at all\n",
		`h_bucket{le=} 1`,
		`h_bucket{endpoint="a",le="NaN"} 3`,
		`h_bucket{endpoint="a",le="+Inf"} 1e400`,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, body string) {
		// A parser failure is a legitimate outcome for garbage, and the guard
		// never runs on it. The property under test is that this returns
		// promptly without panicking.
		series, err := parseExposition(body)
		if err != nil {
			return
		}
		violations := checkHistogramInvariants(series)

		// Every reported violation must be non-empty and must name the series
		// it is about. A violation string that is empty or unanchored would
		// indicate the checker emitted something unusable, which a caller
		// logging it would have no way to act on.
		for _, v := range violations {
			if strings.TrimSpace(v) == "" {
				t.Fatalf("empty violation string for input %q", body)
			}
		}
	})
}

// TestGoldenExpositions pins the guard against committed fixtures in
// testdata/metrics, so the historical defect stays reproducible without
// re-running a fuzzer.
//
// The fuzz target is the better long-run signal, but it is a poor memory: its
// corpus is gitignored, it explores randomly, and a 385M-execution run on one
// machine says nothing about the next one. The single most valuable input here
// is a specific historical output that no fuzzer is likely to rediscover — the
// `__overflow__` bucket as it was actually served, which is why it survived
// three audit rounds. That belongs on disk, parsed by a deterministic test.
//
// Both directions are asserted, because a guard is only useful if it is also
// quiet on correct data. A test that only checks the bad fixture passes just
// as happily when the guard fires on everything.
func TestGoldenExpositions(t *testing.T) {
	for _, tc := range []struct {
		file       string
		wantClean  bool
		wantSubstr string
	}{
		{
			// The defect, transcribed from a real pre-fix scrape: every finite
			// band 0 with a populated +Inf, and a _sum that says the
			// observations were nearly instant. Consecutive buckets are
			// non-decreasing and +Inf agrees with _count, so only the
			// buckets-vs-_sum invariant rejects it.
			file:       "overflow-as-histogram.prom",
			wantClean:  false,
			wantSubstr: "different distributions",
		},
		{
			// Real output from a running gateway under load, with 17 evictions
			// and the __overflow__ gauges present. Must produce nothing.
			file:      "valid-histograms.prom",
			wantClean: true,
		},
	} {
		t.Run(tc.file, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join("testdata/metrics", tc.file))
			if err != nil {
				t.Fatalf("reading fixture: %v", err)
			}
			series, err := parseExposition(string(body))
			if err != nil {
				t.Fatalf("fixture does not parse, so it tests nothing: %v", err)
			}
			if n := countHistogramBuckets(series); n == 0 {
				t.Fatal("fixture contains no histogram buckets; it cannot exercise the guard")
			}

			violations := checkHistogramInvariants(series)
			if tc.wantClean {
				if len(violations) != 0 {
					t.Fatalf("guard fired on known-good output (%d violations), so it is "+
						"too strict to ship:\n%s", len(violations), strings.Join(violations, "\n"))
				}
				return
			}
			if len(violations) == 0 {
				t.Fatal("guard accepted a fixture that is the historical defect; it has " +
					"lost the property it was written for")
			}
			if joined := strings.Join(violations, "\n"); !strings.Contains(joined, tc.wantSubstr) {
				t.Errorf("fixture rejected, but not for the intended reason.\nwant substring: %q\ngot:\n%s",
					tc.wantSubstr, joined)
			}
		})
	}
}
