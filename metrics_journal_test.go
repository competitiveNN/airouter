package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// journalTestTick builds a tick for a given minute with one recorded
// endpoint, so tests can assert on content without going through the
// hot path.
func journalTestTick(minute int64, endpoint string, attempts int64) *attemptTick {
	t := newAttemptTick()
	t.start = time.Unix(minute*60, 0).UTC()
	key := singleLabelKey("endpoint", endpoint)
	t.attempts[key] = attempts
	t.failures[key] = attempts / 2
	t.latencyNS[key] = attempts * 1000
	t.tokens[key] = attempts * 10
	t.activeTicks[key] = 1
	t.hist[key] = make([]int64, len(attemptLatencyBoundaries)+1)
	t.hist[key][0] = attempts
	t.endpoints[key] = endpoint
	return t
}

func TestDashboardJournalRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics-ring.jsonl")
	minute := journalMinute(time.Now())
	tick := journalTestTick(minute, "prov:model", 7)
	if err := appendDashboardJournal(path, tick); err != nil {
		t.Fatalf("append: %v", err)
	}
	lines, err := replayDashboardJournal(path, minute)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	line, ok := lines[minute]
	if !ok {
		t.Fatalf("minute %d missing from replay", minute)
	}
	restored := line.attemptTick()
	key := singleLabelKey("endpoint", "prov:model")
	if restored.attempts[key] != 7 {
		t.Errorf("attempts = %d, want 7", restored.attempts[key])
	}
	if restored.failures[key] != 3 {
		t.Errorf("failures = %d, want 3", restored.failures[key])
	}
	if restored.tokens[key] != 70 {
		t.Errorf("tokens = %d, want 70", restored.tokens[key])
	}
	if restored.hist[key][0] != 7 {
		t.Errorf("hist[0] = %d, want 7", restored.hist[key][0])
	}
	if restored.endpoints[key] != "prov:model" {
		t.Errorf("endpoint = %q, want prov:model", restored.endpoints[key])
	}
	if !restored.start.Equal(tick.start) {
		t.Errorf("start = %v, want %v", restored.start, tick.start)
	}
}

// TestDashboardJournalSlotsByMinute pins the property that makes a
// journal restart-safe: a tick's ring slot is derived from its
// minute, so after a restore every tick sits exactly where the
// DashboardQuery walk-back expects it.
func TestDashboardJournalSlotsByMinute(t *testing.T) {
	now := time.Now().Truncate(time.Minute)
	nowMinute := journalMinute(now)
	for _, offset := range []int64{0, -1, -60, -1440, -43200} {
		minute := nowMinute + offset
		if got := journalSlot(minute); got != journalSlot(journalMinute(time.Unix(minute*60, 0).UTC())) {
			t.Errorf("slot for minute %d is not stable", minute)
		}
		if got := journalSlot(minute); got < 0 || got >= dashboardRingLen {
			t.Errorf("slot for minute %d = %d, outside the ring", minute, got)
		}
	}
	// Consecutive minutes occupy consecutive slots, wrapping at the
	// ring length: that is what makes the walk-back in
	// DashboardQuery address the right minutes after a restore.
	a := journalSlot(nowMinute)
	b := journalSlot(nowMinute + 1)
	if b != (a+1)%dashboardRingLen {
		t.Errorf("slot(%d)+1 = %d, want %d", nowMinute, b, (a+1)%dashboardRingLen)
	}
}

func TestDashboardJournalPruneKeepsWindowOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metrics-ring.jsonl")
	nowMinute := journalMinute(time.Now())
	// One tick inside the window, one just outside it (the window
	// covers dashboardRingLen minutes including the current one).
	if err := appendDashboardJournal(path, journalTestTick(nowMinute, "prov:new", 1)); err != nil {
		t.Fatalf("append new: %v", err)
	}
	stale := nowMinute - dashboardRingLen - 1
	if err := appendDashboardJournal(path, journalTestTick(stale, "prov:old", 2)); err != nil {
		t.Fatalf("append stale: %v", err)
	}
	kept, err := pruneDashboardJournal(path)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if kept != 1 {
		t.Fatalf("prune kept %d ticks, want 1", kept)
	}
	lines, err := replayDashboardJournal(path, nowMinute)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if _, ok := lines[nowMinute]; !ok {
		t.Error("in-window tick was pruned")
	}
	if _, ok := lines[stale]; ok {
		t.Error("out-of-window tick survived the prune")
	}
}

func TestDashboardJournalPruneEmptyRemovesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metrics-ring.jsonl")
	// A journal whose every line is outside the window (the gateway
	// was down for longer than the retention window) is removed
	// rather than left as a stale file the next boot must special-case.
	stale := journalMinute(time.Now()) - dashboardRingLen - 5
	if err := appendDashboardJournal(path, journalTestTick(stale, "prov:old", 1)); err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := pruneDashboardJournal(path); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("pruned-empty journal still on disk (stat err = %v)", err)
	}
}

func TestDashboardJournalReplaySkipsBadLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metrics-ring.jsonl")
	nowMinute := journalMinute(time.Now())
	// A torn final line (the crash artifact) and a garbage line must
	// not cost the valid lines around them.
	good := journalTestTick(nowMinute-1, "prov:good", 3)
	line, err := json.Marshal(good.journalLine())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	content := "not json at all\n" + string(line) + "\n{\"start\":\"tr\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	lines, err := replayDashboardJournal(path, nowMinute)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("replay returned %d lines, want 1 (bad lines must be skipped, not fatal)", len(lines))
	}
}

func TestDashboardJournalReplayDropsFutureAndDuplicates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metrics-ring.jsonl")
	nowMinute := journalMinute(time.Now())
	// A future tick (clock jumped back after being written) and a
	// duplicate minute (two writers) resolve to: future dropped,
	// duplicate last-wins.
	if err := appendDashboardJournal(path, journalTestTick(nowMinute+10, "prov:future", 1)); err != nil {
		t.Fatalf("append future: %v", err)
	}
	if err := appendDashboardJournal(path, journalTestTick(nowMinute-2, "prov:dup", 1)); err != nil {
		t.Fatalf("append dup first: %v", err)
	}
	if err := appendDashboardJournal(path, journalTestTick(nowMinute-2, "prov:dup", 5)); err != nil {
		t.Fatalf("append dup second: %v", err)
	}
	lines, err := replayDashboardJournal(path, nowMinute)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if _, ok := lines[nowMinute+10]; ok {
		t.Error("future tick was restored")
	}
	line, ok := lines[nowMinute-2]
	if !ok {
		t.Fatal("duplicated minute missing")
	}
	key := singleLabelKey("endpoint", "prov:dup")
	if line.Attempts[key] != 5 {
		t.Errorf("duplicate minute resolved to %d attempts, want 5 (last wins)", line.Attempts[key])
	}
}

func TestStartDashboardTelemetryRestoresRing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metrics-ring.jsonl")
	nowMinute := journalMinute(time.Now())
	// Three minutes of history, written out of order to prove the
	// restore slots by minute rather than by position in the file.
	for _, offset := range []int64{-3, -1, -2} {
		if err := appendDashboardJournal(path, journalTestTick(nowMinute+offset, "prov:model", -offset*10)); err != nil {
			t.Fatalf("append %d: %v", offset, err)
		}
	}
	m := NewMetrics()
	stop := m.StartDashboardTelemetry(path)
	defer stop()

	m.mu.Lock()
	defer m.mu.Unlock()
	if want := journalSlot(nowMinute); m.attemptRingNow != want {
		t.Errorf("attemptRingNow = %d, want %d (the current minute's slot)", m.attemptRingNow, want)
	}
	for _, offset := range []int64{-1, -2, -3} {
		slot := journalSlot(nowMinute + offset)
		tick := m.attemptRing[slot]
		if tick == nil {
			t.Fatalf("minute %d not restored to slot %d", nowMinute+offset, slot)
		}
		key := singleLabelKey("endpoint", "prov:model")
		if tick.attempts[key] != -offset*10 {
			t.Errorf("minute %d restored %d attempts, want %d", nowMinute+offset, tick.attempts[key], -offset*10)
		}
	}
}

func TestStartDashboardTelemetryAppendsCompletedTicks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metrics-ring.jsonl")
	m := NewMetrics()
	stop := m.StartDashboardTelemetry(path)
	defer stop()
	// Record after the telemetry starts: the restore moves
	// attemptRingNow to the current minute's slot, so an
	// attempt recorded before the start would land in a slot
	// the ring no longer reads from.
	m.RecordDashboardTokens("prov:model", 42)

	m.mu.Lock()
	tick := m.attemptRing[m.attemptRingNow]
	m.mu.Unlock()
	if tick == nil {
		t.Fatal("no tick was recorded")
	}
	if err := appendDashboardJournal(path, tick); err != nil {
		t.Fatalf("append: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("journal is empty after an append")
	}
}

func TestPruneStatsFlagExitsBeforeServer(t *testing.T) {
	// The prune mode is exercised through its unit (pruneDashboardJournal)
	// and the systemd timer; here pin that the prune of a missing file
	// is a no-op success rather than an error, so a timer firing on a
	// fresh install does not log a failure every day.
	kept, err := pruneDashboardJournal(filepath.Join(t.TempDir(), "absent.jsonl"))
	if err != nil {
		t.Fatalf("prune of missing file: %v", err)
	}
	if kept != 0 {
		t.Errorf("prune of missing file kept %d, want 0", kept)
	}
}
