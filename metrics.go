package main

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Metrics collects Prometheus-style metrics for operational visibility.
// All counters are atomic for thread-safe updates without locks.
type Metrics struct {
	requestsTotal    atomic.Int64
	requestsByModel  map[string]*atomic.Int64
	requestsByStatus map[string]*atomic.Int64
	failuresTotal    atomic.Int64
	failuresByStatus map[string]*atomic.Int64
	fallbacksTotal   atomic.Int64
	// fallbacksByFromEndpoint counts fallbacks triggered by each endpoint
	// (the model that failed and caused the fail-over). Low-cardinality and
	// actionable: operators can see which upstreams are the weakest link.
	fallbacksByFromEndpoint map[string]*atomic.Int64
	cooldownsApplied        atomic.Int64
	latencySumNS            atomic.Int64
	latencyCount            atomic.Int64
	// latencyBuckets[i] is the count of requests whose latency fell at or
	// below the boundary defined by latencyBoundaries[i]. The last bucket
	// (+Inf) catches everything above the largest finite boundary.
	latencyBuckets    []atomic.Int64
	latencyBoundaries []float64 // seconds, e.g. 0.005, 0.01, ..., 10, +Inf
	// circuitTransitions tracks circuit breaker state changes
	// (from="closed",to="open",endpoint="..."). Low-cardinality and
	// actionable: operators can see which endpoints are flapping.
	circuitTransitions map[string]*atomic.Int64
	// sseCommentsRoutedOut counts SSE comment (":" prefixed) lines dropped
	// before they could be mistaken for a data event.
	//
	// This is a canary, not a throughput metric. A relay that emits
	// ": keepalive" lines is a relay whose stream had once already tripped the
	// "[DONE] released ahead of the real chunks" bug (see proxy.go streamSSE):
	// a non-zero count means upstream chatter exists and the comment-filtering
	// path is actively doing its job. A count that climbs while streams look
	// healthy is the early warning that a provider started interleaving
	// keepalives mid-chunk again.
	sseCommentsRoutedOut atomic.Int64
	// Per-endpoint attempt telemetry: how many times an endpoint was tried
	// and how those attempts turned out. This is the missing half of the
	// picture — cooldowns and fallbacks say an endpoint misbehaved, but not
	// how often it was reached or how long it took, so there is no way to
	// answer "should I demote this endpoint" or "is 45s the right budget"
	// from data rather than guesswork.
	attemptsByEndpoint    map[string]*atomic.Int64
	attemptFailuresByEnd  map[string]*atomic.Int64
	attemptLatencyNSByEnd map[string]*atomic.Int64
	// attemptLatencyBucketsByEnd holds a small fixed histogram per endpoint.
	// Buckets are cumulative-free (one atomic per (endpoint, bucket)) so the
	// exporter does not have to sort or merge anything at scrape time.
	attemptLatencyBounds []float64 // seconds
	attemptHistByEnd     map[string][]atomic.Int64
	// labelsCap bounds the number of distinct label values any single
	// label-value map may hold. See (*Metrics).boundLabel for why this
	// exists and why the cap is enforced on the value map rather than on
	// the formatted output.
	labelsCap int
	// labelsOverflowed counts observations that were folded into
	// overflowLabel because their label value was not already tracked and
	// the map was at capacity. Without it, collapsing silently discards
	// data and an operator sees a suspiciously flat series with no
	// indication that anything was dropped.
	labelsOverflowed atomic.Int64
	// overflowLabel is the bucket name that unknown, at-capacity label
	// values collapse into. Chosen to be greppable and obviously not a
	// real provider so it is never mistaken for a configured endpoint.
	overflowLabel string
	mu            sync.RWMutex // protects map initialization
}

// overflowLabelValue is the bucket that unrecognised label values collapse into
// once a label map reaches its cap. Deliberately not a valid
// "provider:model" pair, so an operator seeing it knows immediately that it is
// an overflow bucket and not a misconfigured endpoint.
const overflowLabelValue = "__overflow__"

