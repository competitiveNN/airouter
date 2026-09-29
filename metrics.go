package main

import (
	"fmt"
	"io"
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
	requestsByModel  map[labelKey]*atomic.Int64
	requestsByStatus map[labelKey]*atomic.Int64
	failuresTotal    atomic.Int64
	failuresByStatus map[labelKey]*atomic.Int64
	fallbacksTotal   atomic.Int64
	// fallbacksByFromEndpoint counts fallbacks triggered by each endpoint
	// (the model that failed and caused the fail-over). Low-cardinality and
	// actionable: operators can see which upstreams are the weakest link.
	fallbacksByFromEndpoint map[labelKey]*atomic.Int64
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
	circuitTransitions map[labelKey]*atomic.Int64
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
	attemptsByEndpoint    map[labelKey]*atomic.Int64
	attemptFailuresByEnd  map[labelKey]*atomic.Int64
	attemptLatencyNSByEnd map[labelKey]*atomic.Int64
	// attemptLatencyBucketsByEnd holds a small fixed histogram per endpoint.
	// Buckets are cumulative-free (one atomic per (endpoint, bucket)) so the
	// exporter does not have to sort or merge anything at scrape time.
	attemptLatencyBounds []float64 // seconds
	attemptHistByEnd     map[labelKey][]atomic.Int64
	// labelsCap bounds the number of distinct label values any single
	// label-value map may hold. See boundedAttemptKeyLocked for why this exists and
	// why the cap is enforced on the stored keys rather than on the output.
	labelsCap int
	// attemptAdmitted[i] is the index into the attempt admission ring, and
	// attemptRing[i] is the label that occupied it. Together they implement a
	// FIFO admission window over attemptsByEndpoint: the label whose slot is
	// oldest is the one recycled when a new value needs room. See
	// boundedAttemptKeyLocked for why FIFO beats a purely "first come, never
	// evicted" set.
	attemptAdmitted []int64 // counter/clock, monotonically increasing
	attemptRing     []labelKey
	// attemptClock is the admission counter that attemptAdmitted entries are
	// compared against.
	attemptClock int64
	// attemptIndex maps an admitted label to its position in attemptRing.
	attemptIndex map[labelKey]int
	// labelsEvicted counts labels displaced from the admission window to make
	// room for a new one. Distinct from labelsOverflowed (collapsed into the
	// bucket) — this means a previously-named series went away. A non-zero
	// value is normal after a genuine config change that removes or renames
	// endpoints; a *steadily climbing* value means the cap is too small for
	// the deployment, which is the signal to raise it.
	labelsEvicted atomic.Int64
	// labelsOverflowed counts observations folded into overflowKey because
	// no label could be admitted. Without it, collapsing would silently
	// discard data and an operator would see a suspiciously flat series with
	// no indication that anything was dropped.
	labelsOverflowed atomic.Int64
	// overflowKey is the label key that unknown, at-capacity label values
	// collapse into. It is a complete, legal one-label key rather than a bare
	// value, so it is a valid key in every bounded map and the exporter needs
	// no special case for it — see labelKey for the bug that motivated this.
	overflowKey labelKey
	mu          sync.RWMutex // protects map initialization
}

// overflowLabelValue is the bucket that unrecognised label values collapse into
// once a label map reaches its cap. Deliberately not a valid
// "provider:model" pair, so an operator seeing it knows immediately that it is
// an overflow bucket and not a misconfigured endpoint.
const overflowLabelValue = "__overflow__"

// The label names the bounded maps are keyed by. Typed as labelKey so they
// cannot be misspelled at a call site without a compile error, and named
// constants because the collapse target is derived from them.
const (
	labelNameModel    labelKey = "model"
	labelNameStatus   labelKey = "status"
	labelNameEndpoint labelKey = "endpoint"
)

// overflowLabelKey is overflowLabelValue in the form a labelKey map stores: a
// complete, legal one-label key. It is the bucket for the attempt maps, which
// key on `endpoint` and recycle rather than collapse. Collapsing maps build
// theirs per label name — see boundLabelKey.
var overflowLabelKey = bareKey(string(labelNameEndpoint), overflowLabelValue)

// defaultMaxLabelValues caps how many distinct values a single label map holds.
//
// This is a real ceiling, not a guess: the production config already carries 60
// distinct endpoint keys, and a nightly free-model sync is the kind of job that
// legitimately grows that list. 512 leaves ~8x headroom over the current config
// while keeping the worst case bounded and small. A single overflowing endpoint
// costs one counter plus one 12-bucket histogram, so the absolute memory
// ceiling is roughly 512 * (a few hundred bytes) — negligible.
//
// It is the *default*, not a constant baked into the behaviour: deployments with
// many more endpoints can raise it via `preferences.max_label_cardinality` in
// config.yaml without a rebuild, and validate-config.py enforces the range.
const defaultMaxLabelValues = 512

