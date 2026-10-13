package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestInflightTrackerAggregatesSessionsPerModel(t *testing.T) {
	tr := newInflightTracker()
	tr.Track("s1", "smart", "fake:smart-a")
	tr.Track("s2", "smart", "fake:smart-b")
	tr.Track("s3", "fast", "fake:fast-a")

	snap := tr.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot rows = %d, want 2 (smart, fast)", len(snap))
	}
	// Most sessions first.
	if snap[0].Model != "smart" || snap[0].Sessions != 2 {
		t.Fatalf("first row = %+v, want smart with 2 sessions", snap[0])
	}
	if snap[1].Model != "fast" || snap[1].Sessions != 1 {
		t.Fatalf("second row = %+v, want fast with 1 session", snap[1])
	}
	if snap[0].Since.IsZero() {
		t.Error("smart row carries no since timestamp")
	}
	// Endpoint breakdown, most sessions first.
	if len(snap[0].Endpoints) != 2 {
		t.Fatalf("smart endpoints = %d, want 2", len(snap[0].Endpoints))
	}
	for _, e := range snap[0].Endpoints {
		if e.Sessions != 1 {
			t.Errorf("endpoint %s serves %d sessions, want 1", e.Endpoint, e.Sessions)
		}
	}
}

func TestInflightTrackerCountsSessionsNotRequests(t *testing.T) {
	tr := newInflightTracker()
	// A duplicate client retry re-sends the same body, which
	// hashes to the same session ID: two requests, one session.
	// The dashboard counts sessions.
	tr.Track("s1", "smart", "fake:smart-a")
	tr.Track("s1", "smart", "fake:smart-a")
	snap := tr.Snapshot()
	if len(snap) != 1 || snap[0].Sessions != 1 {
		t.Fatalf("snapshot = %+v, want one row with 1 session", snap)
	}
	// One of the two requests finishing must not empty the
	// panel while its sibling still runs.
	tr.Untrack("s1")
	if snap = tr.Snapshot(); len(snap) != 1 || snap[0].Sessions != 1 {
		t.Fatalf("after one of two requests finished: %+v, want the session still tracked", snap)
	}
	tr.Untrack("s1")
	if snap = tr.Snapshot(); len(snap) != 0 {
		t.Fatalf("after both requests finished: %+v, want an empty panel", snap)
	}
}

func TestInflightTrackerUntrackIsIdempotent(t *testing.T) {
	tr := newInflightTracker()
	tr.Track("s1", "smart", "e")
	tr.Untrack("s1")
	// The request path calls this from a defer; a second
	// untrack for a session that is gone must be a no-op.
	tr.Untrack("s1")
	tr.Untrack("never-tracked")
	if snap := tr.Snapshot(); len(snap) != 0 {
		t.Fatalf("snapshot = %+v, want empty", snap)
	}
}

func TestInflightTrackerUpdateEndpointFollowsFallback(t *testing.T) {
	tr := newInflightTracker()
	tr.Track("s1", "work", "")
	tr.UpdateEndpoint("s1", "fake:broken-model")
	tr.UpdateEndpoint("s1", "fake:fake-model")
	snap := tr.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot rows = %d, want 1", len(snap))
	}
	eps := snap[0].Endpoints
	if len(eps) != 1 || eps[0].Endpoint != "fake:fake-model" {
		t.Fatalf("endpoints = %+v, want only the endpoint the session moved to", eps)
	}
}

func TestInflightTrackerNilReceiverIsSafe(t *testing.T) {
	var tr *inflightTracker
	tr.Track("s1", "smart", "e")
	tr.UpdateEndpoint("s1", "e2")
	tr.Untrack("s1")
	if snap := tr.Snapshot(); snap == nil || len(snap) != 0 {
		t.Fatalf("nil tracker snapshot = %+v, want an empty non-nil slice", snap)
	}
}

func TestInflightTrackerConcurrentAccess(t *testing.T) {
	tr := newInflightTracker()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Three shared session IDs across goroutines, so
			// concurrent Track and Untrack calls contend on
			// the same entries, not just on the map.
			sid := "session-" + string(rune('a'+i%3))
			for j := 0; j < 200; j++ {
				tr.Track(sid, "smart", "fake:smart-a")
				tr.UpdateEndpoint(sid, "fake:smart-b")
				tr.Snapshot()
				tr.Untrack(sid)
			}
		}(i)
	}
	wg.Wait()
	// Every track was paired with an untrack, so the panel must
	// be empty: a non-empty result means Track and Untrack are
	// not symmetric, which is the leak that would make the
	// dashboard accumulate dead sessions forever.
	if snap := tr.Snapshot(); len(snap) != 0 {
		t.Fatalf("after paired track/untrack: %+v, want empty", snap)
	}
}

