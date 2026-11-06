package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// defaultCooldownDBName is the state file the gateway creates beside its
// config. It replaces the two JSON files this package used to write
// (cooldowns.json and cooldowns.priority.json), both of which were rewritten
// in full on every failure or completion-token sample. SQLite gives us
// per-row writes, WAL durability, and crash-safe commits without the
// temp-file-plus-rename dance.
const defaultCooldownDBName = "cooldowns.db"

// cooldownStore owns the SQLite handle for the router's rapidly-changing
// state: cooldowns, circuit breakers, and observed tokens-per-second.
//
// The pragmas below are applied to every connection through the DSN. The pool
// is capped at one connection on purpose: the router already serializes its
// own state under r.mu, writes are tiny and infrequent (debounced), and a
// single connection means `PRAGMA` settings cannot diverge between pooled
// connections. busy_timeout covers the case where an operator's sqlite3
// shell, the model-sync job, or a second gateway process holds the write
// lock for a moment.
//
// Page-cache sizing is deliberately in the single-digit-megabyte range. The
// daemon is built with a 50MB soft heap limit, and the authoritative copies
// of these maps already live in Go memory; a 64MB SQLite page cache would
// only duplicate them and defeat that ceiling.
const cooldownStorePragmas = "" +
	"_pragma=busy_timeout(5000)" +
	"&_pragma=journal_mode(WAL)" +
	"&_pragma=synchronous(NORMAL)" +
	"&_pragma=wal_autocheckpoint(4000)" +
	"&_pragma=journal_size_limit(16777216)" +
	"&_pragma=cache_size(-8000)" +
	"&_pragma=mmap_size(16777216)" +
	"&_pragma=temp_store(FILE)" +
	"&_txlock=immediate"

const cooldownStoreSchema = `
CREATE TABLE IF NOT EXISTS cooldowns (
	key         TEXT PRIMARY KEY,
	expiry      INTEGER NOT NULL,
	status_code INTEGER NOT NULL,
	error_count INTEGER NOT NULL,
	last_error  TEXT NOT NULL
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS circuits (
	key         TEXT PRIMARY KEY,
	state       INTEGER NOT NULL,
	opened_at   INTEGER NOT NULL,
	probes_sent INTEGER NOT NULL
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS priorities (
	key TEXT PRIMARY KEY,
	tps REAL NOT NULL
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS sessions (
	key         TEXT PRIMARY KEY,
	model       TEXT NOT NULL,
	chain       TEXT NOT NULL,
	provider    TEXT NOT NULL,
	model_name  TEXT NOT NULL,
	last_used   INTEGER NOT NULL
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
) WITHOUT ROWID;
`

// cooldownStore is safe for concurrent use. writeMu serializes the diff
// application so two saveCooldowns calls cannot interleave their statements.
type cooldownStore struct {
	db      *sql.DB
	path    string
	writeMu sync.Mutex
}

// cooldownDiff is the set of rows a single flush must change. It is computed
// by comparing the router's current maps with the snapshot that was last
// successfully written, so a save touches only the endpoints whose state
// actually moved.
type cooldownDiff struct {
	upsertCooldowns  map[string]CooldownEntry
	deleteCooldowns  []string
	upsertCircuits   map[string]CircuitBreaker
	deleteCircuits   []string
	upsertPriorities map[string]float64
	deletePriorities []string
}

func (d cooldownDiff) empty() bool {
	return len(d.upsertCooldowns) == 0 && len(d.deleteCooldowns) == 0 &&
		len(d.upsertCircuits) == 0 && len(d.deleteCircuits) == 0 &&
		len(d.upsertPriorities) == 0 && len(d.deletePriorities) == 0
}

