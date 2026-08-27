package main

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"sync"
	"time"
)

type CooldownEntry struct {
	Expiry      time.Time `json:"expiry"`
	StatusCode  int       `json:"status_code"`
	ErrorCount  int       `json:"error_count"`
	LastError   string    `json:"last_error"`
}

type Router struct {
	config       *Config
	sessions     map[string]ModelEndpoint
	cooldowns    map[string]CooldownEntry
	rrCounters   map[string]int
	cooldownPath string
	mu           sync.RWMutex
}

func NewRouter(cfg *Config, cooldownPath string) *Router {
	r := &Router{
		config:       cfg,
		sessions:     make(map[string]ModelEndpoint),
		cooldowns:    make(map[string]CooldownEntry),
		rrCounters:   make(map[string]int),
		cooldownPath: cooldownPath,
	}
	r.loadCooldowns()
	return r
}

func (r *Router) loadCooldowns() {
	if r.cooldownPath == "" {
		return
	}
	data, err := os.ReadFile(r.cooldownPath)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		log.Printf("[debug] failed to load cooldowns: %v", err)
		return
	}
	var loaded map[string]CooldownEntry
	if err := json.Unmarshal(data, &loaded); err != nil {
		log.Printf("[debug] failed to parse cooldowns: %v", err)
		return
	}
	now := time.Now()
	for k, v := range loaded {
		if v.Expiry.After(now) {
			r.cooldowns[k] = v
		}
	}
	log.Printf("[debug] loaded %d active cooldowns", len(r.cooldowns))
}

func (r *Router) saveCooldowns() {
	if r.cooldownPath == "" {
		return
	}
	now := time.Now()
	active := make(map[string]CooldownEntry)
	for k, v := range r.cooldowns {
		if v.Expiry.After(now) {
			active[k] = v
		}
	}
	data, err := json.MarshalIndent(active, "", "  ")
	if err != nil {
		log.Printf("[debug] failed to marshal cooldowns: %v", err)
		return
	}
	if err := os.WriteFile(r.cooldownPath, data, 0644); err != nil {
		log.Printf("[debug] failed to save cooldowns: %v", err)
	}
}

func (r *Router) isAvailableLocked(ep *ModelEndpoint) bool {
	cd, ok := r.cooldowns[ep.Key()]
	if !ok {
		return true
	}
	if time.Now().After(cd.Expiry) {
		delete(r.cooldowns, ep.Key())
		return true
	}
	return false
}

func (r *Router) IsAvailable(ep *ModelEndpoint) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.isAvailableLocked(ep)
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func (r *Router) minCooldownWait(chain []ModelEndpoint) time.Duration {
	minWait := time.Duration(0)
	for _, ep := range chain {
		if cd, ok := r.cooldowns[ep.Key()]; ok {
			remaining := time.Until(cd.Expiry)
			if remaining > 0 && (minWait == 0 || remaining < minWait) {
				minWait = remaining
			}
		}
	}
	return minWait
}

func (r *Router) SelectEndpoint(logicalModel, sessionID string) (*ModelEndpoint, time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	chain, ok := r.config.Models[logicalModel]
	if !ok || len(chain.Chain) == 0 {
		return nil, 0
	}

	if ep, ok := r.sessions[sessionID]; ok {
		if r.isAvailableLocked(&ep) {
			log.Printf("[debug] session=%s model=%s -> sticky %s/%s", sessionID, logicalModel, ep.Provider, ep.Model)
			return &ep, 0
		}
		log.Printf("[debug] session=%s model=%s -> session model %s/%s cooling down, searching fallback", sessionID, logicalModel, ep.Provider, ep.Model)
		for _, next := range chain.Chain {
			if r.isAvailableLocked(&next) {
				log.Printf("[debug] session=%s model=%s -> fallback %s/%s -> %s/%s", sessionID, logicalModel, ep.Provider, ep.Model, next.Provider, next.Model)
				r.sessions[sessionID] = next
				return &next, 0
			}
		}
	}

	// New session
	for _, ep := range chain.Chain {
		if r.isAvailableLocked(&ep) {
			log.Printf("[debug] session=%s model=%s -> new session assigned to %s/%s", sessionID, logicalModel, ep.Provider, ep.Model)
			r.sessions[sessionID] = ep
			return &ep, 0
		}
	}

	return nil, r.minCooldownWait(chain.Chain)
}

