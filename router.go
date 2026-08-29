package main

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"strings"
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
	noVision     map[string]bool // endpoints that rejected an image request
	rrCounters   map[string]int
	cooldownPath string
	mu           sync.RWMutex
}

func NewRouter(cfg *Config, cooldownPath string) *Router {
	r := &Router{
		config:       cfg,
		sessions:     make(map[string]ModelEndpoint),
		cooldowns:    make(map[string]CooldownEntry),
		noVision:     make(map[string]bool),
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

func (r *Router) SelectEndpoint(logicalModel, sessionID string, requireVision bool) (*ModelEndpoint, time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	chain, ok := r.config.Models[logicalModel]
	if !ok || len(chain.Chain) == 0 {
		return nil, 0
	}

	if ep, ok := r.sessions[sessionID]; ok {
		available := r.isAvailableLocked(&ep)
		if available && r.visionEligible(&ep, requireVision) {
			return &ep, 0
		}
		// The sticky model is out: either cooled (routine, don't log) or it
		// can't handle this request's capability (e.g. no vision). Only the
		// latter is worth a log line.
		capabilityFallback := available && !r.visionEligible(&ep, requireVision)
		for _, next := range chain.Chain {
			if r.isEligibleLocked(&next, requireVision) {
				if capabilityFallback {
					log.Printf("[debug] session=%s model=%s -> fallback (no vision) %s/%s -> %s/%s", sessionID, logicalModel, ep.Provider, ep.Model, next.Provider, next.Model)
				}
				r.sessions[sessionID] = next
				return &next, 0
			}
		}
	}

	// New session
	for _, ep := range chain.Chain {
		if r.isEligibleLocked(&ep, requireVision) {
			log.Printf("[debug] session=%s model=%s -> new session -> %s/%s", sessionID, logicalModel, ep.Provider, ep.Model)
			r.sessions[sessionID] = ep
			return &ep, 0
		}
	}

	return nil, r.minCooldownWait(chain.Chain)
}

func (r *Router) SelectNext(logicalModel, sessionID string, current *ModelEndpoint, requireVision bool) (*ModelEndpoint, time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	chain, ok := r.config.Models[logicalModel]
	if !ok {
		return nil, 0
	}

	found := false
	for _, ep := range chain.Chain {
		if found {
			if r.isEligibleLocked(&ep, requireVision) {
				r.sessions[sessionID] = ep
				return &ep, 0
			}
		}
		if ep.Equal(*current) {
			found = true
		}
	}

	for _, ep := range chain.Chain {
		if r.isEligibleLocked(&ep, requireVision) {
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
	duration := r.cooldownForError(statusCode, cd.ErrorCount)
	cd.Expiry = time.Now().Add(duration)
	cd.StatusCode = statusCode
	cd.LastError = errMsg
	r.cooldowns[key] = cd
	if isVisionUnsupported(errMsg) {
		r.noVision[key] = true
	}
	r.saveCooldowns()
	log.Printf("[debug] cooldown %s/%s status=%d errors=%d for %v: %s", ep.Provider, ep.Model, statusCode, cd.ErrorCount, duration, summarizeError(errMsg))
}

// summarizeError reduces a provider error body to a single short line for logs
// so we don't dump multi-kilobyte JSON (e.g. Gemini's full error payload) on
// every cooldown event.
func summarizeError(msg string) string {
	msg = strings.TrimSpace(msg)
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	const max = 120
	if len(msg) > max {
		return msg[:max] + "..."
	}
	return msg
}

// RecordSuccess resets a model's cooldown on a successful response so the error
// count only reflects *recent* consecutive failures and escalation can't run away.
func (r *Router) RecordSuccess(ep *ModelEndpoint) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.cooldowns[ep.Key()]; ok {
		delete(r.cooldowns, ep.Key())
		r.saveCooldowns()
	}
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

// cooldownForError returns how long to keep a model out of rotation after a
// failure. The base duration depends on the error type (see
// baseCooldownForError); repeated failures within the same cooldown window
// escalate it, but always bounded so a transient burst can never disable a
// model for more than a few minutes. The error count is reset on a successful
// request (see Router.RecordSuccess), so escalation only reflects recent
// consecutive failures.
func (r *Router) cooldownForError(statusCode int, errorCount int) time.Duration {
	base := baseCooldownForError(statusCode)
	if errorCount <= 1 {
		return base
	}
	factor := time.Duration(errorCount)
	if factor > 6 {
		factor = 6
	}
	d := base * factor
	const maxCooldown = 10 * time.Minute
	if d > maxCooldown {
		d = maxCooldown
	}
	return d
}

func (r *Router) ResetCooldown(ep *ModelEndpoint) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cooldowns, ep.Key())
	r.saveCooldowns()
}

// MarkNoVision records that an endpoint rejected an image request, so it is
// skipped for subsequent vision requests (without a cooldown, since the model
// itself is healthy). Runtime-only; not persisted.
func (r *Router) MarkNoVision(ep *ModelEndpoint) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.noVision[ep.Key()] = true
}

// visionEligible reports whether ep can serve a request that requires vision.
// Non-vision requests (requireVision=false) are always eligible.
func (r *Router) visionEligible(ep *ModelEndpoint, requireVision bool) bool {
	if !requireVision {
		return true
	}
	return ep.SupportsVision() && !r.noVision[ep.Key()]
}

// isEligibleLocked combines cooldown availability with capability eligibility.
func (r *Router) isEligibleLocked(ep *ModelEndpoint, requireVision bool) bool {
	return r.isAvailableLocked(ep) && r.visionEligible(ep, requireVision)
}

// isVisionUnsupported reports whether a provider error indicates the model
// cannot handle image/multimodal content, so we can mark it noVision.
func isVisionUnsupported(msg string) bool {
	s := strings.ToLower(msg)
	return strings.Contains(s, "image_url") ||
		strings.Contains(s, "multimodal") ||
		(strings.Contains(s, "vision") && strings.Contains(s, "support")) ||
		strings.Contains(s, "does not support image")
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