// Bounds for the configurable cap. The floor exists because a cap too small to
// hold the four logical models plus a handful of endpoints makes the per-endpoint
// series useless, and the ceiling exists because the whole point of the cap is
// to bound memory: at 65536 the worst case is ~65536 histograms, which is enough
// to matter.
const (
	minLabelValues   = 16
	maxAllowedLabels = 65536
)

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
		requestsByModel:         make(map[labelKey]*atomic.Int64),
		requestsByStatus:        make(map[labelKey]*atomic.Int64),
		failuresByStatus:        make(map[labelKey]*atomic.Int64),
		fallbacksByFromEndpoint: make(map[labelKey]*atomic.Int64),
		circuitTransitions:      make(map[labelKey]*atomic.Int64),
		attemptsByEndpoint:      make(map[labelKey]*atomic.Int64),
		attemptFailuresByEnd:    make(map[labelKey]*atomic.Int64),
		attemptLatencyNSByEnd:   make(map[labelKey]*atomic.Int64),
		attemptHistByEnd:        make(map[labelKey][]atomic.Int64),
		attemptIndex:            make(map[labelKey]int),
		labelsCap:               defaultMaxLabelValues,
		overflowKey:             overflowLabelKey,
	}
	m.attemptLatencyBounds = attemptLatencyBoundaries
	m.latencyBoundaries = defaultLatencyBuckets
	m.latencyBuckets = make([]atomic.Int64, len(defaultLatencyBuckets)+1) // +1 for +Inf
	return m
}

// SetLabelCap sets the per-map label cardinality cap. Values outside
// [minLabelValues, maxAllowedLabels] are ignored, because a cap that is too
// small destroys the per-endpoint series and one that is too large defeats the
// memory bound this exists to provide. Rejects rather than clamps so a bad
// config value is visible in the log instead of being silently reinterpreted.
//
// Only affects labels admitted after the call. Shrinking the cap does not
// retroactively delete series: doing that would silently drop history, and the
// next new label will simply evict the oldest admitted one. The effective
// number of named labels therefore converges on the new cap on its own.
func (m *Metrics) SetLabelCap(n int) {
	if n < minLabelValues || n > maxAllowedLabels {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if n == m.labelsCap {
		return
	}
	m.labelsCap = n
	// Prune to the new cap immediately rather than waiting for new labels to
	// arrive. Rolling a victim's counters into the bucket (instead of dropping
	// them) is what lets this happen without losing observations, so the totals
	// are identical either way — but pruning now means the invariant
	// len(map) <= labelsCap holds immediately, instead of being violated for as
	// long as the deployment happens to keep using only the old labels.
	for len(m.attemptsByEndpoint) > n {
		victim := m.oldestAdmittedLocked()
		if victim == "" {
			break
		}
		m.rollUpToOverflowLocked(victim)
		m.labelsEvicted.Add(1)
	}
	// The ring may still hold entries for labels that were already gone before
	// the resize; drop them so the ring and the map stay in agreement.
	m.syncRingLocked()
}

// syncRingLocked drops ring entries whose label is no longer in the attempt
// map. Caller holds m.mu.
func (m *Metrics) syncRingLocked() {
	for i := 0; i < len(m.attemptRing); {
		ep := m.attemptRing[i]
		if _, live := m.attemptsByEndpoint[ep]; live && ep != m.overflowKey {
			i++
			continue
		}
		m.compactRingLocked(i)
	}
}

// LabelCap returns the configured per-map cardinality cap.
func (m *Metrics) LabelCap() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.labelsCap
}

// counterModel returns the atomic counter for a logical model name, creating it
// on first access. The map is bounded like the rest: the model is client-supplied
// and reaches this function even on the pre-validation paths that record an
// empty or rejected model, so it must not be allowed to grow without limit.
func (m *Metrics) counterModel(key labelKey) *atomic.Int64 {
	return m.admitCollapsible(m.requestsByModel, key, labelNameModel)
}

// counterStatus returns the atomic counter for the given status code.
//
// Bounded, and the bound is not optional tidiness. The status here is
// resp.StatusCode forwarded from the *upstream* response, not a code this
// gateway chose, so the value set is whatever a remote server felt like
// returning. A misbehaving or hostile upstream that varies its status code per
// request turns these two maps into exactly the unbounded-label vector the
// rest of this file is built to prevent. They were the only two bounded-intent
// maps still missing the call, and the only reason it went unnoticed is that
// no test drove a status map past its cap — see
// TestEveryBoundedMapHasAWellFormedKeyShape, which asserts the bound for every
// map in one table precisely so "which maps are bounded" stops being a thing
// you have to remember.
func (m *Metrics) counterStatus(key labelKey) *atomic.Int64 {
	return m.admitCollapsible(m.requestsByStatus, key, labelNameStatus)
}

// counterFailure returns the atomic counter for the given failure status code.
// Bounded for the same reason as counterStatus: the status is upstream-supplied.
func (m *Metrics) counterFailure(key labelKey) *atomic.Int64 {
	return m.admitCollapsible(m.failuresByStatus, key, labelNameStatus)
}

