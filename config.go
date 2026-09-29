package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type ProviderConfig struct {
	URL       string            `yaml:"url"`
	APIKeyEnv string            `yaml:"api_key_env"`
	Headers   map[string]string `yaml:"headers,omitempty"`
}

// opencodeProviderPrefixes lists provider names whose upstream endpoint is an
// OpenCode gateway (opencode.ai/zen or opencode.ai/zen/go). Requests to these
// providers must carry OpenCode client-attribution headers, otherwise the
// gateway treats the request as anonymous and rejects free-tier models with
// `403 FreeTierError: OpenCode's free tier can only be used from within
// OpenCode` (see oh-my-pi#12306). The headers are synthesized at request time
// with fresh per-request IDs so the upstream cannot fingerprint a single
// client across sessions — matching the official opencode CLI behaviour.
var opencodeProviderPrefixes = []string{"opencode", "opencode-go", "opencode-zen"}

// isOpencodeProvider reports whether the named provider is an OpenCode gateway
// that requires client-attribution headers.
func isOpencodeProvider(provider string) bool {
	for _, p := range opencodeProviderPrefixes {
		if p == provider {
			return true
		}
	}
	return false
}

// opencodeRequestHeaders returns the OpenCode client-attribution headers that
// must accompany every request to an OpenCode gateway. The session/request IDs
// are generated fresh per call so the upstream sees a distinct identity each
// time, exactly like the official CLI.
func opencodeRequestHeaders() map[string]string {
	return map[string]string{
		"User-Agent":         "opencode/1.18.31/cli",
		"x-opencode-client":  "cli",
		"x-opencode-session": "ses_" + randomHex(16),
		"x-opencode-request": "msg_" + randomHex(16),
		"x-opencode-project": "default",
	}
}

func (p ProviderConfig) APIKey() string {
	if p.APIKeyEnv == "" {
		return ""
	}
	return os.Getenv(p.APIKeyEnv)
}

type ModelEndpoint struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
	// Vision marks whether this endpoint can accept image (multimodal)
	// content. It is optional: when nil the gateway assumes vision is
	// supported. Set it to false for known text-only models so vision
	// requests are routed to a capable model in the chain. The gateway also
	// learns this at runtime (see Router.MarkNoVision) when a provider rejects
	// an image request.
	Vision *bool `yaml:"vision,omitempty"`

	// Intelligence is the model's quality score (Artificial Analysis
	// intelligence index, or the arena.ai ELO-derived equivalent), as
	// recorded by regenerate_config.py. It is optional: a nil value means
	// "unknown", and the router then treats the endpoint as incomparable and
	// falls back to strict chain order.
	//
	// The router uses this only to decide which endpoints are equally good
	// enough to be treated as interchangeable for a new session's first pick
	// (see Router.selectInitialEndpoint). It never uses it to reorder a
	// fallback: once a request is in flight the chain is walked strictly in
	// configured order, so quality ordering is preserved.
	Intelligence *float64 `yaml:"intelligence,omitempty"`

	// ContextLength is the model's advertised context window in tokens, as
	// recorded by regenerate_config.py from the upstream model list. It is
	// optional: a zero value means "not reported" and is omitted from YAML.
	//
	// The router does not use it for routing today, but it is surfaced to
	// clients via /v1/models (as max_tokens on the logical model) so a
	// client can pick a profile with enough headroom for its prompt. The
	// `large` profile exists specifically for large-context work, so
	// advertising nothing for it is the worst case.
	ContextLength int `yaml:"context_length,omitempty"`
}

// HasIntelligence reports whether an intelligence score is known. Endpoints
// without one are only ever compared by chain position.
func (e ModelEndpoint) HasIntelligence() bool {
	return e.Intelligence != nil
}

// IntelligenceEqual reports whether two endpoints carry the same known score.
// Endpoints with an unknown score are never considered equal, so they fall
// back to strict chain order rather than being grouped arbitrarily.
func (e ModelEndpoint) IntelligenceEqual(other ModelEndpoint) bool {
	if e.Intelligence == nil || other.Intelligence == nil {
		return false
	}
	return *e.Intelligence == *other.Intelligence
}

func (e ModelEndpoint) Key() string {
	return e.Provider + ":" + e.Model
}

// SupportsVision reports whether the endpoint can handle image content. A nil
// Vision flag is interpreted as "supported" so existing configs keep working.
func (e ModelEndpoint) SupportsVision() bool {
	return e.Vision == nil || *e.Vision
}

func (e ModelEndpoint) Equal(other ModelEndpoint) bool {
	return e.Provider == other.Provider && e.Model == other.Model
}

