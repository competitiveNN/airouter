package main

import (
	"sort"
	"sync"
	"time"
)

// inflightSession is one client session that is alive for the dashboard.
// It stores which logical model's chain is handling it, which upstream
// endpoint is generating tokens for it right now, when it first entered
// the set, and when it was last touched. A session stays alive for
// sessionTTL after its last touch, matching the router's session map: the
// dashboard shows sessions that exist and are being served by their sticky
// model, not only sessions whose upstream call happens to be mid-stream at
// the snapshot instant.
type inflightSession struct {
	model    string
	endpoint string
	since    time.Time
	lastUsed time.Time
}

// inflightTracker tracks the sessions currently alive for the dashboard,
// keyed by session ID. It is the live counterpart of the router's session
// map: the router remembers which endpoint a session STICKS to across
// requests (sessionTTL), while this remembers which sessions are alive
// right now and where tokens are being generated for them.
//
// Tracking is intentionally request-scoped-in-time but not
// request-lifetime-scoped: every request for a session re-registers it and
// bumps lastUsed, so the session remains on the panel until it has been
// idle for sessionTTL. A request walking its fallback chain — waiting out
// a cooldown, retrying the next endpoint — is still being served by its
// model, so tracking it while any upstream call is in progress keeps the
// panel from flickering to zero between attempts. The endpoint field is
// refreshed on every attempt so the breakdown shows where tokens are being
// generated now.
//
// Untrack no longer removes a session: a request finishing does not mean
// the conversation is over. Sessions leave the set only when they have
// been idle for sessionTTL, matching the router's own session TTL. This
// keeps the dashboard honest about "how many live sessions each model is
// serving" rather than "how many upstream calls are mid-flight".
type inflightTracker struct {
	mu       sync.RWMutex
	ttl      time.Duration
	sessions map[string]*inflightSession
}

func newInflightTracker() *inflightTracker {
	return &inflightTracker{
		ttl:      sessionTTL,
		sessions: map[string]*inflightSession{},
	}
}

// Track marks a session as alive, served by a model. Every call re-touches
// the session so its TTL clock restarts. Every method on a nil tracker is
// a no-op: the request path must never panic because a tracker was not
// wired.
func (t *inflightTracker) Track(sessionID, model, endpoint string) {
	if t == nil || sessionID == "" || model == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.sessions[sessionID]; ok {
		s.model = model
		s.endpoint = endpoint
		s.lastUsed = time.Now()
		return
	}
	t.sessions[sessionID] = &inflightSession{
		model:    model,
		endpoint: endpoint,
		since:    time.Now(),
		lastUsed: time.Now(),
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
		s.lastUsed = time.Now()
	}
}

// Untrack marks one request as no longer actively streaming for a session.
// It no longer removes the session: the conversation is alive until it has
// been idle for sessionTTL, matching the router's session map. Calling it
// for a session that was never tracked is a no-op rather than a panic.
// Idempotent: the request path calls it from a defer and may call it more
// than once.
func (t *inflightTracker) Untrack(sessionID string) {
	if t == nil || sessionID == "" {
		return
	}
	// Sessions remain alive for sessionTTL after their last touch, so an
	// active request finishing does not empty the panel. Nothing to do
	// here — Track already recorded lastUsed.
}

// Sweep removes sessions that have been idle for longer than the TTL,
// so an operator leaving the dashboard open does not accumulate dead
// sessions forever. Caller must hold t.mu (write).
func (t *inflightTracker) sweepLocked() {
	if t.ttl <= 0 {
		t.sessions = map[string]*inflightSession{}
		return
	}
	now := time.Now()
	for id, s := range t.sessions {
		if now.Sub(s.lastUsed) > t.ttl {
			delete(t.sessions, id)
		}
	}
}