// maxLabelValues caps how many distinct values a single label map holds.
//
// This is a real ceiling, not a guess: the production config already carries 60
// distinct endpoint keys, and a nightly free-model sync is the kind of job that
// legitimately grows that list. 512 leaves ~8x headroom over the current
// config while keeping the worst case bounded and small. A single overflowing
// endpoint costs one counter plus one 12-bucket histogram, so the absolute
// memory ceiling is roughly 512 * (a few hundred bytes) — negligible.
const maxLabelValues = 512

// defaultLatencyBuckets are the standard Prometheus histogram boundaries
// (seconds). The trailing +Inf bucket is implied and appended at serve time.
var defaultLatencyBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

// attemptLatencyBoundaries are the histogram boundaries for a single upstream
// attempt, in seconds. They are coarser and shifted higher than the
// request-level buckets: what matters here is whether an endpoint answers
// fast, slowly, or not at all, and the interesting range starts where a
// request-level histogram is already saturated.
var attemptLatencyBoundaries = []float64{
	0.25, 0.5, 1, 2, 3, 5, 8, 12, 20, 30, 45,
}

// NewMetrics creates a new Metrics instance with pre-allocated maps.
func NewMetrics() *Metrics {
	m := &Metrics{
		requestsByModel:         make(map[string]*atomic.Int64),
		requestsByStatus:        make(map[string]*atomic.Int64),
		failuresByStatus:        make(map[string]*atomic.Int64),
		fallbacksByFromEndpoint: make(map[string]*atomic.Int64),
		circuitTransitions:      make(map[string]*atomic.Int64),
		attemptsByEndpoint:      make(map[string]*atomic.Int64),
		attemptFailuresByEnd:    make(map[string]*atomic.Int64),
		attemptLatencyNSByEnd:   make(map[string]*atomic.Int64),
		attemptHistByEnd:        make(map[string][]atomic.Int64),
		labelsCap:               maxLabelValues,
		overflowLabel:           overflowLabelValue,
	}
	m.attemptLatencyBounds = attemptLatencyBoundaries
	m.latencyBoundaries = defaultLatencyBuckets
	m.latencyBuckets = make([]atomic.Int64, len(defaultLatencyBuckets)+1) // +1 for +Inf
	return m
}

// counterModel returns the atomic counter for a logical model name, creating it
// on first access. The map is bounded like the rest: the model is client-supplied
// and reaches this function even on the pre-validation paths that record an
// empty or rejected model, so it must not be allowed to grow without limit.
func (m *Metrics) counterModel(key string) *atomic.Int64 {
	key = m.boundLabelValue(m.requestsByModel, key)
	m.mu.RLock()
	c, ok := m.requestsByModel[key]
	m.mu.RUnlock()
	if !ok {
		m.mu.Lock()
		if c, ok = m.requestsByModel[key]; !ok {
			c = &atomic.Int64{}
			m.requestsByModel[key] = c
		}
		m.mu.Unlock()
	}
	return c
}

// counterStatus returns the atomic counter for the given status code.
func (m *Metrics) counterStatus(key string) *atomic.Int64 {
	m.mu.RLock()
	c, ok := m.requestsByStatus[key]
	m.mu.RUnlock()
	if !ok {
		m.mu.Lock()
		if c, ok = m.requestsByStatus[key]; !ok {
			c = &atomic.Int64{}
			m.requestsByStatus[key] = c
		}
		m.mu.Unlock()
	}
	return c
}

// counterFailure returns the atomic counter for the given failure status code.
func (m *Metrics) counterFailure(key string) *atomic.Int64 {
	m.mu.RLock()
	c, ok := m.failuresByStatus[key]
	m.mu.RUnlock()
	if !ok {
		m.mu.Lock()
		if c, ok = m.failuresByStatus[key]; !ok {
			c = &atomic.Int64{}
			m.failuresByStatus[key] = c
		}
		m.mu.Unlock()
	}
	return c
}

