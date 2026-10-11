package main

import (
	"bufio"
	"encoding/json"
	"os"
	"sort"
	"time"
)

// journalLine is the on-disk form of one attemptTick: a single
// minute's per-endpoint telemetry, one JSON object per line in the
// dashboard's append-only journal.
//
// The maps keep their labelKey key type: labelKey's underlying type
// is a string, which is exactly what encoding/json requires of a map
// key, so the ring's maps marshal and unmarshal without conversion —
// and therefore without a second place where the key type could be
// mistranslated.
//
// Fields are omitempty so a quiet minute (the common case: most
// minutes most endpoints are idle) serializes to a nearly empty line
// instead of seven empty maps.
type journalLine struct {
	Start     time.Time            `json:"start"`
	Attempts  map[labelKey]int64   `json:"attempts,omitempty"`
	Failures  map[labelKey]int64   `json:"failures,omitempty"`
	LatencyNS map[labelKey]int64   `json:"latency_ns,omitempty"`
	Tokens    map[labelKey]int64   `json:"tokens,omitempty"`
	Active    map[labelKey]int64   `json:"active_ticks,omitempty"`
	Hist      map[labelKey][]int64 `json:"hist,omitempty"`
	Endpoints map[labelKey]string  `json:"endpoints,omitempty"`
}

// journalMinute returns a tick's position on the minute grid the ring
// is built on. The ticker advances the ring exactly on minute
// boundaries, so a tick's identity is its minute, not its slot: the
// slot is derived, which is what makes a journal written by one
// process loadable by another (or by the same process after a
// restart) without any handshake about ring positions.
func journalMinute(t time.Time) int64 {
	return t.Unix() / 60
}

// journalSlot maps a minute onto the ring slot that holds it. The
// ring advances +1 modulo dashboardRingLen per minute, so consecutive
// minutes occupy consecutive slots and a minute's slot is a pure
// function of the minute itself.
func journalSlot(minute int64) int {
	return int(minute % dashboardRingLen)
}

func (t *attemptTick) journalLine() journalLine {
	return journalLine{
		Start:     t.start,
		Attempts:  t.attempts,
		Failures:  t.failures,
		LatencyNS: t.latencyNS,
		Tokens:    t.tokens,
		Active:    t.activeTicks,
		Hist:      t.hist,
		Endpoints: t.endpoints,
	}
}

func (l journalLine) attemptTick() *attemptTick {
	return &attemptTick{
		attempts:    l.Attempts,
		failures:    l.Failures,
		latencyNS:   l.LatencyNS,
		tokens:      l.Tokens,
		activeTicks: l.Active,
		hist:        l.Hist,
		endpoints:   l.Endpoints,
		start:       l.Start,
	}
}

// appendDashboardJournal appends one completed tick to the journal.
//
// The file is opened, written and closed on every append rather than
// held open: the daily prune rewrites the journal by writing a
// filtered copy and renaming it over the original, and a long-lived
// handle would keep appending to the old inode after that rename,
// silently discarding every subsequent minute. Reopening per append
// makes the prune's rename safe, at the cost of one open per minute.
//
// A crash between open and write loses at most the line being
// written; a crash mid-write leaves a torn final line, which the
// replay skips. Neither loses data the ring had already acknowledged.
func appendDashboardJournal(path string, t *attemptTick) error {
	if t == nil {
		return nil
	}
	line, err := json.Marshal(t.journalLine())
	if err != nil {
		return err
	}
	line = append(line, '\n')
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(line)
	return err
}

// replayDashboardJournal reads the journal and returns one line per
// minute, dropping lines the current ring cannot hold: minutes older
// than the retention window (their slots have been reused by newer
// ticks, so restoring them would corrupt the window sums) and minutes
// in the future (a clock jump, not data). A line that fails to parse
// is skipped rather than fatal: a torn final line is the expected
// crash artifact, and one bad minute must not cost the other 43,200.
//
// Duplicate minutes (two processes writing, or a restart that
// re-appended a minute) resolve last-wins, matching the ring's own
// semantics: the later observation of the same minute is the more
// complete one.
func replayDashboardJournal(path string, nowMinute int64) (map[int64]journalLine, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := make(map[int64]journalLine)
	sc := bufio.NewScanner(f)
	// A line is one tick: a few KB at the most, but a hist with many
	// endpoints can exceed the 64KB default scanner limit, so raise
	// the cap rather than fail a journal that grew a busy minute.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var line journalLine
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			continue
		}
		minute := journalMinute(line.Start)
		if minute > nowMinute || minute <= nowMinute-dashboardRingLen {
			continue
		}
		out[minute] = line
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// pruneDashboardJournal rewrites the journal keeping only the minutes
// the retention window covers, and returns how many ticks were kept.
// It writes a filtered copy next to the original and renames it into
// place, so a crash mid-prune leaves either the old or the new file,
// never a half-written one. The daemon appends by reopening the file
// each minute, so the rename is safe: its next append lands in the
// pruned file.
func pruneDashboardJournal(path string) (int, error) {
	nowMinute := journalMinute(time.Now())
	lines, err := replayDashboardJournal(path, nowMinute)
	if err != nil {
		return 0, err
	}
	if len(lines) == 0 {
		// Nothing within the window: remove the file so a gateway that
		// has never served a request (or has been down longer than the
		// window) leaves no stale journal behind.
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return 0, err
		}
		return 0, nil
	}
	// Minutes are written in order so the journal stays a chronology
	// an operator can read with tail(1), not a map dump.
	minutes := make([]int64, 0, len(lines))
	for minute := range lines {
		minutes = append(minutes, minute)
	}
	sort.Slice(minutes, func(i, j int) bool { return minutes[i] < minutes[j] })
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return 0, err
	}
	enc := json.NewEncoder(f)
	for _, minute := range minutes {
		if err := enc.Encode(lines[minute]); err != nil {
			f.Close()
			os.Remove(tmp)
			return 0, err
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	return len(lines), nil
}