func TestHandleAdminDashboardDataServesInflight(t *testing.T) {
	cfg := &Config{Models: map[string]ModelConfig{}}
	g := dashboardTestGateway(t, cfg)

	g.inflight.Track("session-1", "smart", "fake:smart-a")
	g.inflight.Track("session-2", "smart", "fake:smart-a")
	g.inflight.Track("session-3", "fast", "fake:fast-a")

	req := httptest.NewRequest(http.MethodGet, "/admin/dashboard/data?window=1d", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	g.HandleAdminDashboardData(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var payload struct {
		Inflight []dashboardInflightModel `json:"inflight"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	if len(payload.Inflight) != 2 {
		t.Fatalf("inflight rows = %d, want 2", len(payload.Inflight))
	}
	if payload.Inflight[0].Model != "smart" || payload.Inflight[0].Sessions != 2 {
		t.Fatalf("first row = %+v, want smart with 2 sessions", payload.Inflight[0])
	}
	if len(payload.Inflight[0].Endpoints) != 1 ||
		payload.Inflight[0].Endpoints[0].Endpoint != "fake:smart-a" ||
		payload.Inflight[0].Endpoints[0].Sessions != 2 {
		t.Fatalf("smart endpoint breakdown = %+v, want fake:smart-a with 2 sessions",
			payload.Inflight[0].Endpoints)
	}

	// A completed session leaves the set: the panel reflects
	// this instant, not the day's history.
	g.inflight.Untrack("session-1")
	g.inflight.Untrack("session-2")
	g.inflight.Untrack("session-3")
	w = httptest.NewRecorder()
	g.HandleAdminDashboardData(w, req)
	var after struct {
		Inflight []dashboardInflightModel `json:"inflight"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &after); err != nil {
		t.Fatalf("second payload is not JSON: %v", err)
	}
	if len(after.Inflight) != 0 {
		t.Fatalf("inflight after all sessions completed = %+v, want empty", after.Inflight)
	}
}

// TestDashboardPageReadsTheInflightFieldsThePayloadEmits pins
// the inflight section to the payload's JSON contract, the
// same way TestDashboardPageReadsTheFieldsThePayloadEmits
// pins the percentile columns: the page fetches the payload
// and renders m.model, m.sessions and e.endpoint, and a
// rename on either side renders the panel empty while every
// other section stays green.
func TestDashboardPageReadsTheInflightFieldsThePayloadEmits(t *testing.T) {
	cfg := &Config{Models: map[string]ModelConfig{}}
	g := dashboardTestGateway(t, cfg)

	dataReq := httptest.NewRequest(http.MethodGet, "/admin/dashboard/data?window=1d", nil)
	dataReq.Header.Set("Authorization", "Bearer secret")
	dataW := httptest.NewRecorder()
	g.HandleAdminDashboardData(dataW, dataReq)
	var payload struct {
		Inflight []struct {
			Model     string `json:"model"`
			Sessions  int    `json:"sessions"`
			Endpoints []struct {
				Endpoint string `json:"endpoint"`
				Sessions int    `json:"sessions"`
			} `json:"endpoints"`
			Since time.Time `json:"since"`
		} `json:"inflight"`
	}
	if err := json.Unmarshal(dataW.Body.Bytes(), &payload); err != nil {
		t.Fatalf("data payload is not JSON: %v", err)
	}

	pageReq := httptest.NewRequest(http.MethodGet, "/admin/dashboard", nil)
	pageReq.Header.Set("Authorization", "Bearer secret")
	pageW := httptest.NewRecorder()
	g.HandleAdminDashboard(pageW, pageReq)
	if pageW.Code != http.StatusOK {
		t.Fatalf("page: status = %d, want 200", pageW.Code)
	}
	html := pageW.Body.String()
	for _, want := range []string{"inflight", "renderInflight", "m.model", "m.sessions", "e.endpoint", "e.sessions"} {
		if !contains(html, want) {
			t.Errorf("dashboard page does not read %q — the inflight section renders empty", want)
		}
	}
}
