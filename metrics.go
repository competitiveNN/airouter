package main

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
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
	mu                   sync.RWMutex // protects map initialization
}

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
	}
	m.attemptLatencyBounds = attemptLatencyBoundaries
	m.latencyBoundaries = defaultLatencyBuckets
	m.latencyBuckets = make([]atomic.Int64, len(defaultLatencyBuckets)+1) // +1 for +Inf
	return m
}

// counter returns the atomic counter for the given key, creating it on first
// access. The double-check under write lock prevents duplicate allocation.
func (m *Metrics) counter(key string) *atomic.Int64 {
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

// counterStatus returns the atomic counter for the given status key.
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

// counterFailure returns the atomic counter for the given failure status key.
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
	m.counter("requests_by_model_" + model).Add(1)
	statusKey := "requests_by_status_" + strconv.Itoa(status)
	m.counterStatus(statusKey).Add(1)
	if status >= 400 {
		m.failuresTotal.Add(1)
		m.counterFailure("failures_by_status_" + strconv.Itoa(status)).Add(1)
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
// creating it on first access.
func (m *Metrics) counterFallbackFrom(key string) *atomic.Int64 {
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
	key := fmt.Sprintf("from=\"%s\",to=\"%s\",endpoint=\"%s\"", from, to, endpoint)
	m.counterCircuitTransition(key).Add(1)
}

// counterCircuitTransition returns the atomic counter for a circuit state
// transition key, creating it on first access.
func (m *Metrics) counterCircuitTransition(key string) *atomic.Int64 {
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
	m.counterAttempts(endpoint).Add(1)
	if !ok {
		m.counterAttemptFailures(endpoint).Add(1)
	}
	ns := d.Nanoseconds()
	if ns < 0 {
		ns = 0
	}
	m.counterAttemptLatencySum(endpoint).Add(ns)

	m.mu.Lock()
	h, exists := m.attemptHistByEnd[endpoint]
	if !exists {
		h = make([]atomic.Int64, len(m.attemptLatencyBounds)+1) // +1 for +Inf
		m.attemptHistByEnd[endpoint] = h
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

func (m *Metrics) counterAttempts(endpoint string) *atomic.Int64 {
	return m.lazyCounter(m.attemptsByEndpoint, endpoint)
}

func (m *Metrics) counterAttemptFailures(endpoint string) *atomic.Int64 {
	return m.lazyCounter(m.attemptFailuresByEnd, endpoint)
}

func (m *Metrics) counterAttemptLatencySum(endpoint string) *atomic.Int64 {
	return m.lazyCounter(m.attemptLatencyNSByEnd, endpoint)
}

// lazyCounter returns the counter for endpoint in store, creating it on first
// access. The store must be one of the attempt-telemetry maps. Read-locks for
// the common case and double-checks under a write lock so two goroutines
// racing on a brand-new endpoint cannot each allocate one.
func (m *Metrics) lazyCounter(store map[string]*atomic.Int64, endpoint string) *atomic.Int64 {
	m.mu.RLock()
	c, ok := store[endpoint]
	m.mu.RUnlock()
	if ok {
		return c
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok = store[endpoint]; !ok {
		c = &atomic.Int64{}
		store[endpoint] = c
	}
	return c
}

// ServeHTTP writes Prometheus text format to the response.
func (m *Metrics) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintln(w, "# HELP airouter_requests_total Total requests by model and status")
	fmt.Fprintln(w, "# TYPE airouter_requests_total counter")

	m.mu.RLock()
	for k, c := range m.requestsByModel {
		fmt.Fprintf(w, "airouter_requests_total{model=\"%s\"} %d\n", k, c.Load())
	}
	for k, c := range m.requestsByStatus {
		fmt.Fprintf(w, "airouter_requests_total{status=\"%s\"} %d\n", k, c.Load())
	}
	m.mu.RUnlock()

	fmt.Fprintln(w, "# HELP airouter_failures_total Total failures by status code")
	fmt.Fprintln(w, "# TYPE airouter_failures_total counter")

	m.mu.RLock()
	for k, c := range m.failuresByStatus {
		fmt.Fprintf(w, "airouter_failures_total{status=\"%s\"} %d\n", k, c.Load())
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
		fmt.Fprintf(w, "airouter_fallbacks_total_by_endpoint{endpoint=\"%s\"} %d\n", k, c.Load())
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
		fmt.Fprintf(w, "airouter_endpoint_attempts_total{endpoint=\"%s\"} %d\n", ep, n)
		fmt.Fprintf(w, "airouter_endpoint_attempt_failures_total{endpoint=\"%s\"} %d\n", ep, failures[ep])

		sum := float64(latSum[ep]) / 1e9
		h := hists[ep]
		var cumulative int64
		for i, bound := range m.attemptLatencyBounds {
			if i < len(h) {
				cumulative += h[i]
			}
			fmt.Fprintf(w, "airouter_endpoint_attempt_duration_seconds_bucket{endpoint=\"%s\",le=\"%g\"} %d\n", ep, bound, cumulative)
		}
		fmt.Fprintf(w, "airouter_endpoint_attempt_duration_seconds_bucket{endpoint=\"%s\",le=\"+Inf\"} %d\n", ep, n)
		fmt.Fprintf(w, "airouter_endpoint_attempt_duration_seconds_sum{endpoint=\"%s\"} %f\n", ep, sum)
		fmt.Fprintf(w, "airouter_endpoint_attempt_duration_seconds_count{endpoint=\"%s\"} %d\n", ep, n)
	}
}