// Request records a successful or failed request.
func (m *Metrics) Request(model string, status int, latency time.Duration) {
	m.requestsTotal.Add(1)
	m.counterModel(model).Add(1)
	m.counterStatus(strconv.Itoa(status)).Add(1)
	if status >= 400 {
		m.failuresTotal.Add(1)
		m.counterFailure(strconv.Itoa(status)).Add(1)
	}
	secs := latency.Seconds()
	m.latencySumNS.Add(latency.Nanoseconds())
	m.latencyCount.Add(1)
	// Histogram: increment every bucket whose boundary is >= latency.
	// The last bucket (+Inf) always catches the value.
	for i, bound := range m.latencyBoundaries {
		if secs <= bound {
			m.latencyBuckets[i].Add(1)
		}
	}
	m.latencyBuckets[len(m.latencyBuckets)-1].Add(1) // +Inf
}

// Fallback records a model fallback during request processing. The from
// endpoint is the model that failed and triggered the fail-over; it is
// used to expose per-endpoint fallback counts so operators can identify
// the weakest link in a chain.
func (m *Metrics) Fallback(fromEndpoint string) {
	m.fallbacksTotal.Add(1)
	m.counterFallbackFrom(fromEndpoint).Add(1)
}

// counterFallbackFrom returns the atomic counter for the given endpoint,
// creating it on first access. Bounded like the attempt telemetry, since the
// endpoint is a provider:model key subject to the same config churn.
func (m *Metrics) counterFallbackFrom(key string) *atomic.Int64 {
	key = m.boundLabelValue(m.fallbacksByFromEndpoint, key)
	m.mu.RLock()
	c, ok := m.fallbacksByFromEndpoint[key]
	m.mu.RUnlock()
	if !ok {
		m.mu.Lock()
		if c, ok = m.fallbacksByFromEndpoint[key]; !ok {
			c = &atomic.Int64{}
			m.fallbacksByFromEndpoint[key] = c
		}
		m.mu.Unlock()
	}
	return c
}

// Cooldown records a model cooldown application.
func (m *Metrics) Cooldown() {
	m.cooldownsApplied.Add(1)
}

// CircuitTransition records a circuit breaker state change for an endpoint.
func (m *Metrics) CircuitTransition(from, to CircuitState, endpoint string) {
	// from.String()/to.String() rather than string(from): CircuitState is an
	// int-backed enum, so a direct conversion would emit the rune for the
	// state number ("\x00", "\x01") instead of "closed"/"open"/"half-open".
	// The String form is closed over four values, so it needs no escaping —
	// labelEscape is applied anyway so this stays correct if that changes.
	key := fmt.Sprintf("from=%q,to=%q,endpoint=%q",
		labelEscape(from.String()), labelEscape(to.String()), labelEscape(endpoint))
	m.counterCircuitTransition(key).Add(1)
}

// counterCircuitTransition returns the atomic counter for a circuit state
// transition key, creating it on first access.
func (m *Metrics) counterCircuitTransition(key string) *atomic.Int64 {
	key = m.boundLabelValue(m.circuitTransitions, key)
	m.mu.RLock()
	c, ok := m.circuitTransitions[key]
	m.mu.RUnlock()
	if !ok {
		m.mu.Lock()
		if c, ok = m.circuitTransitions[key]; !ok {
			c = &atomic.Int64{}
			m.circuitTransitions[key] = c
		}
		m.mu.Unlock()
	}
	return c
}

// SSECommentRoutedOut records that an SSE comment line was dropped rather than
// being treated as part of an event. See the field comment for why this is
// worth counting at all.
func (m *Metrics) SSECommentRoutedOut(n int) {
	if n > 0 {
		m.sseCommentsRoutedOut.Add(int64(n))
	}
}