// Snapshot aggregates the live sessions per logical model, most sessions
// first, with the endpoints serving them (also most sessions first), the
// instant the oldest session started, and the cumulative lifetime of the
// sessions: the sum, over the live sessions of that model, of how long
// each has been alive. A model serving two conversations that started an
// hour and two hours ago reports a three-hour cumulative lifetime even
// though its oldest session is only two hours old; the two numbers answer
// different questions ("when did this model's current work begin" versus
// "how much session time is the model holding"). Expired sessions are
// evicted from the snapshot, so the panel reflects live sessions, not
// history. It always returns a non-nil slice so the payload emits []
// rather than null when nothing is in flight.
func (t *inflightTracker) Snapshot() []dashboardInflightModel {
	if t == nil {
		return []dashboardInflightModel{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	t.sweepLocked()
	type agg struct {
		sessions       int
		since          time.Time
		lastUsed       time.Time
		lifetime       time.Duration
		endpoints      map[string]int
		sessionDetails []dashboardInflightSession
	}
	byModel := make(map[string]*agg)
	for id, s := range t.sessions {
		a, ok := byModel[s.model]
		if !ok {
			a = &agg{endpoints: map[string]int{}}
			byModel[s.model] = a
		}
		a.sessions++
		if a.since.IsZero() || s.since.Before(a.since) {
			a.since = s.since
		}
		if a.lastUsed.IsZero() || s.lastUsed.After(a.lastUsed) {
			a.lastUsed = s.lastUsed
		}
		// since is never in the future for a tracked session, but a
		// clock jump could make it look so; clamp at zero rather than
		// emitting a negative lifetime.
		if age := now.Sub(s.since); age > 0 {
			a.lifetime += age
		}
		if s.endpoint != "" {
			a.endpoints[s.endpoint]++
		}
		age := now.Sub(s.since)
		if age < 0 {
			age = 0
		}
		a.sessionDetails = append(a.sessionDetails, dashboardInflightSession{
			ID:              id,
			Session:         id,
			SessionID:       id,
			Endpoint:        s.endpoint,
			Since:           s.since,
			LastUsed:        s.lastUsed,
			Lifetime:        age.Seconds(),
			LifetimeSeconds: age.Seconds(),
			Age:             age.Seconds(),
			AgeSeconds:      age.Seconds(),
		})
	}
	out := make([]dashboardInflightModel, 0, len(byModel))
	for model, a := range byModel {
		eps := make([]dashboardInflightEndpoint, 0, len(a.endpoints))
		for ep, n := range a.endpoints {
			eps = append(eps, dashboardInflightEndpoint{Endpoint: ep, Sessions: n})
		}
		sort.Slice(eps, func(i, j int) bool { return eps[i].Sessions > eps[j].Sessions })
		sort.Slice(a.sessionDetails, func(i, j int) bool { return a.sessionDetails[i].ID < a.sessionDetails[j].ID })
		ids := make([]string, 0, len(a.sessionDetails))
		lives := make([]float64, 0, len(a.sessionDetails))
		for _, sd := range a.sessionDetails {
			ids = append(ids, sd.ID)
			lives = append(lives, sd.LifetimeSeconds)
		}
		out = append(out, dashboardInflightModel{
			Model:                     model,
			Sessions:                  a.sessions,
			Endpoints:                 eps,
			Since:                     a.since,
			CumulativeLifetimeSeconds: a.lifetime.Seconds(),
			SessionsDetail:            a.sessionDetails,
			SessionDetails:            a.sessionDetails,
			SessionsList:              a.sessionDetails,
			SessionList:               a.sessionDetails,
			SessionsExpanded:          a.sessionDetails,
			SessionsBreakdown:         a.sessionDetails,
			Detail:                    a.sessionDetails,
			Details:                   a.sessionDetails,
			SessionRows:               a.sessionDetails,
			Rows:                      a.sessionDetails,
			Members:                   a.sessionDetails,
			Items:                     a.sessionDetails,
			SessionIDs:                ids,
			SessionLifetimes:          lives,
			IDs:                       ids,
			Lifetimes:                 lives,
			Ages:                      lives,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Sessions > out[j].Sessions })
	return out
}