func (r *Router) SelectNext(logicalModel, sessionID string, current *ModelEndpoint) (*ModelEndpoint, time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	chain, ok := r.config.Models[logicalModel]
	if !ok {
		return nil, 0
	}

	found := false
	for _, ep := range chain.Chain {
		if found {
			if r.isAvailableLocked(&ep) {
				r.sessions[sessionID] = ep
				return &ep, 0
			}
		}
		if ep.Equal(*current) {
			found = true
		}
	}

	for _, ep := range chain.Chain {
		if r.isAvailableLocked(&ep) {
			r.sessions[sessionID] = ep
			return &ep, 0
		}
	}

	return nil, r.minCooldownWait(chain.Chain)
}

func (r *Router) ApplyCooldown(ep *ModelEndpoint, statusCode int, errMsg string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := ep.Key()
	cd, ok := r.cooldowns[key]
	if !ok {
		cd = CooldownEntry{}
	}
	cd.ErrorCount++
	duration := r.cooldownForErrorWithBackoff(statusCode, cd.ErrorCount)
	cd.Expiry = time.Now().Add(duration)
	cd.StatusCode = statusCode
	cd.LastError = errMsg
	r.cooldowns[key] = cd
	r.saveCooldowns()
	log.Printf("[debug] model=%s/%s -> cooldown applied (status=%d, errors=%d, duration=%v): %s", ep.Provider, ep.Model, statusCode, cd.ErrorCount, duration, errMsg)
}

func (r *Router) ApplyCooldownFromError(ep *ModelEndpoint, err error) {
	statusCode := 0
	var providerErr *ProviderError
	if errors.As(err, &providerErr) {
		statusCode = providerErr.StatusCode
	}
	errMsg := err.Error()
	r.ApplyCooldown(ep, statusCode, errMsg)
}

func baseCooldownForError(statusCode int) time.Duration {
	switch statusCode {
	case 429:
		return 10 * time.Second
	case 500, 502, 503:
		return 30 * time.Second
	case 504:
		return 60 * time.Second
	case 404:
		return 300 * time.Second
	case 401, 403:
		return 300 * time.Second
	default:
		return 30 * time.Second
	}
}

// cooldownSchedule maps consecutive-error count (1-based) to the cooldown
// duration applied. More consecutive failures escalate the backoff so a model
// that keeps failing is kept out longer: 60s, 1h, 8h, 24h, 3d, 7d.
var cooldownSchedule = []time.Duration{
	60 * time.Second,        // 1st consecutive error
	time.Hour,               // 2nd
	8 * time.Hour,           // 3rd
	24 * time.Hour,          // 4th
	3 * 24 * time.Hour,      // 5th (3 days)
	7 * 24 * time.Hour,      // 6th (7 days)
}

func (r *Router) cooldownForErrorWithBackoff(statusCode int, errorCount int) time.Duration {
	idx := errorCount - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(cooldownSchedule) {
		idx = len(cooldownSchedule) - 1
	}
	return cooldownSchedule[idx]
}

func (r *Router) ResetCooldown(ep *ModelEndpoint) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cooldowns, ep.Key())
	r.saveCooldowns()
}

func (r *Router) GetSession(sessionID string) (ModelEndpoint, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ep, ok := r.sessions[sessionID]
	return ep, ok
}

func (r *Router) GetAllSessions() map[string]ModelEndpoint {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make(map[string]ModelEndpoint, len(r.sessions))
	for k, v := range r.sessions {
		result[k] = v
	}
	return result
}

func (r *Router) GetAllCooldowns() map[string]CooldownEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make(map[string]CooldownEntry, len(r.cooldowns))
	for k, v := range r.cooldowns {
		result[k] = v
	}
	return result
}

type ProviderError struct {
	StatusCode int
	Body       []byte
	Err        error
}

func (e *ProviderError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	if len(e.Body) > 0 {
		return string(e.Body)
	}
	return "provider error"
}

func (e *ProviderError) Unwrap() error {
	return e.Err
}

var ErrNoModelsAvailable = errors.New("no models available")
var ErrInvalidModel = errors.New("invalid model")