// Attempt records one upstream attempt against an endpoint: how long it took
// and whether it succeeded. Every fallback loop calls this around its
// per-endpoint call, so the resulting series answers the questions the
// request-level metrics cannot: which endpoints are actually slow, how often
// the router reaches for them, and how large a share of those reaches fail.
func (m *Metrics) Attempt(endpoint string, d time.Duration, ok bool) {
	if endpoint == "" {
		return
	}
	// One decision, four maps. Every attempt-telemetry structure below is
	// indexed by the returned key, so the cap is enforced once and the key sets
	// are identical by construction.
	key := m.boundedAttemptKey(endpoint)
	m.lazyCounter(m.attemptsByEndpoint, key).Add(1)
	if !ok {
		m.lazyCounter(m.attemptFailuresByEnd, key).Add(1)
	}
	ns := d.Nanoseconds()
	if ns < 0 {
		ns = 0
	}
	m.lazyCounter(m.attemptLatencyNSByEnd, key).Add(ns)

	m.mu.Lock()
	h, exists := m.attemptHistByEnd[key]
	if !exists {
		h = make([]atomic.Int64, len(m.attemptLatencyBounds)+1) // +1 for +Inf
		m.attemptHistByEnd[key] = h
	}
	m.mu.Unlock()

	secs := d.Seconds()
	idx := len(m.attemptLatencyBounds) // default to the +Inf bucket
	for i, bound := range m.attemptLatencyBounds {
		if secs <= bound {
			idx = i
			break
		}
	}
	h[idx].Add(1)
}

// boundedAttemptKey admits one new attempt label and returns the key it was
// actually stored under — the raw endpoint below the cap, overflowLabelValue at
// or above it.
//
// It must be called EXACTLY once per Attempt, before any of the attempt maps are
// touched, and the returned key is what all of them must be indexed by. An
// earlier version bounded each map independently inside lazyCounter, which was
// wrong twice over: the bounded value never propagated back to the caller, so
// the histogram map was still keyed by the raw label and grew without bound;
// and bounding each map separately let the maps disagree at the boundary, which
// could produce an endpoint with attempt counts but no latency histogram.
// Deciding once, up front, makes the key set identical across all four maps by
// construction and needs no further coordination.
//
// The cap includes the overflow bucket, because that bucket occupies a slot in
// the map exactly like a real endpoint does. The first version of this admitted
// cap real endpoints and then let the overflow key in on top, so the map
// reached cap+1. One extra counter is immaterial to memory, but "the map never
// exceeds the cap" is the property the tests assert, and a limit that is not
// actually the limit is worse than no limit at all — it reads as a guarantee it
// does not provide.
//
// So one slot is reserved for the bucket: a new real value is only admitted
// while there is room for the bucket to follow. That makes len(map) <= labelsCap
// an invariant, and it also guarantees the bucket is present whenever the map is
// full, which is what makes the collapse below total (there is always somewhere
// to put the observation). The cost is labelsCap-1 individually-named endpoints,
// which maxLabelValues leaves ample room for.
func (m *Metrics) boundedAttemptKey(endpoint string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, tracked := m.attemptsByEndpoint[endpoint]; tracked {
		return endpoint
	}
	// Room for a real value plus the reserved slot for the bucket.
	if len(m.attemptsByEndpoint) < m.labelsCap-1 {
		m.attemptsByEndpoint[endpoint] = &atomic.Int64{}
		return endpoint
	}
	// The bucket itself is a legitimate label that any caller may submit, and
	// it is counted in its own right when it is.
	if endpoint == m.overflowLabel {
		m.attemptsByEndpoint[endpoint] = &atomic.Int64{}
		return endpoint
	}
	m.labelsOverflowed.Add(1)
	key := m.overflowLabel
	if _, has := m.attemptsByEndpoint[key]; !has {
		// Unreachable given the reservation above, but the fallback keeps the
		// invariant true even if the reservation is ever changed: without it a
		// missing bucket would silently drop the observation.
		m.attemptsByEndpoint[key] = &atomic.Int64{}
	}
	return key
}

