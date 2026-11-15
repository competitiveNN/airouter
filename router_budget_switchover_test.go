package main

import (
	"testing"
	"time"
)

func TestSessionRetryBudgetExhaustionMigratesToSubstituteChain(t *testing.T) {
	cfg := &Config{
		Providers:   map[string]ProviderConfig{},
		Preferences: &Preferences{InitialRotationWindow: &noRotation},
		Models: map[string]ModelConfig{
			"work":  {Chain: xcChain("work", 1)},
			"large": {Chain: xcChain("large", 1)},
			"smart": {Chain: xcChain("smart", 1)},
			"fast":  {Chain: xcChain("fast", 1)},
		},
	}
	for _, m := range cfg.Models {
		for _, ep := range m.Chain {
			cfg.Providers[ep.Provider] = ProviderConfig{URL: "https://x", APIKeyEnv: "K"}
		}
	}
	r := NewRouter(cfg, "")
	defer r.Close()

	sessionID := "s1"
	logicalModel := "work"

	// Bind session to work chain
	ep := cfg.Models[logicalModel].Chain[0]
	r.mu.Lock()
	r.sessions[sessionID] = sessionEntry{
		ep:       ep,
		chain:    logicalModel,
		model:    logicalModel,
		lastUsed: time.Now(),
	}
	r.sessionRetryBudget[sessionID] = DefaultSessionRetryBudget
	r.mu.Unlock()

	if !r.SessionRetryBudgetExhausted(sessionID) {
		t.Fatalf("expected budget exhausted")
	}
	chainBefore := xcSessionChain(r, sessionID)
	if chainBefore != logicalModel {
		t.Fatalf("expected chain %s, got %s", logicalModel, chainBefore)
	}

	migrated := r.MigrateSessionOnBudgetExhaustion(sessionID, logicalModel)
	if !migrated {
		t.Fatalf("expected migration to succeed")
	}
	// Budget should be reset
	if r.SessionRetryBudgetExhausted(sessionID) {
		t.Fatalf("budget should be reset after migration")
	}
	chainAfter := xcSessionChain(r, sessionID)
	if chainAfter == logicalModel {
		t.Fatalf("session should have moved off original chain, still on %s", chainAfter)
	}
	// Chain should be one of substituteChains and not logicalModel
	valid := false
	for _, c := range substituteChains {
		if c == chainAfter {
			valid = true
			break
		}
	}
	if !valid {
		t.Fatalf("expected session to be on a substitute chain, got %s", chainAfter)
	}
	// Verify session can be selected from new chain
	tried := map[string]bool{}
	selected, _ := r.SelectEndpoint(logicalModel, sessionID, false, tried)
	if selected == nil {
		t.Fatalf("expected a selectable endpoint after migration")
	}
	if xcChainOf(r, selected) != chainAfter {
		t.Fatalf("selected endpoint %s not from migrated chain %s", selected.Key(), chainAfter)
	}
}

func TestSessionRetryBudgetExhaustionNoMigrationWhenNoSubstitute(t *testing.T) {
	cfg := &Config{
		Providers:   map[string]ProviderConfig{},
		Preferences: &Preferences{InitialRotationWindow: &noRotation},
		Models: map[string]ModelConfig{
			"work": {Chain: xcChain("work", 1)},
		},
	}
	for _, m := range cfg.Models {
		for _, ep := range m.Chain {
			cfg.Providers[ep.Provider] = ProviderConfig{URL: "https://x", APIKeyEnv: "K"}
		}
	}
	r := NewRouter(cfg, "")
	defer r.Close()

	sessionID := "s2"
	logicalModel := "work"
	ep := cfg.Models[logicalModel].Chain[0]
	r.mu.Lock()
	r.sessions[sessionID] = sessionEntry{
		ep:       ep,
		chain:    logicalModel,
		model:    logicalModel,
		lastUsed: time.Now(),
	}
	r.sessionRetryBudget[sessionID] = DefaultSessionRetryBudget
	r.mu.Unlock()

	migrated := r.MigrateSessionOnBudgetExhaustion(sessionID, logicalModel)
	if migrated {
		t.Fatalf("expected migration to fail when no substitute chains exist")
	}
	// Budget should remain exhausted
	if !r.SessionRetryBudgetExhausted(sessionID) {
		t.Fatalf("budget should still be exhausted")
	}
}
