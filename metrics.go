package main

import (
	"fmt"
	"net/http"
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
	mu                 sync.RWMutex // protects map initialization
}

// defaultLatencyBuckets are the standard Prometheus histogram boundaries
// (seconds). The trailing +Inf bucket is implied and appended at serve time.
var defaultLatencyBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

// NewMetrics creates a new Metrics instance with pre-allocated maps.
func NewMetrics() *Metrics {
	m := &Metrics{
		requestsByModel:           make(map[string]*atomic.Int64),
		requestsByStatus:          make(map[string]*atomic.Int64),
		failuresByStatus:          make(map[string]*atomic.Int64),
		fallbacksByFromEndpoint:   make(map[string]*atomic.Int64),
		circuitTransitions:        make(map[string]*atomic.Int64),
	}
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
}