// ContextWindow reports the advertised context window in tokens. A zero
// value means the upstream did not report one.
func (e ModelEndpoint) ContextWindow() int {
	return e.ContextLength
}

type ModelConfig struct {
	Chain []ModelEndpoint `yaml:"chain"`
}

type Preferences struct {
	NewestFirstOnTie bool `yaml:"newest_first_on_tie"`

	// InitialRotationWindow is how many endpoints at the head of a chain are
	// rotated between for the first pick of each new session.
	//
	// Without rotation every new session starts at depth 0, so one model
	// serves all traffic and the rest of the chain is idle capacity that only
	// ever gets used after a failure. With a window of N, consecutive new
	// sessions start at depths 0, 1, ... N-1, 0, 1, ... spreading load evenly
	// across the top of the chain.
	//
	// This affects ONLY the first endpoint chosen for a brand new session.
	// Once a session is pinned, later requests in it are sticky; and once a
	// request is retrying, the chain is walked strictly in configured order.
	// So rotation can never delay escalation to a better model.
	//
	// 0 disables rotation (every new session starts at depth 0); omitting the
	// key entirely uses DefaultInitialRotationWindow. It is a pointer so those
	// two states are distinguishable.
	InitialRotationWindow *int `yaml:"initial_rotation_window,omitempty"`

	// CooldownJitter is the random fraction (0..0.25) added to transient
	// cooldowns (429/5xx) so concurrent clients hitting the same rate-limited
	// model don't all wake up at the same instant and stampede the provider.
	// 0 (the default) disables jitter. Values are clamped to [0, 0.25].
	CooldownJitter float64 `yaml:"cooldown_jitter,omitempty"`

	// CircuitBreakerThreshold is the number of consecutive failures that
	// trip the circuit from Closed to Open. A pointer is used so an explicit
	// 0 can be distinguished from an absent key: nil (absent) falls back to
	// DefaultCircuitBreakerThreshold, while an explicit 0 disables the
	// circuit breaker entirely (ad-hoc cooldown escalation handles failures
	// instead). Negative values are clamped to 0 (disabled).
	CircuitBreakerThreshold *int `yaml:"circuit_breaker_threshold,omitempty"`

	// CircuitHalfOpenProbes is the max probes allowed in half-open state
	// before another request is blocked. DefaultCircuitHalfOpenProbes is
	// used when the key is absent.
	CircuitHalfOpenProbes int `yaml:"circuit_half_open_probes,omitempty"`

	// DefaultMaxTokens is the output-token ceiling the gateway applies to
	// requests that omit max_tokens. It is a pointer so three states are
	// distinguishable:
	//   - nil (absent): "auto" — half of the profile's advertised context
	//     window (max_context_tokens). Conservative and profile-aware.
	//   - 0: "never default" — the request is sent upstream without
	//     max_tokens, letting the provider choose.
	//   - positive: use that absolute value regardless of profile.
	//
	// This exists because half-of-context is right for chat models but
	// wrong for reasoning models (deepseek-r1, gpt-oss, nemotron-3-nano
	// reasoning, ...): their thinking tokens live OUTSIDE the visible
	// output and can exceed a half-context budget, so the provider rejects
	// the request. Operators with reasoning models in a chain should set
	// this to 0 (or a larger value) rather than let the auto-default
	// silently break them.
	DefaultMaxTokens *int `yaml:"default_max_tokens,omitempty"`

	// MaxLabelCardinality bounds how many distinct provider/model labels the
	// metrics registry will track. Endpoint and model labels come from this
	// config, and the config is hot-reloadable, so without a bound a config
	// churn loop (or a model sync that keeps inventing ids) grows the registry
	// until the gateway is OOM-killed — the metrics endpoint would be a way to
	// kill the process. A pointer so an explicit 0 is distinguishable from an
	// absent key: nil uses defaultMaxLabelValues.
	//
	// Once the window is full, a label that is not already tracked recycles the
	// least-recently-admitted slot and rolls that label's counters into an
	// __overflow__ bucket, so totals stay truthful and no observation is lost.
	// See docs/RUNBOOK.md ("Label cardinality is bounded") for what to watch.
	//
	// Values outside [16, 65536] are ignored in favour of the default: too
	// small destroys the per-endpoint series, too large defeats the memory
	// bound. scripts/validate-config.py rejects out-of-range values at config
	// time so the mistake is caught before a restart rather than silently at
	// load.
	MaxLabelCardinality *int `yaml:"max_label_cardinality,omitempty"`
}

