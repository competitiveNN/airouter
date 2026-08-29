package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

type ChatCompletionRequest struct {
	Model         string                  `json:"model"`
	Messages      []ChatCompletionMessage `json:"messages"`
	Temperature   *float64                `json:"temperature,omitempty"`
	TopP          *float64                `json:"top_p,omitempty"`
	N             *int                    `json:"n,omitempty"`
	Stream        bool                    `json:"stream,omitempty"`
	StreamOptions *StreamOptions          `json:"stream_options,omitempty"`
	MaxTokens     *int                    `json:"max_tokens,omitempty"`
	PresencePenalty *float64              `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64             `json:"frequency_penalty,omitempty"`
	LogitBias     map[string]int          `json:"logit_bias,omitempty"`
	User          string                   `json:"user,omitempty"`
}

type ChatCompletionMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Name    string `json:"name,omitempty"`
}

// UnmarshalJSON tolerates both string content (standard) and array content
// (vision / multimodal requests, where content is a list of text/image parts).
// The raw request body is forwarded verbatim to providers, so we only need
// Content as a string for token estimation; array content is stored verbatim.
func (m *ChatCompletionMessage) UnmarshalJSON(data []byte) error {
	var a struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
		Name    string          `json:"name,omitempty"`
	}
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	m.Role, m.Name = a.Role, a.Name
	m.Content = string(a.Content)
	if len(a.Content) > 1 && a.Content[0] == '"' {
		var s string
		if json.Unmarshal(a.Content, &s) == nil {
			m.Content = s
		}
	}
	return nil
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

type ChatCompletionResponse struct {
	ID      string                       `json:"id"`
	Object  string                       `json:"object"`
	Created int64                        `json:"created"`
	Model   string                       `json:"model"`
	Choices []ChatCompletionChoice       `json:"choices"`
	Usage   *ChatCompletionUsage         `json:"usage,omitempty"`
}

type ChatCompletionChoice struct {
	Index        int                   `json:"index"`
	Message      ChatCompletionMessage `json:"message"`
	FinishReason *string               `json:"finish_reason,omitempty"`
}

type ChatCompletionUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type ChatCompletionStreamResponse struct {
	ID      string                       `json:"id"`
	Object  string                       `json:"object"`
	Created int64                        `json:"created"`
	Model   string                       `json:"model"`
	Choices []ChatCompletionStreamChoice `json:"choices"`
}

type ChatCompletionStreamChoice struct {
	Index        int                        `json:"index"`
	Delta        ChatCompletionStreamDelta  `json:"delta"`
	FinishReason *string                    `json:"finish_reason,omitempty"`
}

type ChatCompletionStreamDelta struct {
	Content string  `json:"content,omitempty"`
	Role    string  `json:"role,omitempty"`
}

type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

type ModelListResponse struct {
	Object string   `json:"object"`
	Data   []Model `json:"data"`
}

type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type GatewayContext struct {
	router     *Router
	proxy      *Proxy
	config     *Config
	configPath string
	gatewayAPIKey string
}

func NewGatewayContext(router *Router, proxy *Proxy, config *Config, configPath, gatewayAPIKey string) *GatewayContext {
	return &GatewayContext{
		router:        router,
		proxy:         proxy,
		config:        config,
		configPath:    configPath,
		gatewayAPIKey: gatewayAPIKey,
	}
}

func (g *GatewayContext) checkAuth(r *http.Request) bool {
	if g.gatewayAPIKey == "" {
		return true
	}
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return false
	}
	parts := strings.SplitN(auth, " ", 2)
	if len(parts) != 2 || parts[0] != "Bearer" {
		return false
	}
	return parts[1] == g.gatewayAPIKey
}

func (g *GatewayContext) getSessionID(r *http.Request) string {
	sessionID := r.Header.Get("X-Session-ID")
	if sessionID == "" {
		sessionID = r.URL.Query().Get("session_id")
	}
	if sessionID == "" {
		sessionID = generateSessionID()
	}
	return sessionID
}

func generateSessionID() string {
	return fmt.Sprintf("sess_%d_%d", time.Now().UnixNano(), time.Now().Nanosecond())
}

func writeAPIError(w http.ResponseWriter, statusCode int, message, errType, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(ErrorResponse{
		Error: ErrorDetail{
			Message: message,
			Type:    errType,
			Code:    code,
		},
	})
}

func writeSSEError(w http.ResponseWriter, flusher http.Flusher, message string) {
	errJSON, _ := json.Marshal(ErrorResponse{
		Error: ErrorDetail{
			Message: message,
			Type:    "server_error",
			Code:    "stream_error",
		},
	})
	fmt.Fprintf(w, "data: %s\n\n", errJSON)
	flusher.Flush()
}

func (g *GatewayContext) HandleModels(w http.ResponseWriter, r *http.Request) {
	if !g.checkAuth(r) {
		writeAPIError(w, 401, "Invalid API key", "authentication_error", "invalid_api_key")
		return
	}
	if r.Method != http.MethodGet {
		writeAPIError(w, 405, "Method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}

	models := make([]Model, 0, len(LogicalModels))
	now := time.Now().Unix()
	for _, name := range LogicalModels {
		models = append(models, Model{
			ID:      name,
			Object:  "model",
			Created: now,
			OwnedBy: "airouter",
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ModelListResponse{
		Object: "list",
		Data:   models,
	})
}

func (g *GatewayContext) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if !g.checkAuth(r) {
		writeAPIError(w, 401, "Invalid API key", "authentication_error", "invalid_api_key")
		return
	}
	if r.Method != http.MethodPost {
		writeAPIError(w, 405, "Method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAPIError(w, 400, "Failed to read request body", "invalid_request_error", "bad_request")
		return
	}

	var req ChatCompletionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeAPIError(w, 400, "Invalid JSON", "invalid_request_error", "bad_json")
		return
	}

	if !IsValidModel(req.Model) {
		writeAPIError(w, 400, fmt.Sprintf("Unknown model: %s. Available: %s", req.Model, strings.Join(LogicalModels, ", ")), "invalid_request_error", "model_not_found")
		return
	}

	sessionID := g.getSessionID(r)

	if req.Stream {
		g.handleStream(w, r, body, &req, sessionID)
	} else {
		g.handleCompletion(w, r, body, &req, sessionID)
	}
}

// estimateTokens approximates the request context size in tokens from the
// prompt messages. We use the common heuristic of ~4 characters per token
// since we don't have a model-specific tokenizer available here.
func estimateTokens(req *ChatCompletionRequest) int {
	totalChars := 0
	for _, m := range req.Messages {
		totalChars += len(m.Content)
	}
	tokens := totalChars / 4
	if tokens < 1 {
		tokens = 1
	}
	return tokens
}

// requestHasVision reports whether the raw request body contains image (vision)
// content, by looking for the OpenAI-style "image_url" part. This is what
// drives capability-aware model selection.
func requestHasVision(body []byte) bool {
	return bytes.Contains(body, []byte("image_url"))
}

// requestTimeout returns a timeout that scales with the request context size:
// 5s for up to 1000 tokens, plus 1s for each additional 10000 tokens.
func requestTimeout(tokens int) time.Duration {
	const base = 5 * time.Second
	if tokens <= 1000 {
		return base
	}
	extra := (tokens - 1000 + 4999) / 5000 // ceil division, 2s per extra 5k tokens
	return base + time.Duration(extra)*time.Second
}

func (g *GatewayContext) handleCompletion(w http.ResponseWriter, r *http.Request, body []byte, req *ChatCompletionRequest, sessionID string) {
	ctx := r.Context()
	tokens := estimateTokens(req)
	timeout := requestTimeout(tokens)

	for {
		ep, wait := g.router.SelectEndpoint(req.Model, sessionID, requestHasVision(body))
		if ep == nil {
			if wait > 0 {
				select {
				case <-ctx.Done():
					writeAPIError(w, 503, "Request cancelled", "server_error", "cancelled")
					return
				case <-time.After(minDuration(wait, 30*time.Second)):
					continue
				}
			}
			writeAPIError(w, 503, "All models are currently unavailable", "rate_limit_error", "all_models_unavailable")
			return
		}

		reqCtx, cancel := context.WithTimeout(ctx, timeout)
		log.Printf("[debug] session=%s model=%s -> request -> %s/%s (timeout=%v, ~%d tokens)", sessionID, req.Model, ep.Provider, ep.Model, timeout, tokens)
		resp, err := g.proxy.Forward(reqCtx, body, *ep)
		if err != nil {
			cancel()
			g.router.ApplyCooldownFromError(ep, err)
			continue
		}
		if resp.StatusCode != 200 {
			respBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			cancel()
			g.router.ApplyCooldown(ep, resp.StatusCode, string(respBody))
			continue
		}

		for k, v := range resp.Header {
			if k != "Transfer-Encoding" && k != "Connection" {
				w.Header()[k] = v
			}
		}
		w.WriteHeader(200)
		io.Copy(w, resp.Body)
		resp.Body.Close()
		cancel()
		return
	}
}

func (g *GatewayContext) handleStream(w http.ResponseWriter, r *http.Request, body []byte, req *ChatCompletionRequest, sessionID string) {
	ctx := r.Context()
	tokens := estimateTokens(req)
	timeout := requestTimeout(tokens)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIError(w, 500, "Streaming not supported", "server_error", "streaming_unsupported")
		return
	}

	for {
		ep, wait := g.router.SelectEndpoint(req.Model, sessionID, requestHasVision(body))
		if ep == nil {
			if wait > 0 {
				fmt.Fprintf(w, ": reconnecting in %v\n\n", wait.Round(time.Second))
				flusher.Flush()
				select {
				case <-ctx.Done():
					return
				case <-time.After(minDuration(wait, 30*time.Second)):
					continue
				}
			}
			writeSSEError(w, flusher, "All models are currently unavailable")
			return
		}

		log.Printf("[debug] session=%s model=%s -> request -> %s/%s (timeout=%v, ~%d tokens)", sessionID, req.Model, ep.Provider, ep.Model, timeout, tokens)
		// StreamToClient buffers the start of the stream and only flushes to the
		// client once the first content token arrives, so an early upstream
		// error is swallowed and we fall back with no output sent to the client.
		// The size-based timeout guards only time-to-first-token; the parent
		// context keeps the connection alive for the rest of a long generation.
		err := g.proxy.StreamToClient(ctx, w, flusher, body, *ep, timeout)
		if err == nil {
			g.router.RecordSuccess(ep)
			return
		}

		g.router.ApplyCooldownFromError(ep, err)
		continue
	}
}

func (g *GatewayContext) HandleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, 405, "Method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "ok",
		"time":   time.Now().Format(time.RFC3339),
	})
}

func (g *GatewayContext) HandleAdminProviders(w http.ResponseWriter, r *http.Request) {
	if !g.checkAuth(r) {
		writeAPIError(w, 401, "Invalid API key", "authentication_error", "invalid_api_key")
		return
	}
	if r.Method != http.MethodGet {
		writeAPIError(w, 405, "Method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}

	type ProviderInfo struct {
		Name     string `json:"name"`
		URL      string `json:"url"`
		HasKey   bool   `json:"has_key"`
	}
	providers := make([]ProviderInfo, 0, len(g.config.Providers))
	for name, p := range g.config.Providers {
		providers = append(providers, ProviderInfo{
			Name:   name,
			URL:    p.URL,
			HasKey: p.APIKey() != "",
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"providers": providers,
	})
}

func (g *GatewayContext) HandleAdminSessions(w http.ResponseWriter, r *http.Request) {
	if !g.checkAuth(r) {
		writeAPIError(w, 401, "Invalid API key", "authentication_error", "invalid_api_key")
		return
	}
	if r.Method != http.MethodGet {
		writeAPIError(w, 405, "Method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}

	sessions := g.router.GetAllSessions()
	type SessionInfo struct {
		SessionID string `json:"session_id"`
		Provider  string `json:"provider"`
		Model     string `json:"model"`
	}
	result := make([]SessionInfo, 0, len(sessions))
	for sid, ep := range sessions {
		result = append(result, SessionInfo{
			SessionID: sid,
			Provider:  ep.Provider,
			Model:     ep.Model,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessions": result,
		"count":    len(result),
	})
}

func (g *GatewayContext) HandleAdminCooldowns(w http.ResponseWriter, r *http.Request) {
	if !g.checkAuth(r) {
		writeAPIError(w, 401, "Invalid API key", "authentication_error", "invalid_api_key")
		return
	}
	if r.Method != http.MethodGet {
		writeAPIError(w, 405, "Method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}

	cooldowns := g.router.GetAllCooldowns()
	type CooldownInfo struct {
		ModelKey   string    `json:"model_key"`
		Expiry     time.Time `json:"expiry"`
		StatusCode int       `json:"status_code"`
		ErrorCount int       `json:"error_count"`
		LastError  string    `json:"last_error"`
	}
	result := make([]CooldownInfo, 0, len(cooldowns))
	for key, cd := range cooldowns {
		result = append(result, CooldownInfo{
			ModelKey:   key,
			Expiry:     cd.Expiry,
			StatusCode: cd.StatusCode,
			ErrorCount: cd.ErrorCount,
			LastError:  cd.LastError,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"cooldowns": result,
		"count":     len(result),
	})
}

// ReloadConfig swaps the in-memory configuration used by the gateway, router
// and proxy without restarting the server. Sessions and cooldowns are kept, so
// sticky routing and backoff state survive a reload. Used by the admin config
// endpoint and the on-disk config file watcher.
func (g *GatewayContext) ReloadConfig(cfg *Config) {
	g.config = cfg
	g.router.config = cfg
	g.proxy.config = cfg
	log.Printf("[debug] config reloaded: %d providers, %d logical models", len(cfg.Providers), len(cfg.Models))
}

func (g *GatewayContext) HandleAdminConfig(w http.ResponseWriter, r *http.Request) {
	if !g.checkAuth(r) {
		writeAPIError(w, 401, "Invalid API key", "authentication_error", "invalid_api_key")
		return
	}
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		// Return config without exposing secrets (api_key_env only)
		// Config struct is safe to marshal
		json.NewEncoder(w).Encode(g.config)
	case http.MethodPost, http.MethodPut:
		var cfg Config
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			writeAPIError(w, 400, "Invalid JSON", "invalid_request_error", "bad_json")
			return
		}
		if err := SaveConfig(g.configPath, &cfg); err != nil {
			writeAPIError(w, 400, err.Error(), "invalid_request_error", "validation_error")
			return
		}
		// Reload in memory
		newCfg, err := LoadConfig(g.configPath)
		if err != nil {
			writeAPIError(w, 500, "Failed to reload config", "server_error", "reload_failed")
			return
		}
		g.ReloadConfig(newCfg)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	default:
		writeAPIError(w, 405, "Method not allowed", "invalid_request_error", "method_not_allowed")
	}
}

func (g *GatewayContext) HandleAdminDashboard(w http.ResponseWriter, r *http.Request) {
	if !g.checkAuth(r) {
		writeAPIError(w, 401, "Invalid API key", "authentication_error", "invalid_api_key")
		return
	}
	// Simple static dashboard HTML
	html := `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>AI Router Dashboard</title>
<style>
body{font-family:system-ui,sans-serif;margin:0;padding:0;background:#f6f7fb;color:#111}
header{background:#0f172a;color:#fff;padding:1rem 2rem;display:flex;justify-content:space-between;align-items:center}
main{padding:2rem}
.card{background:#fff;border-radius:12px;box-shadow:0 1px 3px rgba(0,0,0,.1);padding:1.5rem;margin-bottom:1.5rem}
h2{margin-top:0}
textarea{background:#0f172a;color:#e2e8f0;padding:1rem;border-radius:8px;overflow:auto;font-family:ui-monospace,monospace}
button{background:#2563eb;color:#fff;border:none;padding:.6rem 1rem;border-radius:8px;cursor:pointer;margin-right:.5rem}
button:disabled{opacity:.5}
</style>
</head>
<body>
<header><h1>AI Router Dashboard</h1><span id="status"></span></header>
<main>
<div class="card">
<h2>Config Editor</h2>
<p>Load and edit config (JSON). Save writes back to config.yaml and reloads.</p>
<button id="load">Load Config</button>
<button id="save" disabled>Save Config</button>
<textarea id="editor" rows="30" style="width:100%;"></textarea>
</div>
<div class="card">
<h2>Quick View</h2>
<div id="summary"></div>
</div>
</main>
<script>
const apiKey = prompt('Enter gateway API key for admin:') || '';
async function req(path, opts={}) {
  const headers = Object.assign({'Content-Type':'application/json'}, opts.headers||{});
  if(apiKey) headers['Authorization']='Bearer '+apiKey;
  const res = await fetch(path,{...opts,headers});
  if(!res.ok){ const t=await res.text(); throw new Error(t); }
  return res.headers.get('content-type')?.includes('application/json')? res.json(): res.text();
}
let cfg=null;
const editor=document.getElementById('editor');
document.getElementById('load').onclick=async()=>{
  try{
    cfg=await req('/admin/config');
    editor.value=JSON.stringify(cfg,null,2);
    document.getElementById('save').disabled=false;
    renderSummary(cfg);
    document.getElementById('status').textContent='Loaded';
  }catch(e){ alert(e); }
};
document.getElementById('save').onclick=async()=>{
  try{
    const parsed=JSON.parse(editor.value);
    await req('/admin/config',{method:'POST',body:JSON.stringify(parsed)});
    alert('Saved and reloaded');
    document.getElementById('status').textContent='Saved';
  }catch(e){ alert('Save failed: '+e); }
};
function renderSummary(c){
  const providers=Object.keys(c.providers||{}).length;
  const models=Object.keys(c.models||{}).length;
  document.getElementById('summary').innerHTML='<b>Providers:</b> '+providers+'<br><b>Logical models:</b> '+models;
}
document.getElementById('load').click();
</script>
</body>
</html>`
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(html))
}

var (
	sessionIDMu sync.Mutex
	sessionIDCounter int64
)
