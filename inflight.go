package main

import (
	"sort"
	"sync"
	"time"
)

// inflightSession is one client session whose request the gateway
// is serving right now: which logical model's chain is handling
// it, which upstream endpoint is generating tokens for it, and
// when the session entered the inflight set.
type inflightSession struct {
	model    string
	endpoint string
	since    time.Time
	// refs counts the requests currently in flight for this
	// session. A duplicate client retry re-sends the same
	// body, which hashes to the same session ID; without the
	// count the first request to finish would untrack a
	// session its sibling is still serving.
	refs int
}

// inflightTracker tracks the sessions currently being served,
// keyed by session ID. It is the live counterpart of the router's
// session map: the router remembers which endpoint a session
// STICKS to across requests (sessionTTL), while this remembers
// which sessions are being served THIS instant — the difference
// between "has a session" and "is generating tokens right now".
//
// The tracker is deliberately request-scoped rather than
// attempt-scoped. A request walking its fallback chain — waiting
// out a cooldown, retrying the next endpoint — is still being
// served by its model, and counting it only while an upstream
// call is in progress would make the panel flicker to zero
// between attempts. The endpoint field is refreshed on every
// attempt so the breakdown shows where tokens are being generated
// now.
type inflightTracker struct {
	mu       sync.RWMutex
	sessions map[string]*inflightSession
}

func newInflightTracker() *inflightTracker {
	return &inflightTracker{sessions: map[string]*inflightSession{}}
}

// Track marks a session as being served by a model. Every method
// on a nil tracker is a no-op: the request path must never panic
// because a tracker was not wired, the same fail-open posture the
// metrics field takes.
func (t *inflightTracker) Track(sessionID, model, endpoint string) {
	if t == nil || sessionID == "" || model == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.sessions[sessionID]; ok {
		s.refs++
		s.model = model
		if endpoint != "" {
			s.endpoint = endpoint
		}
		return
	}
	t.sessions[sessionID] = &inflightSession{
		model: model, endpoint: endpoint, since: time.Now(), refs: 1,
	}
}

// UpdateEndpoint records which upstream endpoint is currently
// generating for a session. A session that falls back mid-request
// moves between endpoints, and the dashboard should show where it
// is being served now, not where it started.
func (t *inflightTracker) UpdateEndpoint(sessionID, endpoint string) {
	if t == nil || sessionID == "" || endpoint == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.sessions[sessionID]; ok {
		s.endpoint = endpoint
	}
}

// Untrack removes one request's claim on a session. Idempotent:
// the request path calls it from a defer, and a session that was
// never tracked is a no-op rather than a panic.
func (t *inflightTracker) Untrack(sessionID string) {
	if t == nil || sessionID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.sessions[sessionID]
	if !ok {
		return
	}
	s.refs--
	if s.refs <= 0 {
		delete(t.sessions, sessionID)
	}
}

// Snapshot aggregates the inflight sessions per logical model, most
// sessions first, with the endpoints serving them (also most
// sessions first) and the instant the oldest session started. It
// always returns a non-nil slice so the payload emits [] rather
// than null when nothing is in flight.
func (t *inflightTracker) Snapshot() []dashboardInflightModel {
	if t == nil {
		return []dashboardInflightModel{}
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	type agg struct {
		sessions  int
		since     time.Time
		endpoints map[string]int
	}
	byModel := make(map[string]*agg)
	for _, s := range t.sessions {
		a, ok := byModel[s.model]
		if !ok {
			a = &agg{endpoints: map[string]int{}}
			byModel[s.model] = a
		}
		a.sessions++
		if a.since.IsZero() || s.since.Before(a.since) {
			a.since = s.since
		}
		if s.endpoint != "" {
			a.endpoints[s.endpoint]++
		}
	}
	out := make([]dashboardInflightModel, 0, len(byModel))
	for model, a := range byModel {
		eps := make([]dashboardInflightEndpoint, 0, len(a.endpoints))
		for ep, n := range a.endpoints {
			eps = append(eps, dashboardInflightEndpoint{Endpoint: ep, Sessions: n})
		}
		sort.Slice(eps, func(i, j int) bool { return eps[i].Sessions > eps[j].Sessions })
		out = append(out, dashboardInflightModel{
			Model: model, Sessions: a.sessions, Endpoints: eps, Since: a.since,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Sessions > out[j].Sessions })
	return out
}