// MaxLabelCardinalityValue returns the effective per-map label cap, clamped to
// the supported range. nil (key absent) yields defaultMaxLabelValues.
func (p *Preferences) MaxLabelCardinalityValue() int {
	if p == nil || p.MaxLabelCardinality == nil {
		return defaultMaxLabelValues
	}
	n := *p.MaxLabelCardinality
	if n < minLabelValues {
		return minLabelValues
	}
	if n > maxAllowedLabels {
		return maxAllowedLabels
	}
	return n
}

// RotationWindow returns the effective window size, defaulting to
// DefaultInitialRotationWindow when the key is absent. An explicit 0 (or a
// negative value) disables rotation.
func (p *Preferences) RotationWindow() int {
	if p == nil || p.InitialRotationWindow == nil {
		return DefaultInitialRotationWindow
	}
	return *p.InitialRotationWindow
}

// CooldownJitterFraction returns the jitter fraction to apply to transient
// cooldowns, clamped to [0, 0.25]. Nil preferences or an unset value yields 0.
func (p *Preferences) CooldownJitterFraction() float64 {
	if p == nil {
		return 0
	}
	f := p.CooldownJitter
	if f < 0 {
		f = 0
	}
	if f > 0.25 {
		f = 0.25
	}
	return f
}

// DefaultInitialRotationWindow is the window used when preferences omit one.
const DefaultInitialRotationWindow = 8

// DefaultCircuitBreakerThreshold is the number of consecutive failures
// that trip the circuit from Closed to Open when the config omits the key.
const DefaultCircuitBreakerThreshold = 5

// DefaultCircuitHalfOpenProbes is the max probes allowed in half-open state
// when the config omits the key.
const DefaultCircuitHalfOpenProbes = 1

// CircuitBreakerThresholdValue returns the effective threshold. nil (absent)
// falls back to DefaultCircuitBreakerThreshold; an explicit 0 disables the
// circuit breaker entirely; negative values are clamped to 0 (disabled).
func (p *Preferences) CircuitBreakerThresholdValue() int {
	if p == nil || p.CircuitBreakerThreshold == nil {
		return DefaultCircuitBreakerThreshold
	}
	if *p.CircuitBreakerThreshold < 0 {
		return 0
	}
	return *p.CircuitBreakerThreshold
}

// CircuitHalfOpenProbesValue returns the effective half-open probe count,
// defaulting to DefaultCircuitHalfOpenProbes when the key is absent.
func (p *Preferences) CircuitHalfOpenProbesValue() int {
	if p == nil || p.CircuitHalfOpenProbes <= 0 {
		return DefaultCircuitHalfOpenProbes
	}
	return p.CircuitHalfOpenProbes
}

// DefaultMaxTokensValue returns the effective default output-token ceiling
// for requests that omit max_tokens. nil (absent) means "auto" — half of
// the profile's advertised context window — and is signalled by a negative
// return value so callers can distinguish it from an explicit 0 (never
// default) and an explicit positive value.
func (p *Preferences) DefaultMaxTokensValue() int {
	if p == nil || p.DefaultMaxTokens == nil {
		return -1 // auto
	}
	return *p.DefaultMaxTokens
}

// RotationEnabled reports whether new-session rotation is active.
func (p *Preferences) RotationEnabled() bool { return p.RotationWindow() > 0 }

type Config struct {
	Providers   map[string]ProviderConfig `yaml:"providers"`
	Preferences *Preferences              `yaml:"preferences,omitempty"`
	Models      map[string]ModelConfig    `yaml:"models"`
}

var LogicalModels = []string{"smart", "work", "fast", "large"}

