package main

import (
	"errors"
	"sync"
	"time"
)

type CooldownEntry struct {
	Expiry      time.Time
	StatusCode  int
	ErrorCount  int
	LastError   string
}

type Router struct {
	config    *Config
	sessions  map[string]ModelEndpoint
	cooldowns map[string]CooldownEntry
	mu        sync.RWMutex
}

func NewRouter(cfg *Config) *Router {
	return &Router{
		config:    cfg,
		sessions:  make(map[string]ModelEndpoint),
		cooldowns: make(map[string]CooldownEntry),
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
	if !ok {
		return nil, 0
	}

	if ep, ok := r.sessions[sessionID]; ok {
		if r.isAvailableLocked(&ep) {
			return &ep, 0
		}
		for _, next := range chain.Chain {
			if r.isAvailableLocked(&next) {
				r.sessions[sessionID] = next
				return &next, 0
			}
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

	duration := cooldownForError(statusCode)
	key := ep.Key()
	cd, ok := r.cooldowns[key]
	if !ok {
		cd = CooldownEntry{}
	}
	cd.Expiry = time.Now().Add(duration)
	cd.StatusCode = statusCode
	cd.ErrorCount++
	cd.LastError = errMsg
	r.cooldowns[key] = cd
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

func cooldownForError(statusCode int) time.Duration {
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

func (r *Router) ResetCooldown(ep *ModelEndpoint) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cooldowns, ep.Key())
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