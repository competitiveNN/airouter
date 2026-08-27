package main

import (
	"bufio"
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

type Proxy struct {
	client *http.Client
	config *Config
}

func NewProxy(cfg *Config) *Proxy {
	return &Proxy{
		client: &http.Client{
			Timeout: 0,
			Transport: &http.Transport{
				MaxIdleConns:        200,
				MaxIdleConnsPerHost: 50,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		config: cfg,
	}
}

// geminiStripFields lists top-level request fields that Gemini's
// OpenAI-compatible endpoint rejects. The router otherwise forwards the raw
// client body, so these are removed before sending upstream. Applies to all
// providers whose name starts with "gemini" (gemini, gemini2, ...).
var geminiStripFields = []string{"thinking", "thinking_budget", "reasoning_effort"}

// sanitizeRequestBody removes provider-specific unsupported fields from the
// request JSON. It returns the original body unchanged if nothing applies or
// on a parse failure (fail-open so we never drop a valid request).
func sanitizeRequestBody(body []byte, provider string) []byte {
	var fields []string
	if strings.HasPrefix(provider, "gemini") {
		fields = geminiStripFields
	}
	if len(fields) == 0 {
		return body
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	changed := false
	for _, f := range fields {
		if _, ok := obj[f]; ok {
			delete(obj, f)
			changed = true
		}
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	log.Printf("[debug] provider=%s -> stripped unsupported fields %v", provider, fields)
	return out
}

func (p *Proxy) buildRequest(ctx context.Context, body []byte, endpoint ModelEndpoint, providerCfg *ProviderConfig, stream bool) (*http.Request, error) {
	backendBody, err := ReplaceModelName(body, endpoint.Model)
	if err != nil {
		return nil, fmt.Errorf("replace model name: %w", err)
	}
	backendBody = sanitizeRequestBody(backendBody, endpoint.Provider)

	req, err := http.NewRequestWithContext(ctx, "POST", providerCfg.URL+"/chat/completions", bytes.NewReader(backendBody))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	if providerCfg.APIKey() != "" {
		req.Header.Set("Authorization", "Bearer "+providerCfg.APIKey())
	}
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}

	return req, nil
}

func (p *Proxy) Forward(ctx context.Context, body []byte, endpoint ModelEndpoint) (*http.Response, error) {
	providerCfg, ok := p.config.Providers[endpoint.Provider]
	if !ok {
		return nil, fmt.Errorf("unknown provider: %s", endpoint.Provider)
	}

	req, err := p.buildRequest(ctx, body, endpoint, &providerCfg, false)
	if err != nil {
		return nil, err
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, &ProviderError{Err: err}
	}
	return resp, nil
}

func (p *Proxy) StreamToClient(ctx context.Context, w io.Writer, flusher http.Flusher, body []byte, endpoint ModelEndpoint, timeout time.Duration) error {
	providerCfg, ok := p.config.Providers[endpoint.Provider]
	if !ok {
		return fmt.Errorf("unknown provider: %s", endpoint.Provider)
	}

	// Use the parent context for the request so the connection stays alive for
	// the whole (potentially long) generation. The size-based timeout only
	// guards the time-to-first-token via firstByteReader below.
	req, err := p.buildRequest(ctx, body, endpoint, &providerCfg, true)
	if err != nil {
		return err
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return &ProviderError{Err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return &ProviderError{StatusCode: resp.StatusCode, Body: bodyBytes}
	}

	// Guard only the first byte with the size-based timeout. Once the stream
	// starts producing output we stream the rest under the parent context so a
	// long generation is never cut off mid-stream.
	firstCtx, firstCancel := context.WithTimeout(ctx, timeout)
	defer firstCancel()
	guarded := &firstByteReader{r: resp.Body, firstCtx: firstCtx}

	return p.streamSSE(w, flusher, guarded)
}

// firstByteReader applies firstCtx (the size-based timeout) to only the first
// Read call. After the first byte arrives, subsequent reads use the underlying
// reader directly so the rest of a long stream is not bounded by the timeout.
type firstByteReader struct {
	r         io.Reader
	firstCtx  context.Context
	once      sync.Once
	released  bool
}

func (f *firstByteReader) Read(p []byte) (int, error) {
	if f.released {
		return f.r.Read(p)
	}
	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		n, err := f.r.Read(p)
		ch <- result{n, err}
	}()
	select {
	case <-f.firstCtx.Done():
		return 0, f.firstCtx.Err()
	case res := <-ch:
		f.once.Do(func() { f.released = true })
		return res.n, res.err
	}
}

func (p *Proxy) streamSSE(w io.Writer, flusher http.Flusher, body io.Reader) error {
	var eventBuf bytes.Buffer
	scanner := bufio.NewScanner(body)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	// Buffer events until the first real content token (or [DONE]) arrives.
	// Upstream errors (SSE error events, or non-200 status handled by the
	// caller) almost always occur before any content, so discarding the
	// buffer on error means the client receives nothing and we can fall back
	// to the next model with no duplicated output.
	var buffered bytes.Buffer
	released := false

	flushBuffered := func() error {
		if buffered.Len() == 0 {
			return nil
		}
		if _, err := w.Write(buffered.Bytes()); err != nil {
			return err
		}
		buffered.Reset()
		flusher.Flush()
		return nil
	}

	for scanner.Scan() {
		line := scanner.Bytes()

		if len(line) == 0 {
			if eventBuf.Len() > 0 {
				raw := eventBuf.Bytes()
				if err := ExtractSSEError(raw); err != nil {
					// Early upstream error: discard buffered output so far and
					// signal the caller to fall back.
					return err
				}
				if !released && sseEventIsRelease(raw) {
					released = true
				}
				if released {
					if err := flushBuffered(); err != nil {
						return err
					}
					if _, err := w.Write(raw); err != nil {
						return err
					}
					if _, err := w.Write([]byte("\n\n")); err != nil {
						return err
					}
					flusher.Flush()
				} else {
					buffered.Write(raw)
					buffered.Write([]byte("\n\n"))
				}
				eventBuf.Reset()
			}
			continue
		}

		eventBuf.Write(line)
		eventBuf.WriteByte('\n')
	}

	if err := scanner.Err(); err != nil {
		// If we never released, this is an upstream failure before any content
		// reached the client: discard the buffer and fall back.
		return err
	}

	// Flush any trailing buffered events (e.g. a final [DONE] with no content).
	if err := flushBuffered(); err != nil {
		return err
	}
	return nil
}

// sseEventIsRelease reports whether an SSE event is safe to flush to the
// client: either it is the [DONE] sentinel or it carries the first non-empty
// content delta. Before this point we keep events buffered so an error can be
// swallowed without the client noticing.
func sseEventIsRelease(raw []byte) bool {
	payload := sseDataPayload(raw)
	if bytes.Equal(payload, []byte("[DONE]")) {
		return true
	}
	var obj struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &obj); err != nil {
		// Unparseable: release to avoid stalling (fail open).
		return true
	}
	if len(obj.Choices) > 0 && obj.Choices[0].Delta.Content != "" {
		return true
	}
	return false
}

// sseDataPayload extracts the JSON payload from the first "data:" line of an
// SSE event buffer.
func sseDataPayload(raw []byte) []byte {
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("data:")) {
			return bytes.TrimSpace(line[len("data:"):])
		}
	}
	return bytes.TrimSpace(raw)
}

func extractSSEErrorFromBuffer(buf *bytes.Buffer) error {
	if buf.Len() == 0 {
		return nil
	}
	return ExtractSSEError(buf.Bytes())
}