package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type ProviderConfig struct {
	URL       string `yaml:"url"`
	APIKeyEnv string `yaml:"api_key_env"`
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
	return mc.Chain, true
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

type SSEError struct {
	Data string
}

func (e *SSEError) Error() string {
	return "SSE error event: " + e.Data
}

func ExtractSSEError(data []byte) error {
	for _, line := range splitLines(data) {
		line = trimSpace(line)
		if len(line) == 0 {
			continue
		}
		if bytesHasPrefix(line, []byte("data: ")) {
			payload := line[6:]
			if bytesEqual(payload, []byte("[DONE]")) {
				return nil
			}
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(payload, &obj); err == nil {
				if errRaw, ok := obj["error"]; ok {
					var apiErr struct {
						Error struct {
							Message string `json:"message"`
							Type    string `json:"type"`
							Code    string `json:"code"`
						} `json:"error"`
					}
					_ = json.Unmarshal(errRaw, &apiErr.Error)
					return &SSEError{Data: string(errRaw)}
				}
			}
			return nil
		}
	}
	return nil
}

func splitLines(data []byte) [][]byte {
	return bytesSplit(data, []byte("\n"))
}

func trimSpace(data []byte) []byte {
	return bytesTrimSpace(data)
}

func bytesHasPrefix(data, prefix []byte) bool {
	return len(data) >= len(prefix) && equal(data[:len(prefix)], prefix)
}

func bytesEqual(a, b []byte) bool {
	return len(a) == len(b) && equal(a, b)
}

func equal(a, b []byte) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func bytesSplit(data, sep []byte) [][]byte {
	var result [][]byte
	start := 0
	for i := 0; i <= len(data)-len(sep); i++ {
		if equal(data[i:i+len(sep)], sep) {
			result = append(result, data[start:i])
			start = i + len(sep)
			i += len(sep) - 1
		}
	}
	result = append(result, data[start:])
	return result
}

func bytesTrimSpace(data []byte) []byte {
	start := 0
	for start < len(data) && (data[start] == ' ' || data[start] == '\t' || data[start] == '\r') {
		start++
	}
	end := len(data)
	for end > start && (data[end-1] == ' ' || data[end-1] == '\t' || data[end-1] == '\r') {
		end--
	}
	return data[start:end]
}