// Request records a successful or failed request.
func (m *Metrics) Request(model string, status int, latency time.Duration) {
	m.requestsTotal.Add(1)
	m.counterModel(singleLabelKey("model", model)).Add(1)
	m.counterStatus(singleLabelKey("status", strconv.Itoa(status))).Add(1)
	if status >= 400 {
		m.failuresTotal.Add(1)
		m.counterFailure(singleLabelKey("status", strconv.Itoa(status))).Add(1)
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
	m.counterFallbackFrom(singleLabelKey("endpoint", fromEndpoint)).Add(1)
}

// counterFallbackFrom returns the atomic counter for the given endpoint,
// creating it on first access. Bounded like the attempt telemetry, since the
// endpoint is a provider:model key subject to the same config churn.
func (m *Metrics) counterFallbackFrom(key labelKey) *atomic.Int64 {
	return m.admitCollapsible(m.fallbacksByFromEndpoint, key, labelNameEndpoint)
}

// Cooldown records a model cooldown application.
func (m *Metrics) Cooldown() {
	m.cooldownsApplied.Add(1)
}

// CircuitTransition records a circuit breaker state change for an endpoint.
func (m *Metrics) CircuitTransition(from, to CircuitState, endpoint string) {
	m.counterCircuitTransition(circuitTransitionKey(from, to, endpoint)).Add(1)
}

// counterCircuitTransition returns the atomic counter for a circuit state
// transition key, creating it on first access.
func (m *Metrics) counterCircuitTransition(key labelKey) *atomic.Int64 {
	return m.admitCollapsible(m.circuitTransitions, key, labelNameEndpoint)
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
	ns := d.Nanoseconds()
	if ns < 0 {
		ns = 0
	}
	idx := len(m.attemptLatencyBounds) // default to the +Inf bucket
	secs := d.Seconds()
	for i, bound := range m.attemptLatencyBounds {
		if secs <= bound {
			idx = i
			break
		}
	}

	// The whole observation is recorded under ONE write lock, rather than
	// deciding the key under a lock and then updating each map in a separate
	// critical section. The split version had a real race that the concurrency
	// test caught: thread A could be handed the key "ep:7", thread B could
	// evict "ep:7" and recycle its slot, and thread A would then re-create the
	// entry in the maps afterwards. The result was a key set that exceeded the
	// cap (the resurrected key plus the recycled one) and an attempt counted
	// against a series whose history had just been rolled into the bucket, so
	// the observation was effectively double-counted.
	//
	// Holding the lock for the whole update is the fix, and it is cheap: the
	// critical section is a few map lookups on a map of at most a few hundred
	// entries. The atomics inside are still needed, because ServeHTTP reads the
	// counters under a read lock and must not be blocked — but now the counters
	// can no longer be mutated between being created and being used.
	m.mu.Lock()
	key := m.boundedAttemptKeyLocked(singleLabelKey("endpoint", endpoint))
	if c, ok := m.attemptsByEndpoint[key]; ok {
		c.Add(1)
	}
	if !ok {
		// Create-on-write, not lookup-only. The first version read the map and
		// incremented only if the entry happened to exist, which meant the very
		// first failure for an endpoint was silently discarded: the failure
		// series stayed absent and the endpoint reported a 0% failure rate
		// forever. The buckets are keyed by the same set as the attempt
		// counters, so the entry is created here and nowhere else.
		c := m.attemptFailuresByEnd[key]
		if c == nil {
			c = &atomic.Int64{}
			m.attemptFailuresByEnd[key] = c
		}
		c.Add(1)
	}
	if c := m.attemptLatencyNSByEnd[key]; c != nil {
		c.Add(ns)
	} else {
		sum := &atomic.Int64{}
		sum.Add(ns)
		m.attemptLatencyNSByEnd[key] = sum
	}
	h, exists := m.attemptHistByEnd[key]
	if !exists {
		h = make([]atomic.Int64, len(m.attemptLatencyBounds)+1) // +1 for +Inf
		m.attemptHistByEnd[key] = h
	}
	// The overflow bucket gets its own (empty) histogram the moment it is
	// created. rollUpToOverflowLocked admits it into the counter maps partway
	// through this same critical section, and that path deliberately does not
	// touch the histogram — so without this the bucket ends up in
	// attemptsByEndpoint with no histogram at all, and /metrics prints a
	// zeroed duration distribution for it that is not the one it accumulated.
	// The bucket's own buckets stay empty until something is recorded directly
	// against the bucket label; its count and sum come from the rolled-up
	// counters, which is documented in the exporter.
	if bh := m.attemptHistByEnd[m.overflowKey]; bh == nil {
		if _, live := m.attemptsByEndpoint[m.overflowKey]; live {
			m.attemptHistByEnd[m.overflowKey] = make([]atomic.Int64, len(m.attemptLatencyBounds)+1)
		}
	}
	m.mu.Unlock()

	h[idx].Add(1)
}

// boundedAttemptKey admits one new attempt label and returns the key it was
// actually stored under — the raw endpoint below the cap, overflowLabelValue at
// or above it.
//
// Caller must hold m.mu (write). It must be called EXACTLY once per Attempt,
// before any of the attempt maps are touched, and the returned key is what all
// of them must be indexed by. An earlier version bounded each map independently
// per map, which was wrong twice over: the bounded value never
// propagated back to the caller, so the histogram map was still keyed by the
// raw label and grew without bound; and bounding each map separately let the
// maps disagree at the boundary, which could produce an endpoint with attempt
// counts but no latency histogram. Deciding once, up front, makes the key set
// identical across all four maps by construction and needs no further
// coordination.
//
// Requires the write lock, which is what makes admission atomic with respect to
// eviction: a label cannot be evicted between being admitted and being updated.
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
// to put the observation). The cost is labelsCap-1 individually-named endpoints.
//
// The window is FIFO, not "first come, forever". The first implementation
// admitted labels and never released them, which is correct for memory but wrong
// for meaning: because label values come from config and config is hot-reloadable,
// a burst of labels from a config that is no longer live holds its slots
// permanently, and a genuinely hot endpoint that first appears afterwards is
// collapsed into __overflow__ for the remaining life of the process. The gateway
// would report a broken upstream as "no data" indefinitely, which is the worst
// possible failure for the series that exist to answer "is this upstream
// healthy?". So a new value recycles the least-recently-admitted slot.
//
// "Least recently admitted", not "least recently used". Reordering on use would
// need a read-lock-and-bump on every single observation — a global write lock on
// the hot path, for a list of a few hundred strings. Admission order is a good
// proxy: a label that was admitted long ago and is being used right now is one
// whose config was just re-added, which is rare, and it will be re-admitted with
// its history on the next miss regardless.
//
// Evicting rolls the victim's counters into __overflow__ rather than dropping
// them, so the totals stay truthful and airouter_metrics_label_overflow_total
// remains the single place that says "some of this is aggregated".
func (m *Metrics) boundedAttemptKeyLocked(endpoint labelKey) labelKey {
	if _, tracked := m.attemptsByEndpoint[endpoint]; tracked {
		return endpoint
	}
	// Room for a real value plus the reserved slot for the bucket.
	if len(m.attemptsByEndpoint) < m.labelsCap-1 {
		m.admitLocked(endpoint)
		return endpoint
	}
	// The bucket itself is a legitimate label that any caller may submit, and
	// it is counted in its own right when it is. It is never recycled.
	if endpoint == m.overflowKey {
		m.admitLocked(endpoint)
		return endpoint
	}
	// The window is full and this is a genuinely new value. Recycle the slot of
	// the least-recently-admitted label, and roll its counters into the bucket
	// first so nothing is lost: the endpoint is disappearing from the output,
	// but its observations stay in the totals.
	victim := m.oldestAdmittedLocked()
	if victim == "" {
		// No recyclable slot at all. Collapse rather than drop, and count it so
		// the condition is visible. Reaching here means the ring and the map
		// have drifted apart, which is a bug rather than a normal state — hence
		// labelsOverflowed rather than silently admitting a cap+1 key.
		m.labelsOverflowed.Add(1)
		m.admitLocked(m.overflowKey)
		return m.overflowKey
	}
	m.rollUpToOverflowLocked(victim)
	m.labelsEvicted.Add(1)
	m.admitLocked(endpoint)
	return endpoint
}

// admitLocked records endpoint in the FIFO admission window and returns its
// attempt counter, creating it if needed. Caller holds m.mu.
//
// Idempotent in the sense that matters: if the label is already present its
// counter is returned without consuming a new ring slot, so the window cannot
// be padded with duplicates of a label that is already tracked.
func (m *Metrics) admitLocked(endpoint labelKey) *atomic.Int64 {
	if c, exists := m.attemptsByEndpoint[endpoint]; exists {
		return c
	}
	// The overflow bucket is pinned outside the ring. It is where every
	// recycled label's history goes, so it must never be a recycling candidate
	// itself — otherwise it gets evicted into itself, which double-counts.
	//
	// Keeping it out of the ring entirely (rather than merely skipping it when
	// picking a victim) is what makes the window size predictable: the ring
	// holds exactly the real labels, len(ring) == len(attemptsByEndpoint)-1
	// once the bucket exists. The first version appended it to the ring and
	// filtered it at selection time, which meant the ring held one entry too
	// many and a single eviction reported two.
	if endpoint == m.overflowKey {
		c := &atomic.Int64{}
		m.attemptsByEndpoint[endpoint] = c
		return c
	}
	m.attemptClock++
	m.attemptAdmitted = append(m.attemptAdmitted, m.attemptClock)
	m.attemptRing = append(m.attemptRing, endpoint)
	m.attemptIndex[endpoint] = len(m.attemptRing) - 1
	c := &atomic.Int64{}
	m.attemptsByEndpoint[endpoint] = c
	return c
}

// oldestAdmittedLocked returns the label whose slot is next to be recycled, or
// "" if the window has no recyclable entry. Caller holds m.mu.
//
// The overflow bucket is skipped: it is the one label that must never be
// displaced, because it is where everything being displaced goes. Recycling it
// would drop observations on the floor and could hand the same label both a real
// series and a bucket role.
func (m *Metrics) oldestAdmittedLocked() labelKey {
	for i, ep := range m.attemptRing {
		if i >= len(m.attemptAdmitted) {
			break
		}
		if ep == m.overflowKey {
			continue
		}
		if _, live := m.attemptsByEndpoint[ep]; !live {
			// Stale ring entry whose label was already removed; skip it.
			m.compactRingLocked(i)
			return m.oldestAdmittedLocked()
		}
		return ep
	}
	return ""
}

// compactRingLocked drops ring/index entry i, keeping attemptAdmitted aligned
// with attemptRing. Caller holds m.mu.
func (m *Metrics) compactRingLocked(i int) {
	ep := m.attemptRing[i]
	delete(m.attemptIndex, ep)
	m.attemptRing = append(m.attemptRing[:i], m.attemptRing[i+1:]...)
	m.attemptAdmitted = append(m.attemptAdmitted[:i], m.attemptAdmitted[i+1:]...)
	for j := i; j < len(m.attemptRing); j++ {
		m.attemptIndex[m.attemptRing[j]] = j
	}
}

// recycleLocked removes victim's series from all four attempt maps, rolling its
// counters into the overflow bucket so no observation is lost. Caller holds m.mu.
func (m *Metrics) rollUpToOverflowLocked(victim labelKey) {
	if victim == m.overflowKey {
		return
	}
	// The bucket is created through the normal admission path, not poked
	// directly into one map. The first version added it to attemptsByEndpoint
	// alone, which produced a series with an attempt count but no histogram and
	// no ring entry — so the boundary test caught an __overflow__ that would
	// print a zeroed duration distribution in /metrics, and whose slot the ring
	// did not know about.
	bucket := m.admitLocked(m.overflowKey)

	// Attempt counts and the latency sum fold in. The histogram does NOT, and
	// cannot: the buckets are cumulative-free, so merging a victim's per-bucket
	// counts into the bucket's would be exact arithmetic on data whose meaning
	// is now "some unknown endpoint", and there is no way to represent "the
	// bucket had 40 observations in the 0.25s band, all of them from a label we
	// evicted" honestly. The sum and count carry the truth; the bucket's
	// histogram deliberately reports only observations recorded directly against
	// the bucket itself, which is why _count can legitimately be lower than
	// _sum implies. Documented in the exporter.
	if c, ok := m.attemptsByEndpoint[victim]; ok {
		bucket.Add(c.Load())
		delete(m.attemptsByEndpoint, victim)
	}
	// Create-on-write for the bucket entries too. Reading first and only
	// adding when the entry happened to exist (the first version) dropped the
	// rollup on the floor whenever the victim was the first label of its kind —
	// which is every label that failed on its only attempt — so the bucket's
	// failure and latency totals silently under-reported.
	if c, ok := m.attemptFailuresByEnd[victim]; ok {
		bc := m.attemptFailuresByEnd[m.overflowKey]
		if bc == nil {
			bc = &atomic.Int64{}
			m.attemptFailuresByEnd[m.overflowKey] = bc
		}
		bc.Add(c.Load())
		delete(m.attemptFailuresByEnd, victim)
	}
	if c, ok := m.attemptLatencyNSByEnd[victim]; ok {
		bc := m.attemptLatencyNSByEnd[m.overflowKey]
		if bc == nil {
			bc = &atomic.Int64{}
			m.attemptLatencyNSByEnd[m.overflowKey] = bc
		}
		bc.Add(c.Load())
		delete(m.attemptLatencyNSByEnd, victim)
	}
	delete(m.attemptHistByEnd, victim)
	if i, ok := m.attemptIndex[victim]; ok {
		m.compactRingLocked(i)
	}
}

// boundLabelValue maps a raw label value onto the value that will actually be
// used as a map key, folding unknown values into a single overflow bucket once
// the map is full. Used by the single-key maps (per-model requests, per-endpoint
// fallbacks, circuit transitions); the attempt telemetry decides once up front
// in boundedAttemptKeyLocked instead, because it spans four maps that must
// agree and has to stay atomic against eviction.
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
//
// The collapse target is a well-formed one-label key carrying overflowLabelValue
// under the SAME label name the map uses — status maps collapse to
// status="__overflow__", model maps to model="__overflow__", and so on. Two
// reasons, and the second is the one that matters:
//
//   - It is what an operator expects. Searching the exposition for
//     __overflow__ finds every bucket, wherever it lives, and each one is
//     labelled by the dimension it summarises.
//   - Collapsing a status map into endpoint="__overflow__" silently changes the
//     metric's label schema. Prometheus treats a label-set change as a new
//     series, so the bucket would not continue the series an alert on
//     failures_by_status is watching; it would start a new one, and a query
//     that groups by status would lose the bucket entirely. The cap is a
//     safety valve and must not alter the schema of healthy data.
//
// The overflowKey field exists for the attempt maps, which recycle rather than
// collapse; these use the per-map name.
// admitCollapsible returns the counter for key in store, creating it if needed,
// collapsing to the overflow bucket when the map is at capacity.
//
// The whole decision happens under ONE write lock, and that is the fix rather
// than a stylistic choice. The first version read the map and the size under a
// read lock, released it, and only then took the write lock to insert — so N
// goroutines with N brand-new labels could all observe "there is room" before
// any of them inserted, and all N would insert. The map reached cap+N, which
// is not a cap: the memory bound this file exists to provide silently did not
// apply under exactly the load that would generate the most labels. Every
// collapsing map had this shape, and a test that asserted len(store) <= cap
// after the workers had finished passed against it every time, because the
// last insertion to complete is usually followed by nothing to shrink it back
// — the overshoot is what is left.
//
// It is the same defect the attempt map had (see Attempt) and the same fix.
// The lock is uncontended in the common case: a key that is already tracked
// takes the fast path and never mutates.
//
// The read lock is still worth taking first for that fast path, so a scrape
// storm on existing labels does not serialise behind a write lock.
func (m *Metrics) admitCollapsible(store map[labelKey]*atomic.Int64, key, labelName labelKey) *atomic.Int64 {
	m.mu.RLock()
	c, tracked := store[key]
	m.mu.RUnlock()
	if tracked {
		return c
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	// Re-check: another goroutine may have inserted this exact key, or the map
	// may have filled, between the read lock above and here.
	if c, tracked := store[key]; tracked {
		return c
	}
	// Same slot reservation as boundedAttemptKeyLocked, for the same reason:
	// without it the map reaches cap+1 by letting the bucket in on top.
	key = m.boundedCollapsibleKeyLocked(store, key, labelName)
	if c, tracked := store[key]; tracked {
		return c
	}
	c = &atomic.Int64{}
	store[key] = c
	return c
}

// boundedCollapsibleKeyLocked decides which key a new label is stored under,
// given that the caller already holds the write lock and has established the
// key is not present.
func (m *Metrics) boundedCollapsibleKeyLocked(store map[labelKey]*atomic.Int64, key, labelName labelKey) labelKey {
	if len(store) < m.labelsCap-1 {
		return key
	}
	// The bucket is a legitimate label in its own right: a caller that submits
	// overflowLabelValue directly is counted as itself, not folded in again.
	if key == overflowKeyFor(labelName) {
		return key
	}
	m.labelsOverflowed.Add(1)
	return overflowKeyFor(labelName)
}

// overflowKeyFor is the bucket key for a given label name. One map may have
// several label names sharing a store (requests_total is keyed by model and by
// status), so the name is threaded through rather than inferred.
func overflowKeyFor(labelName labelKey) labelKey {
	return bareKey(string(labelName), overflowLabelValue)
}

// labelKey is a *pre-rendered, fully escaped label set* — the exact bytes that
// go between the braces of a sample line — used as the key of a bounded label
// map.
//
// Every bounded map keys on this type, and the exporter writes the key out
// verbatim inside the braces. That is the whole point, and it is the opposite
// of what the first implementation did, which cost a real outage-shaped bug.
//
// The two shapes that used to coexist:
//
//   - requestsByModel, fallbacksByFromEndpoint, attemptsByEndpoint keyed on a
//     BARE value ("p:m") and escaped it at export time.
//   - circuitTransitions keyed on a PRE-RENDERED label set
//     (`from="..",to="..",endpoint=".."`) that the exporter spliced in raw,
//     because escaping had already happened when the key was built.
//
// The second shape means the map's key is no longer a label *value* but a
// whole label *set*, so the overflow bucket — which is a single bare value —
// no longer fits the key space. An at-cap transition was therefore stored under
// the bare key `__overflow__` and printed as
//
//	airouter_circuit_state_transitions{__overflow__} 4
//
// which is not a legal sample. A Prometheus scrape is all-or-nothing, so
// reaching the cardinality cap would have taken down every dashboard, alert and
// recording rule on the instance. The cap was worse than the bug it fixed.
//
// Collapsing both conventions into one fixes the class, not the instance. A
// bare value becomes a one-element label set rendered the same way as a
// three-element one, so the overflow bucket is a legal key in every map and
// needs no special case in the exporter at all. Adding a map now means writing
// a render function that cannot be forgotten, because the key type will not
// compile without one.
//
// labelKeys does NOT re-escape: values are escaped when the key is built
// (labelEscape at each call site), which is why export is a plain write.
type labelKey string

// singleLabelKey builds a key for a one-label series, e.g. `endpoint="p:m"`.
//
// The value is escaped exactly once, here, at the point the key is built — and
// nowhere else, because the exporter writes keys verbatim. The first version of
// this also escaped at export time, which double-escaped every label: the
// sample stayed valid and safe, so nothing failed and no dashboard broke, but
// every label containing a quote or newline came back to the scraper with a
// stray backslash and could never be matched against the config that produced
// it. Silent data corruption is exactly what a green test suite does not catch,
// so the round-trip equality in TestMetricsLabelValuesCannotForgeSeries is the
// assertion that matters here, not "it parses".
func singleLabelKey(name, value string) labelKey {
	return bareKey(name, labelEscape(value))
}

// bareKey builds a key for a one-label series from a value that is already in
// its final escaped form — the overflow bucket, which is a constant with
// nothing to escape. Kept separate from singleLabelKey so the constant cannot
// be made to double-escape if it ever gains a special character.
//
// It concatenates the quotes rather than using %q. %q is Go string quoting,
// not Prometheus label quoting: it applies the *same* escaping rules plus Go's
// own \x and \u handling, so feeding it an already-escaped value escapes the
// backslashes a second time. The result still parses and is still perfectly
// safe — it just reports a label value that is not the one configured, which
// no parse check will ever flag. That is the whole reason this is a
// hand-written concatenation with a comment.
func bareKey(name, escapedValue string) labelKey {
	return labelKey(name + `="` + escapedValue + `"`)
}

// circuitTransitionKey builds the key for a (from, to, endpoint) transition.
// from.String()/to.String() rather than string(from): CircuitState is an
// int-backed enum, so a direct conversion emits the rune for the state number
// ("\x00", "\x01") instead of "closed"/"open"/"half-open". The String form is
// closed over four values so it needs no escaping, but it is escaped anyway so
// this stays correct if that ever changes.
func circuitTransitionKey(from, to CircuitState, endpoint string) labelKey {
	return bareKey("from", labelEscape(from.String())) + "," +
		bareKey("to", labelEscape(to.String())) + "," +
		bareKey("endpoint", labelEscape(endpoint))
}

// writeSample writes one already-labelled integer series. Every bounded map's
// export goes through here so the shape `name{key} value` is written in
// exactly one place.
func writeSample(w io.Writer, name string, k labelKey, value int64) {
	fmt.Fprintf(w, "%s{%s} %d\n", name, k, value)
}

// writeSampleFloat is writeSample for a float value (histogram sums). The
// value is formatted with %f, matching what the exposition has always
// emitted; it is NOT routed through an int, because a nanosecond sum has more
// precision than that and the sum is the one number a latency SLO is computed
// from.
func writeSampleFloat(w io.Writer, name string, k labelKey, value float64) {
	fmt.Fprintf(w, "%s{%s} %f\n", name, k, value)
}

// writeBucket writes one histogram bucket sample: the key's label set with
// `le="<bound>"` appended, which is the only legal way to extend a
// pre-rendered key. Kept next to writeSample so that composing a key lives in
// one place and a caller cannot hand-concatenate braces.
//
// bound is already-formatted (the caller passes fmt.Sprintf("%g", bound) or
// "+Inf"); it is not escaped, because the only values that reach it are
// float64 boundaries this package chose and the literal "+Inf".
func writeBucket(w io.Writer, name string, k labelKey, bound string, value int64) {
	fmt.Fprintf(w, "%s{%s,le=\"%s\"} %d\n", name, k, bound, value)
}

// labelEscape escapes a label value for the Prometheus text exposition format.
//
// Every label value comes from config, and config is writable through
// HandleAdminConfig and rewritten on disk by the model-sync job, so escaping is
// a real injection barrier and not a theoretical one. A provider named
// `x" 1\nairouter_requests_total{model="victim` interpolated unescaped produces
// two syntactically valid series on the next scrape, and an operator or a
// downstream scraper reads both as genuine. The exposition format requires
// escaping backslash, double quote, and newline; everything else is literal.
//
// Call this EXACTLY ONCE per value. Keys are pre-rendered at record time and
// the exporter writes them verbatim, so escaping a value twice produces a
// sample that is still valid and still safe — which is what made the double
// escape survive review. It corrupts the data instead: the label a scraper
// reads back is `x\" 1\n...` rather than the provider name that is actually
// configured, so a series is permanently unmatchable against the config that
// produced it. The round-trip assertion in
// TestMetricsLabelValuesCannotForgeSeries is what caught it; a prefix or
// contains check does not, because an over-escaped value still has the original
// as a substring.
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
		writeSample(w, "airouter_requests_total", k, c.Load())
	}
	for k, c := range m.requestsByStatus {
		writeSample(w, "airouter_requests_total", k, c.Load())
	}
	m.mu.RUnlock()

	fmt.Fprintln(w, "# HELP airouter_failures_total Total failures by status code")
	fmt.Fprintln(w, "# TYPE airouter_failures_total counter")

	m.mu.RLock()
	for k, c := range m.failuresByStatus {
		writeSample(w, "airouter_failures_total", k, c.Load())
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
		writeSample(w, "airouter_fallbacks_total_by_endpoint", k, c.Load())
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
		// No special case for the overflow bucket: it is a well-formed one-label
		// key like every other, so it writes out through the same path. That
		// used to be the one line in this function that could emit an illegal
		// sample — see labelKey.
		writeSample(w, "airouter_circuit_state_transitions", k, c.Load())
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

	// Labels that lost their slot to a newer label, with their history rolled
	// into the bucket. A one-off rise is a normal config change (an endpoint
	// renamed or removed); a steady climb means the cap is smaller than the
	// deployment's real label set and should be raised via
	// preferences.max_label_cardinality (see "Tuning the label cap" in the
	// RUNBOOK).
	fmt.Fprintln(w, "# HELP airouter_metrics_label_evictions_total Labels displaced from the registry to admit a newer one")
	fmt.Fprintln(w, "# TYPE airouter_metrics_label_evictions_total counter")
	fmt.Fprintf(w, "airouter_metrics_label_evictions_total %d\n", m.labelsEvicted.Load())

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
	attempts := make(map[labelKey]int64, len(m.attemptsByEndpoint))
	for k, c := range m.attemptsByEndpoint {
		attempts[k] = c.Load()
	}
	failures := make(map[labelKey]int64, len(m.attemptFailuresByEnd))
	for k, c := range m.attemptFailuresByEnd {
		failures[k] = c.Load()
	}
	latSum := make(map[labelKey]int64, len(m.attemptLatencyNSByEnd))
	for k, c := range m.attemptLatencyNSByEnd {
		latSum[k] = c.Load()
	}
	hists := make(map[labelKey][]int64, len(m.attemptHistByEnd))
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
	endpoints := make([]labelKey, 0, len(attempts))
	for k := range attempts {
		endpoints = append(endpoints, k)
	}
	sort.Slice(endpoints, func(i, j int) bool { return endpoints[i] < endpoints[j] })

	for _, ep := range endpoints {
		n := attempts[ep]
		writeSample(w, "airouter_endpoint_attempts_total", ep, n)
		writeSample(w, "airouter_endpoint_attempt_failures_total", ep, failures[ep])

		sum := float64(latSum[ep]) / 1e9

		// The overflow bucket gets NO histogram, and the reason is not
		// caution about accuracy — it is that the alternative is actively
		// corrupt output.
		//
		// Evicting a label rolls its attempt count, failure count and latency
		// sum into the bucket, but its per-band data cannot be rolled in: once
		// the endpoint is gone, "the 0.25s band held 40 observations, all from
		// a label we evicted" is not representable. So the bucket has a count
		// and a sum and no distribution.
		//
		// Emitting the histogram anyway produced this, which is what a scrape
		// actually served:
		//
		//	..._bucket{endpoint="__overflow__",le="0.25"} 0
		//	..._bucket{endpoint="__overflow__",le="45"}   0
		//	..._bucket{endpoint="__overflow__",le="+Inf"} 11
		//	..._sum{endpoint="__overflow__"} 0.110000
		//	..._count{endpoint="__overflow__"} 11
		//
		// That is not a histogram. A Prometheus histogram is cumulative and its
		// buckets are monotonic, so every finite bound must be <= +Inf; here
		// the largest finite bucket (0) is below it, and the 11 observations
		// are claimed to be entirely above 45s when the real sum says they
		// averaged 10ms. Nothing rejects it: the scrape parses, the series
		// type is declared histogram, and every dashboard is green.
		//
		// histogram_quantile over that returns a number with no relationship
		// to reality, and rate(_sum)/rate(_count) returns an average
		// computed against a distribution that does not exist. The failure is
		// silent and the number looks plausible, which is the worst kind.
		//
		// So the bucket reports as a counter and a gauge instead, under names
		// that say what they are and that no quantile function will accept:
		//
		//	airouter_endpoint_attempts_total          (already emitted)
		//	airouter_evicted_attempt_latency_seconds_sum  (gauge)
		//	airouter_evicted_attempt_latency_seconds_count (gauge)
		//
		// The count is a gauge, not a counter, for the same reason: it can
		// change without the process doing anything, because an eviction
		// ADDS to it, and a rate() over a series that decreases is worse than
		// no series. An operator who wants the mean latency of evicted
		// attempts divides the two by hand, which is correct precisely because
		// both are honest totals.
		if ep == m.overflowKey {
			continue
		}
		h := hists[ep]
		var cumulative int64
		for i, bound := range m.attemptLatencyBounds {
			if i < len(h) {
				cumulative += h[i]
			}
			writeBucket(w, "airouter_endpoint_attempt_duration_seconds_bucket", ep, fmt.Sprintf("%g", bound), cumulative)
		}
		// +Inf is the sum of the finite buckets for a real label, which equals
		// the attempt count because every observation lands in exactly one
		// band. Read from the histogram rather than from `n` so the
		// monotonicity of the series is structural rather than a coincidence.
		writeBucket(w, "airouter_endpoint_attempt_duration_seconds_bucket", ep, "+Inf", cumulative)
		writeSampleFloat(w, "airouter_endpoint_attempt_duration_seconds_sum", ep, sum)
		writeSample(w, "airouter_endpoint_attempt_duration_seconds_count", ep, n)
	}
	m.serveEvictedLatency(w, endpoints, latSum)
}

// serveEvictedLatency exports the overflow bucket's latency totals under names
// that cannot be mistaken for a histogram.
//
// This is the mitigation for the problem described in serveAttemptMetrics: the
// bucket's distribution is unknowable, so it is not exported as one. The names
// are deliberately unlike the histogram's — no _bucket, no _sum/_count suffix
// attached to a _seconds histogram name — because the whole failure mode was a
// quantile function accepting the data. A gauge that does not look like a
// histogram cannot be fed to histogram_quantile by accident.
//
// Both series are emitted for every endpoint, not just the bucket, so a query
// written as "attempts minus evicted" or a ratio against the total works
// without a special case, and so the metric does not appear and disappear as
// the cap is hit.
func (m *Metrics) serveEvictedLatency(w http.ResponseWriter, endpoints []labelKey, latSum map[labelKey]int64) {
	fmt.Fprintln(w, "# HELP airouter_evicted_attempts_latency_seconds_sum Latency of attempts whose endpoint was evicted from the metrics window. NOT a histogram: the per-latency-band distribution is unknown and is not reported, so do not use histogram_quantile on this.")
	fmt.Fprintln(w, "# TYPE airouter_evicted_attempts_latency_seconds_sum gauge")
	fmt.Fprintln(w, "# HELP airouter_evicted_attempts_latency_seconds_count Number of attempts whose endpoint was evicted from the metrics window. NOT a histogram: see the _sum metric.")
	fmt.Fprintln(w, "# TYPE airouter_evicted_attempts_latency_seconds_count gauge")

	m.mu.RLock()
	bucketAttempts := int64(0)
	if c, ok := m.attemptsByEndpoint[m.overflowKey]; ok {
		bucketAttempts = c.Load()
	}
	m.mu.RUnlock()

	for _, ep := range endpoints {
		var sum float64
		var count int64
		if ep == m.overflowKey {
			sum = float64(latSum[ep]) / 1e9
			count = bucketAttempts
		}
		writeSampleFloat(w, "airouter_evicted_attempts_latency_seconds_sum", ep, sum)
		writeSample(w, "airouter_evicted_attempts_latency_seconds_count", ep, count)
	}
}