// openCooldownStore opens (and, on first use, migrates) the SQLite state
// database. A legacy cooldowns.json at the requested path — the shape older
// releases wrote and the shape a handful of tests still seed — is imported
// and renamed out of the way so the path can become a real database.
func openCooldownStore(path string) (*cooldownStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}

	legacyRenamed := ""
	if fi, err := os.Stat(path); err == nil && !fi.IsDir() {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, fmt.Errorf("read %s: %w", path, readErr)
		}
		if len(data) > 0 && !isSQLiteFile(data) {
			// A pre-SQLite state file. Move it aside before sql.Open so the
			// driver does not mistake JSON for a database header. The data is
			// imported below; the rename keeps it recoverable if the import
			// cannot be parsed.
			backup := path + ".migrated"
			if err := os.Rename(path, backup); err != nil {
				return nil, fmt.Errorf("move legacy state %s aside: %w", path, err)
			}
			legacyRenamed = backup
			log.Printf("[info] migrating legacy cooldown state %s -> %s (SQLite)", backup, path)
		}
	}

	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("create state directory %s: %w", dir, err)
		}
	}
	// Create the file with 0600 before sql.Open: the driver's default create
	// mode follows the umask, and this file can contain upstream error text.
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		f, createErr := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
		if createErr != nil {
			return nil, fmt.Errorf("create state database %s: %w", path, createErr)
		}
		if closeErr := f.Close(); closeErr != nil {
			return nil, fmt.Errorf("close new state database %s: %w", path, closeErr)
		}
	}

	dsn := "file:" + filepath.ToSlash(path) + "?" + cooldownStorePragmas
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open state database %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect state database %s: %w", path, err)
	}
	if _, err := db.Exec(cooldownStoreSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize state database %s: %w", path, err)
	}

	s := &cooldownStore{db: db, path: path}
	if err := s.migrateLegacy(legacyRenamed); err != nil {
		// A failed import must not take the gateway down: the new database is
		// usable, and the old file has been preserved. Log loudly and move on.
		log.Printf("[warn] legacy cooldown import failed (state preserved at %s): %v", legacyRenamed, err)
	}
	return s, nil
}