// lazyCounter returns the counter for key in store, creating it on first access.
// The key must already have been through boundedAttemptKey; this function only
// guarantees the counter allocation is race-free (read-lock fast path, then a
// double-check under the write lock so two goroutines racing on a brand-new key
// cannot each allocate one).
func (m *Metrics) lazyCounter(store map[string]*atomic.Int64, key string) *atomic.Int64 {
	m.mu.RLock()
	c, ok := store[key]
	m.mu.RUnlock()
	if ok {
		return c
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok = store[key]; !ok {
		c = &atomic.Int64{}
		store[key] = c
	}
	return c
}

// boundLabelValue maps a raw label value onto the value that will actually be
// used as a map key, folding unknown values into a single overflow bucket once
// the map is full. Used by the single-key maps (per-model requests, per-endpoint
// fallbacks, circuit transitions); the attempt telemetry decides once up front
// in boundedAttemptKey instead, because it spans four maps that must agree.
//
// Why this is needed. Every label value here comes from configuration
// (provider:model keys), but the configuration is not fixed for the process
// lifetime: HandleAdminConfig accepts an arbitrary new config and
// watchConfig hot-reloads the file on a 3 s poll. A config churn loop — or a
// nightly free-model sync that keeps introducing fresh model ids — therefore
// feeds genuinely unbounded values into these maps. Nothing ever evicted a
// series, and each endpoint costs a counter set plus a 12-bucket histogram, so
// the process grows until it is OOM-killed. A metrics endpoint that can be used
// to kill the gateway is a self-inflicted DoS.
//
// A value that is already a tracked key keeps its own series even at capacity;
// the cap only affects admitting *new* values. That means the hot endpoints of
// a churning config stay individually visible, and only the tail collapses.
func (m *Metrics) boundLabelValue(store map[string]*atomic.Int64, value string) string {
	m.mu.RLock()
	_, tracked := store[value]
	// Same slot reservation as boundedAttemptKey, for the same reason: without
	// it the map reaches cap+1 by letting the bucket in on top.
	room := len(store) < m.labelsCap-1
	m.mu.RUnlock()
	if tracked || room || value == m.overflowLabel {
		return value
	}
	m.labelsOverflowed.Add(1)
	return m.overflowLabel
}

// ServeHTTP writes Prometheus text format to the response.
// labelEscape escapes a label value for the Prometheus text exposition format.
//
// Every label value here is interpolated into a `"..."` quoted string with no
// escaping at all. That is a real injection path, not a theoretical one: the
// provider and model names come from config, and config is writable through
// HandleAdminConfig and rewritten on disk by the model-sync job. A provider
// named `x" 1\nairouter_requests_total{model="victim` therefore produces two
// syntactically valid series on the next scrape, and an operator or a downstream
// scraper reads both as genuine. The exposition format requires escaping
// backslash, double quote, and newline; everything else is literal.
func labelEscape(v string) string {
	if !strings.ContainsAny(v, "\\\"\n") {
		return v
	}
	var b strings.Builder
	b.Grow(len(v) + 8)
	for _, r := range v {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (m *Metrics) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintln(w, "# HELP airouter_requests_total Total requests by model and status")
	fmt.Fprintln(w, "# TYPE airouter_requests_total counter")

	m.mu.RLock()
	for k, c := range m.requestsByModel {
		fmt.Fprintf(w, "airouter_requests_total{model=\"%s\"} %d\n", labelEscape(k), c.Load())
	}
	for k, c := range m.requestsByStatus {
		fmt.Fprintf(w, "airouter_requests_total{status=\"%s\"} %d\n", labelEscape(k), c.Load())
	}
	m.mu.RUnlock()

	fmt.Fprintln(w, "# HELP airouter_failures_total Total failures by status code")
	fmt.Fprintln(w, "# TYPE airouter_failures_total counter")

	m.mu.RLock()
	for k, c := range m.failuresByStatus {
		fmt.Fprintf(w, "airouter_failures_total{status=\"%s\"} %d\n", labelEscape(k), c.Load())
	}
	m.mu.RUnlock()

	fmt.Fprintln(w, "# HELP airouter_fallbacks_total Number of model fallbacks")
	fmt.Fprintln(w, "# TYPE airouter_fallbacks_total counter")
	fmt.Fprintf(w, "airouter_fallbacks_total %d\n", m.fallbacksTotal.Load())

	// Per-endpoint fallback counts so operators can see which upstreams
	// trigger the most fail-overs.
	fmt.Fprintln(w, "# HELP airouter_fallbacks_total_by_endpoint Number of fallbacks triggered by each endpoint")
	fmt.Fprintln(w, "# TYPE airouter_fallbacks_total_by_endpoint counter")
	m.mu.RLock()
	for k, c := range m.fallbacksByFromEndpoint {
		fmt.Fprintf(w, "airouter_fallbacks_total_by_endpoint{endpoint=\"%s\"} %d\n", labelEscape(k), c.Load())
	}
	m.mu.RUnlock()

	fmt.Fprintln(w, "# HELP airouter_cooldowns_applied_total Number of cooldown applications")
	fmt.Fprintln(w, "# TYPE airouter_cooldowns_applied_total counter")
	fmt.Fprintf(w, "airouter_cooldowns_applied_total %d\n", m.cooldownsApplied.Load())

	// Circuit state transitions: per (from, to, endpoint) counters.
	fmt.Fprintln(w, "# HELP airouter_circuit_state_transitions Number of circuit breaker state transitions")
	fmt.Fprintln(w, "# TYPE airouter_circuit_state_transitions counter")
	m.mu.RLock()
	for k, c := range m.circuitTransitions {
		fmt.Fprintf(w, "airouter_circuit_state_transitions{%s} %d\n", k, c.Load())
	}
	m.mu.RUnlock()

	// Latency histogram (Prometheus _bucket / _sum / _count).
	fmt.Fprintln(w, "# HELP airouter_request_duration_seconds Request latency in seconds")
	fmt.Fprintln(w, "# TYPE airouter_request_duration_seconds histogram")
	for i, bound := range m.latencyBoundaries {
		fmt.Fprintf(w, "airouter_request_duration_seconds_bucket{le=\"%g\"} %d\n",
			bound, m.latencyBuckets[i].Load())
	}
	fmt.Fprintf(w, "airouter_request_duration_seconds_bucket{le=\"+Inf\"} %d\n",
		m.latencyBuckets[len(m.latencyBuckets)-1].Load())
	count := m.latencyCount.Load()
	if count > 0 {
		sum := float64(m.latencySumNS.Load()) / 1e9
		fmt.Fprintf(w, "airouter_request_duration_seconds_sum %f\n", sum)
		fmt.Fprintf(w, "airouter_request_duration_seconds_count %d\n", count)
	}

	fmt.Fprintln(w, "# HELP airouter_sse_comments_routed_out SSE comment lines dropped instead of being parsed as events")
	fmt.Fprintln(w, "# TYPE airouter_sse_comments_routed_out counter")
	fmt.Fprintf(w, "airouter_sse_comments_routed_out %d\n", m.sseCommentsRoutedOut.Load())

	// A non-zero value here means label values were being discarded for lack
	// of registry space and folded into __overflow__ instead. It is the signal
	// that either the cap is too low for the deployment or something is
	// feeding the gateway unbounded label values (a config churn loop, or a
	// model sync inventing new ids). While it is zero, every series in this
	// response is exact.
	fmt.Fprintln(w, "# HELP airouter_metrics_label_overflow_total Observations folded into the overflow bucket because the label registry is full")
	fmt.Fprintln(w, "# TYPE airouter_metrics_label_overflow_total counter")
	fmt.Fprintf(w, "airouter_metrics_label_overflow_total %d\n", m.labelsOverflowed.Load())

	m.serveAttemptMetrics(w)
}

// serveAttemptMetrics exports the per-endpoint attempt telemetry: how often an
// endpoint is reached, how often those attempts fail, and how long they take.
//
// Read together these answer the tuning questions the request-level metrics
// cannot. A high attempt count with a high failure ratio means the endpoint is
// being reached and is broken (demote it). A high attempt count concentrated
// in the 12s+ buckets means the chain is paying a real timeout for it on every
// request (reorder it, or shorten the budget).
func (m *Metrics) serveAttemptMetrics(w http.ResponseWriter) {
	// Snapshot under one read lock, then format outside it: formatting writes
	// to the network and must not block the counters other requests update.
	m.mu.RLock()
	attempts := make(map[string]int64, len(m.attemptsByEndpoint))
	for k, c := range m.attemptsByEndpoint {
		attempts[k] = c.Load()
	}
	failures := make(map[string]int64, len(m.attemptFailuresByEnd))
	for k, c := range m.attemptFailuresByEnd {
		failures[k] = c.Load()
	}
	latSum := make(map[string]int64, len(m.attemptLatencyNSByEnd))
	for k, c := range m.attemptLatencyNSByEnd {
		latSum[k] = c.Load()
	}
	hists := make(map[string][]int64, len(m.attemptHistByEnd))
	for k, h := range m.attemptHistByEnd {
		row := make([]int64, len(h))
		for i := range h {
			row[i] = h[i].Load()
		}
		hists[k] = row
	}
	m.mu.RUnlock()

	if len(attempts) == 0 {
		return
	}

	fmt.Fprintln(w, "# HELP airouter_endpoint_attempts_total Upstream attempts made against each endpoint")
	fmt.Fprintln(w, "# TYPE airouter_endpoint_attempts_total counter")
	fmt.Fprintln(w, "# HELP airouter_endpoint_attempt_failures_total Upstream attempts against each endpoint that failed")
	fmt.Fprintln(w, "# TYPE airouter_endpoint_attempt_failures_total counter")
	fmt.Fprintln(w, "# HELP airouter_endpoint_attempt_duration_seconds Duration of a single upstream attempt")
	fmt.Fprintln(w, "# TYPE airouter_endpoint_attempt_duration_seconds histogram")

	// Sort keys so the output is stable between scrapes. Prometheus does not
	// require it, but a diffable /metrics is much easier to read in a
	// terminal and makes accidental churn visible.
	endpoints := make([]string, 0, len(attempts))
	for k := range attempts {
		endpoints = append(endpoints, k)
	}
	sort.Strings(endpoints)

	for _, ep := range endpoints {
		n := attempts[ep]
		fmt.Fprintf(w, "airouter_endpoint_attempts_total{endpoint=\"%s\"} %d\n", labelEscape(ep), n)
		fmt.Fprintf(w, "airouter_endpoint_attempt_failures_total{endpoint=\"%s\"} %d\n", labelEscape(ep), failures[ep])

		sum := float64(latSum[ep]) / 1e9
		h := hists[ep]
		var cumulative int64
		for i, bound := range m.attemptLatencyBounds {
			if i < len(h) {
				cumulative += h[i]
			}
			fmt.Fprintf(w, "airouter_endpoint_attempt_duration_seconds_bucket{endpoint=\"%s\",le=\"%g\"} %d\n", labelEscape(ep), bound, cumulative)
		}
		fmt.Fprintf(w, "airouter_endpoint_attempt_duration_seconds_bucket{endpoint=\"%s\",le=\"+Inf\"} %d\n", labelEscape(ep), n)
		fmt.Fprintf(w, "airouter_endpoint_attempt_duration_seconds_sum{endpoint=\"%s\"} %f\n", labelEscape(ep), sum)
		fmt.Fprintf(w, "airouter_endpoint_attempt_duration_seconds_count{endpoint=\"%s\"} %d\n", labelEscape(ep), n)
	}
}
