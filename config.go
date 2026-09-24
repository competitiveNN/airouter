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

type ModelConfig struct {
	Chain []ModelEndpoint `yaml:"chain"`
}

type Config struct {
	Providers map[string]ProviderConfig `yaml:"providers"`
	Models    map[string]ModelConfig    `yaml:"models"`
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