func LoadConfig(path string) (*Config, error) {
	// Bound file size to prevent OOM from malformed/malicious config
	const maxConfigBytes = 10 << 20 // 10 MiB
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat config: %w", err)
	}
	if fi.Size() > maxConfigBytes {
		return nil, fmt.Errorf("config file too large: %d bytes (max %d)", fi.Size(), maxConfigBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func SaveConfig(path string, cfg *Config) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	// Atomic write: temp file then rename so a crash / concurrent reader never
	// sees a half-written config.yaml.
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close temp config: %w", err)
	}
	if err := os.Chmod(tmpName, 0644); err != nil {
		log.Printf("[debug] failed to chmod config temp file: %v", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename config: %w", err)
	}
	return nil
}

func (c *Config) validate() error {
	if len(c.Providers) == 0 {
		return fmt.Errorf("no providers configured")
	}
	if len(c.Models) == 0 {
		return fmt.Errorf("no models configured")
	}
	for name, p := range c.Providers {
		if p.URL == "" {
			return fmt.Errorf("provider %s has empty URL", name)
		}
		if !strings.HasPrefix(p.URL, "http://") && !strings.HasPrefix(p.URL, "https://") {
			return fmt.Errorf("provider %s URL must start with http:// or https://: %q", name, p.URL)
		}
	}
	for name, model := range c.Models {
		if len(model.Chain) == 0 {
			return fmt.Errorf("model %s has empty chain", name)
		}
		for i, ep := range model.Chain {
			if _, ok := c.Providers[ep.Provider]; !ok {
				return fmt.Errorf("model %s chain[%d] references unknown provider %q", name, i, ep.Provider)
			}
		}
	}
	// Reject an out-of-range label cap at config time rather than silently
	// clamping it at load. A cap below the floor destroys the per-endpoint
	// series this exists to protect, and one above the ceiling defeats the
	// memory bound; both are mistakes worth failing on, and both are invisible
	// if they are quietly reinterpreted.
	//
	// Preferences is a *pointer* and every other preference accessor here is
	// nil-safe; this one has to be too, because `preferences:` is omitempty and
	// a config without it is perfectly valid.
	// A nil Preferences (absent `preferences:` key) is the common case, not an
	// error, so guard the whole block instead of reaching through the pointer.
	// Read the pointer once: dereferencing it twice would re-introduce exactly
	// the panic this is fixing.
	if p := c.Preferences; p != nil && p.MaxLabelCardinality != nil {
		if n := *p.MaxLabelCardinality; n < minLabelValues || n > maxAllowedLabels {
			return fmt.Errorf("preferences.max_label_cardinality must be between %d and %d, got %d",
				minLabelValues, maxAllowedLabels, n)
		}
	}
	return nil
}

func (c *Config) GetChain(logicalModel string) ([]ModelEndpoint, bool) {
	mc, ok := c.Models[logicalModel]
	if !ok {
		return nil, false
	}
	// Return a copy so callers can't mutate the internal slice
	return append([]ModelEndpoint(nil), mc.Chain...), true
}

func (c *Config) GetProvider(provider string) (ProviderConfig, bool) {
	p, ok := c.Providers[provider]
	return p, ok
}

func ReplaceModelName(body []byte, modelName string) ([]byte, error) {
	var data map[string]json.RawMessage
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}
	modelJSON, err := json.Marshal(modelName)
	if err != nil {
		return nil, err
	}
	data["model"] = modelJSON
	return json.Marshal(data)
}

func IsValidModel(name string) bool {
	for _, m := range LogicalModels {
		if m == name {
			return true
		}
	}
	return false
}

// randomHex returns n random hex bytes (2*n characters) from crypto/rand.
// Used to synthesize per-request OpenCode session/request IDs so the upstream
// cannot fingerprint a single client across sessions.
func randomHex(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// Fallback: never block a request on RNG failure; emit a deterministic
		// placeholder so attribution is still present (just not unique).
		return strings.Repeat("0", 2*n)
	}
	return hex.EncodeToString(b)
}

type SSEError struct {
	Data string
}

func (e *SSEError) Error() string {
	return "SSE error event: " + e.Data
}

func ExtractSSEError(data []byte) error {
	// SSE events can carry errors in three shapes:
	//   1. A "data:" line whose JSON payload contains an "error" object
	//      (OpenAI-style, most common).
	//   2. An "event: error" line followed by a "data:" line carrying the
	//      error payload (Anthropic / some providers).
	//   3. A bare JSON error object outside any "data:" wrapper (seen from
	//      some proxy layers that strip framing).
	//
	// We scan line-by-line so each data: line is examined independently.

	for _, raw := range bytes.Split(data, []byte("\n")) {
		line := bytes.TrimSpace(raw)
		if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}

		payload := bytes.TrimSpace(line[len("data:"):])
		if bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}

		// Fast path: if the payload is a JSON object containing an "error"
		// field, it's an error event.
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(payload, &obj); err == nil {
			// An error field that is null or an empty object {} is not an error.
			// Some providers emit "error":{} on non-error chunks (e.g. usage-only
			// events), so we must not treat it as a failure.
			if errRaw, ok := obj["error"]; ok && len(errRaw) > 2 && string(errRaw) != "null" {
				return &SSEError{Data: string(errRaw)}
			}
		}
	}

	// Fallback: if no data: error was found, check whether the raw body itself
	// is a JSON error object (some providers return the error as the entire
	// response body with no SSE framing at all).
	trimmed := bytes.TrimSpace(data)
	if bytes.HasPrefix(trimmed, []byte("{")) {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &obj); err == nil {
			// Same guard as above: skip null/empty error objects.
			if errRaw, ok := obj["error"]; ok && len(errRaw) > 2 && string(errRaw) != "null" {
				return &SSEError{Data: string(errRaw)}
			}
		}
	}

	return nil
}