func (s *cooldownStore) close() error {
	if s == nil || s.db == nil {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	// Fold the WAL back into the main file before closing. Without this a
	// crash immediately after shutdown can leave a large -wal beside a tiny
	// database; more importantly, a copied/single-file backup taken right
	// after shutdown would silently miss the newest rows.
	_, _ = s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return s.db.Close()
}

func isSQLiteFile(data []byte) bool {
	return bytes.HasPrefix(data, []byte("SQLite format 3\x00"))
}

func unixNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func timeFromUnixNano(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// migrateLegacy imports the two JSON files written by older releases. It runs
// at most once per database (guarded by the meta table), and it never
// overwrites rows already present: the database is authoritative the moment
// it exists.
func (s *cooldownStore) migrateLegacy(renamedPath string) error {
	// The cooldown source is either the file that was renamed out of the way
	// at the requested path (tests and a first deploy over an old install) or
	// the conventional cooldowns.json beside the database.
	var legacyCooldowns []byte
	cooldownSource := ""
	if renamedPath != "" {
		if data, err := os.ReadFile(renamedPath); err == nil {
			legacyCooldowns = data
			cooldownSource = renamedPath
		}
	}
	if legacyCooldowns == nil {
		sibling := filepath.Join(filepath.Dir(s.path), "cooldowns.json")
		// When the database path IS the conventional legacy path (old tests,
		// or a first start over an install that used cooldowns.json), the
		// sibling lookup resolves to the database file this call just
		// created. Reading that as JSON would parse the SQLite header as a
		// legacy envelope and fail on every startup.
		if filepath.Clean(sibling) != filepath.Clean(s.path) {
			if data, err := os.ReadFile(sibling); err == nil {
				legacyCooldowns = data
				cooldownSource = sibling
			}
		}
	}

	if !s.metaFlag("legacy_cooldowns_imported") {
		if len(legacyCooldowns) > 0 {
			cds, cbs := parseLegacyCooldownState(legacyCooldowns)
			if err := s.importLegacy(cds, cbs, nil); err != nil {
				return err
			}
			if cooldownSource != "" && cooldownSource != renamedPath {
				if err := os.Rename(cooldownSource, cooldownSource+".migrated"); err != nil {
					log.Printf("[warn] could not archive legacy cooldown file %s: %v", cooldownSource, err)
				}
			}
			log.Printf("[info] imported %d cooldowns and %d circuits from legacy JSON", len(cds), len(cbs))
		}
		if err := s.setMetaFlag("legacy_cooldowns_imported"); err != nil {
			return err
		}
	}

	if !s.metaFlag("legacy_priorities_imported") {
		var prioritySource string
		if strings.HasSuffix(s.path, ".json") {
			prioritySource = strings.TrimSuffix(s.path, ".json") + ".priority.json"
		} else {
			prioritySource = filepath.Join(filepath.Dir(s.path), "cooldowns.priority.json")
		}
		if data, err := os.ReadFile(prioritySource); err == nil {
			var priorities map[string]float64
			if err := json.Unmarshal(data, &priorities); err != nil {
				log.Printf("[warn] ignoring malformed legacy priorities %s: %v", prioritySource, err)
			} else if err := s.importLegacy(nil, nil, priorities); err != nil {
				return err
			} else if err := os.Rename(prioritySource, prioritySource+".migrated"); err != nil {
				log.Printf("[warn] could not archive legacy priority file %s: %v", prioritySource, err)
			} else {
				log.Printf("[info] imported %d TPS priorities from legacy JSON", len(priorities))
			}
		}
		if err := s.setMetaFlag("legacy_priorities_imported"); err != nil {
			return err
		}
	}
	return nil
}

// parseLegacyCooldownState accepts both historical shapes:
//
//	{"cooldowns":{...},"circuits":{...}}
//	{"provider:model":{...}, ...}            (pre-circuit releases)
//
// The sections are decoded independently. One malformed circuit must cost
// only that circuit: decoding the envelope in one pass would abort everything
// and silently discard all backoff state on the next start.
func parseLegacyCooldownState(data []byte) (map[string]CooldownEntry, map[string]CircuitBreaker) {
	now := time.Now()
	active := func(loaded map[string]CooldownEntry) map[string]CooldownEntry {
		out := make(map[string]CooldownEntry, len(loaded))
		for k, v := range loaded {
			if v.Expiry.After(now) {
				out[k] = v
			}
		}
		return out
	}

	trimmed := strings.TrimLeft(string(data), " \t\r\n")
	if strings.HasPrefix(trimmed, "{") && !strings.Contains(trimmed[:min(len(trimmed), 200)], `"cooldowns"`) {
		var loaded map[string]CooldownEntry
		if err := json.Unmarshal(data, &loaded); err != nil {
			log.Printf("[warn] failed to parse legacy cooldowns (state reset): %v", err)
			return map[string]CooldownEntry{}, map[string]CircuitBreaker{}
		}
		return active(loaded), map[string]CircuitBreaker{}
	}

	var state cooldownsState
	if err := json.Unmarshal(data, &state); err != nil {
		var cooldownsOnly struct {
			Cooldowns map[string]CooldownEntry `json:"cooldowns"`
		}
		if err := json.Unmarshal(data, &cooldownsOnly); err != nil {
			log.Printf("[warn] failed to parse cooldowns (state reset): %v", err)
			return map[string]CooldownEntry{}, map[string]CircuitBreaker{}
		}
		state.Cooldowns = cooldownsOnly.Cooldowns
	}

	circuits := state.Circuits
	if circuits == nil {
		var circuitsOnly struct {
			Circuits map[string]CircuitBreaker `json:"circuits"`
		}
		if err := json.Unmarshal(data, &circuitsOnly); err == nil {
			circuits = circuitsOnly.Circuits
		}
	}

	keptCircuits := make(map[string]CircuitBreaker)
	for k, cb := range circuits {
		switch cb.State {
		case CircuitOpen:
			// Only meaningful while the cooldown is still active; an expired
			// cooldown re-opens it on the next request anyway.
			if cd, ok := state.Cooldowns[k]; ok && cd.Expiry.After(now) {
				keptCircuits[k] = cb
			}
		case CircuitHalfOpen:
			keptCircuits[k] = cb
		}
	}
	return active(state.Cooldowns), keptCircuits
}

func (s *cooldownStore) importLegacy(cds map[string]CooldownEntry, cbs map[string]CircuitBreaker, tps map[string]float64) error {
	if len(cds) == 0 && len(cbs) == 0 && len(tps) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// INSERT OR IGNORE: if a row already exists, it is newer than the legacy
	// snapshot and must win. This makes the migration safe to retry.
	cdStmt, err := tx.Prepare(`INSERT OR IGNORE INTO cooldowns
		(key, expiry, status_code, error_count, last_error) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer cdStmt.Close()
	for k, v := range cds {
		if _, err := cdStmt.Exec(k, unixNano(v.Expiry), v.StatusCode, v.ErrorCount, v.LastError); err != nil {
			return err
		}
	}

	cbStmt, err := tx.Prepare(`INSERT OR IGNORE INTO circuits
		(key, state, opened_at, probes_sent) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer cbStmt.Close()
	for k, v := range cbs {
		if _, err := cbStmt.Exec(k, int(v.State), unixNano(v.OpenedAt), v.ProbesSent); err != nil {
			return err
		}
	}

	tpsStmt, err := tx.Prepare(`INSERT OR IGNORE INTO priorities (key, tps) VALUES (?, ?)`)
	if err != nil {
		return err
	}
	defer tpsStmt.Close()
	for k, v := range tps {
		if _, err := tpsStmt.Exec(k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *cooldownStore) metaFlag(key string) bool {
	var value string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&value)
	return err == nil && value == "1"
}

func (s *cooldownStore) setMetaFlag(key string) error {
	_, err := s.db.Exec(`INSERT INTO meta (key, value) VALUES (?, '1')
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key)
	return err
}

// loadCooldowns returns the active rows. Expired rows are dropped in the same
// transaction so a long-stopped gateway does not resurrect a stale ban.
func (s *cooldownStore) loadCooldowns(now time.Time) (map[string]CooldownEntry, map[string]CircuitBreaker, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM cooldowns WHERE expiry <= ?`, unixNano(now)); err != nil {
		return nil, nil, err
	}

	rows, err := tx.Query(`SELECT key, expiry, status_code, error_count, last_error FROM cooldowns`)
	if err != nil {
		return nil, nil, err
	}
	cooldowns := make(map[string]CooldownEntry)
	for rows.Next() {
		var (
			key       string
			expiry    int64
			status    int
			errCount  int
			lastError string
		)
		if err := rows.Scan(&key, &expiry, &status, &errCount, &lastError); err != nil {
			rows.Close()
			return nil, nil, err
		}
		cooldowns[key] = CooldownEntry{
			Expiry:     timeFromUnixNano(expiry),
			StatusCode: status,
			ErrorCount: errCount,
			LastError:  lastError,
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, err
	}
	rows.Close()

	crows, err := tx.Query(`SELECT key, state, opened_at, probes_sent FROM circuits`)
	if err != nil {
		return nil, nil, err
	}
	circuits := make(map[string]CircuitBreaker)
	for crows.Next() {
		var (
			key    string
			state  int
			opened int64
			probes int
		)
		if err := crows.Scan(&key, &state, &opened, &probes); err != nil {
			crows.Close()
			return nil, nil, err
		}
		circuits[key] = CircuitBreaker{
			State:      CircuitState(state),
			OpenedAt:   timeFromUnixNano(opened),
			ProbesSent: probes,
		}
	}
	if err := crows.Err(); err != nil {
		crows.Close()
		return nil, nil, err
	}
	crows.Close()

	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return cooldowns, circuits, nil
}

func (s *cooldownStore) loadPriorities() (map[string]float64, error) {
	rows, err := s.db.Query(`SELECT key, tps FROM priorities`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]float64)
	for rows.Next() {
		var (
			key string
			tps float64
		)
		if err := rows.Scan(&key, &tps); err != nil {
			return nil, err
		}
		out[key] = tps
	}
	return out, rows.Err()
}

// apply writes one diff in a single transaction. Statements are row-scoped:
// a single endpoint failure updates one cooldown row and possibly one circuit
// row, instead of rewriting every entry in the state file.
func (s *cooldownStore) apply(d cooldownDiff, now time.Time) error {
	if d.empty() {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if len(d.upsertCooldowns) > 0 {
		stmt, err := tx.Prepare(`INSERT INTO cooldowns
			(key, expiry, status_code, error_count, last_error) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET
				expiry = excluded.expiry,
				status_code = excluded.status_code,
				error_count = excluded.error_count,
				last_error = excluded.last_error`)
		if err != nil {
			return err
		}
		for k, v := range d.upsertCooldowns {
			if _, err := stmt.Exec(k, unixNano(v.Expiry), v.StatusCode, v.ErrorCount, v.LastError); err != nil {
				stmt.Close()
				return err
			}
		}
		stmt.Close()
	}

	if len(d.upsertCircuits) > 0 {
		stmt, err := tx.Prepare(`INSERT INTO circuits
			(key, state, opened_at, probes_sent) VALUES (?, ?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET
				state = excluded.state,
				opened_at = excluded.opened_at,
				probes_sent = excluded.probes_sent`)
		if err != nil {
			return err
		}
		for k, v := range d.upsertCircuits {
			if _, err := stmt.Exec(k, int(v.State), unixNano(v.OpenedAt), v.ProbesSent); err != nil {
				stmt.Close()
				return err
			}
		}
		stmt.Close()
	}

	if len(d.upsertPriorities) > 0 {
		stmt, err := tx.Prepare(`INSERT INTO priorities (key, tps) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET tps = excluded.tps`)
		if err != nil {
			return err
		}
		for k, v := range d.upsertPriorities {
			if _, err := stmt.Exec(k, v); err != nil {
				stmt.Close()
				return err
			}
		}
		stmt.Close()
	}

	deleteRows := func(query string, keys []string) error {
		if len(keys) == 0 {
			return nil
		}
		stmt, err := tx.Prepare(query)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, k := range keys {
			if _, err := stmt.Exec(k); err != nil {
				return err
			}
		}
		return nil
	}
	if err := deleteRows(`DELETE FROM cooldowns WHERE key = ?`, d.deleteCooldowns); err != nil {
		return err
	}
	if err := deleteRows(`DELETE FROM circuits WHERE key = ?`, d.deleteCircuits); err != nil {
		return err
	}
	if err := deleteRows(`DELETE FROM priorities WHERE key = ?`, d.deletePriorities); err != nil {
		return err
	}

	// Expiries are wall-clock values, so the database can be swept without
	// the router having observed each individual cooldown ending.
	if _, err := tx.Exec(`DELETE FROM cooldowns WHERE expiry <= ?`, unixNano(now)); err != nil {
		return err
	}
	return tx.Commit()
}

// loadSessions reads the persisted sticky sessions. Entries older than the TTL
// are dropped so a restart does not resurrect conversations that have already
// expired.
func (s *cooldownStore) loadSessions(now time.Time) (map[string]sessionEntry, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	rows, err := s.db.Query("SELECT key, model, chain, provider, model_name, last_used FROM sessions")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]sessionEntry{}
	for rows.Next() {
		var key, model, chain, provider, modelName string
		var lastUsed int64
		if err := rows.Scan(&key, &model, &chain, &provider, &modelName, &lastUsed); err != nil {
			return nil, err
		}
		se := sessionEntry{
			model:    model,
			chain:    chain,
			lastUsed: timeFromUnixNano(lastUsed),
			ep:       ModelEndpoint{Provider: provider, Model: modelName},
		}
		if now.Sub(se.lastUsed) > sessionTTL {
			continue
		}
		out[key] = se
	}
	return out, rows.Err()
}

// saveSession upserts one sticky session row.
func (s *cooldownStore) saveSession(key string, se sessionEntry) error {
	if s == nil || s.db == nil {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO sessions (key, model, chain, provider, model_name, last_used)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET
		   model=excluded.model, chain=excluded.chain, provider=excluded.provider,
		   model_name=excluded.model_name, last_used=excluded.last_used`,
		key, se.model, se.chain, se.ep.Provider, se.ep.Model, unixNano(se.lastUsed))
	return err
}

// deleteSession removes one sticky session row.
func (s *cooldownStore) deleteSession(key string) error {
	if s == nil || s.db == nil {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.Exec("DELETE FROM sessions WHERE key = ?", key)
	return err
}

// saveSessions rewrites the sessions table to match the supplied map.
func (s *cooldownStore) saveSessions(sessions map[string]sessionEntry) error {
	if s == nil || s.db == nil {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM sessions"); err != nil {
		tx.Rollback()
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO sessions (key, model, chain, provider, model_name, last_used)
		VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	for key, se := range sessions {
		if _, err := stmt.Exec(key, se.model, se.chain, se.ep.Provider, se.ep.Model, unixNano(se.lastUsed)); err != nil {
			stmt.Close()
			tx.Rollback()
			return err
		}
	}
	stmt.Close()
	return tx.Commit()
}
