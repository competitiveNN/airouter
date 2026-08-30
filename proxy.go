package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Proxy struct {
	client *http.Client
	config atomic.Pointer[Config]
}

func NewProxy(cfg *Config) *Proxy {
	p := &Proxy{
		client: &http.Client{
			Timeout: 0,
			Transport: &http.Transport{
				MaxIdleConns:        200,
				MaxIdleConnsPerHost: 50,
				IdleConnTimeout:     90 * time.Second,
				// Bound the connection-setup phases so a fallback request to a
				// flaky provider cannot hang forever inside http.Client.Do.
				// The response body (the actual generation) is guarded
				// separately by firstByteReader / idleTimeoutReader.
				DialContext: (&net.Dialer{
					Timeout:   15 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				TLSHandshakeTimeout:   15 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
			},
		},
	}
	p.config.Store(cfg)
	return p
}

// geminiStripFields lists request fields that Gemini's OpenAI-compatible
// endpoint rejects (it surfaces them as 400 "Unknown name" proto errors).
// The router otherwise forwards the raw client body, so these are removed
// before sending upstream. They are stripped recursively at any nesting
// depth, because clients may place them inside nested objects/arrays. Applies
// to all providers whose name starts with "gemini" (gemini, gemini2, ...).
var geminiStripFields = []string{"thinking", "thinking_budget", "reasoning", "reasoning_effort"}

// sanitizeRequestBody removes provider-specific unsupported fields from the
// request JSON, recursing into nested objects and arrays. It returns the
// original body unchanged if nothing applies or on a parse failure (fail-open
// so we never drop a valid request).
func sanitizeRequestBody(body []byte, provider string) []byte {
	var fields []string
	if strings.HasPrefix(provider, "gemini") {
		fields = geminiStripFields
	}
	if len(fields) == 0 {
		return body
	}
	fieldSet := make(map[string]bool, len(fields))
	for _, f := range fields {
		fieldSet[f] = true
	}
	out, changed := stripJSONFields(body, fieldSet)
	if !changed {
		return body
	}
	log.Printf("[debug] provider=%s -> stripped unsupported fields %v", provider, fields)
	return out
}

// stripJSONFields recursively walks a JSON document and removes any object
// keys present in fields. It returns the (possibly re-encoded) document and
// whether anything was removed.
func stripJSONFields(v json.RawMessage, fields map[string]bool) (json.RawMessage, bool) {
	v = bytes.TrimSpace(v)
	if len(v) == 0 {
		return v, false
	}
	switch v[0] {
	case '{':
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(v, &obj); err != nil {
			return v, false
		}
		changed := false
		for f := range fields {
			if _, ok := obj[f]; ok {
				delete(obj, f)
				changed = true
			}
		}
		for k, val := range obj {
			newVal, c := stripJSONFields(val, fields)
			if c {
				obj[k] = newVal
				changed = true
			}
		}
		if !changed {
			return v, false
		}
		out, err := json.Marshal(obj)
		if err != nil {
			return v, false
		}
		return out, true
	case '[':
		var arr []json.RawMessage
		if err := json.Unmarshal(v, &arr); err != nil {
			return v, false
		}
		changed := false
		for i, val := range arr {
			newVal, c := stripJSONFields(val, fields)
			if c {
				arr[i] = newVal
				changed = true
			}
		}
		if !changed {
			return v, false
		}
		out, err := json.Marshal(arr)
		if err != nil {
			return v, false
		}
		return out, true
	}
	return v, false
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
	providerCfg, ok := p.config.Load().Providers[endpoint.Provider]
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

func (p *Proxy) StreamToClient(ctx context.Context, w io.Writer, flusher http.Flusher, body []byte, endpoint ModelEndpoint, timeout time.Duration) (string, []streamToolCall, error) {
	providerCfg, ok := p.config.Load().Providers[endpoint.Provider]
	if !ok {
		return "", nil, fmt.Errorf("unknown provider: %s", endpoint.Provider)
	}

	// Use the parent context for the request so the connection stays alive for
	// the whole (potentially long) generation. The size-based timeout only
	// guards the time-to-first-token via firstByteReader below.
	req, err := p.buildRequest(ctx, body, endpoint, &providerCfg, true)
	if err != nil {
		return "", nil, err
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return "", nil, &ProviderError{Err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", nil, &ProviderError{StatusCode: resp.StatusCode, Body: bodyBytes}
	}

	// Guard only the first byte with the size-based timeout. Once the stream
	// starts producing output we stream the rest under the parent context so a
	// long generation is never cut off mid-stream. The idleTimeoutReader on top
	// catches upstream stalls that neither emit an SSE error nor close the
	// connection: if no chunk arrives within streamIdleTimeout we treat it as a
	// failure and fall back to the next model instead of hanging the client.
	firstCtx, firstCancel := context.WithTimeout(ctx, timeout)
	defer firstCancel()
	guarded := &firstByteReader{r: resp.Body, firstCtx: firstCtx}
	idle := &idleTimeoutReader{r: guarded, timeout: streamIdleTimeout}
	defer idle.Close()

	return p.streamSSE(w, flusher, idle)
}

// firstByteReader applies firstCtx (the size-based timeout) to only the first
// Read call. After the first byte arrives, subsequent reads use the underlying
// reader directly so the rest of a long stream is not bounded by the timeout.
type firstByteReader struct {
	r        io.Reader
	firstCtx context.Context
	once     sync.Once
	released bool
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
		// The goroutine spawned below may still be blocked in f.r.Read. Close the
		// underlying reader so that Read returns and the goroutine can exit,
		// avoiding a leak. The deferred idle.Close()/resp.Body.Close() will close
		// it again (idempotent for a net/http body).
		if c, ok := f.r.(io.Closer); ok {
			c.Close()
		}
		return 0, f.firstCtx.Err()
	case res := <-ch:
		f.once.Do(func() { f.released = true })
		return res.n, res.err
	}
}

// Close closes the underlying reader so an idle watchdog (idleTimeoutReader) can
// unblock a Read that is blocked inside the firstByteReader goroutine.
func (f *firstByteReader) Close() error {
	if c, ok := f.r.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// streamIdleTimeout bounds the time the router waits for the next chunk from an
// upstream model once a stream has begun. The first chunk is additionally
// bounded by the size-scaled timeout via firstByteReader. If an upstream stalls
// (rate limit, dropped connection that never returns EOF, etc.) without
// emitting an SSE error event, we detect it here and fall back to the next
// model instead of hanging the client indefinitely.
const streamIdleTimeout = 60 * time.Second

// idleTimeoutReader wraps an io.Reader and returns an error if no data is read
// for longer than timeout. It is used to detect mid-stream stalls that produce
// neither an SSE error event nor a connection close. On idle expiry it closes
// the underlying reader (when it implements io.Closer) to unblock any in-flight
// Read so the error can propagate and trigger a fallback.
type idleTimeoutReader struct {
	r       io.Reader
	timeout time.Duration
	mu      sync.Mutex
	timer   *time.Timer
	closed  bool
}

func (i *idleTimeoutReader) Read(p []byte) (int, error) {
	i.mu.Lock()
	if i.closed {
		i.mu.Unlock()
		return 0, io.EOF
	}
	if i.timer == nil {
		i.timer = time.AfterFunc(i.timeout, i.onIdle)
	} else if i.timer.Stop() {
		// Stopped before firing: safe to reschedule per Go's Timer contract.
		i.timer.Reset(i.timeout)
	}
	// If Stop() returned false the timer already fired (onIdle set i.closed);
	// that is handled by the closed-check at the top of the next Read.
	i.mu.Unlock()

	n, err := i.r.Read(p)

	i.mu.Lock()
	if err == nil {
		if i.timer != nil {
			i.timer.Reset(i.timeout)
		}
	} else {
		if i.timer != nil {
			i.timer.Stop()
			i.timer = nil
		}
	}
	i.mu.Unlock()
	return n, err
}

func (i *idleTimeoutReader) onIdle() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return
	}
	i.closed = true
	if c, ok := i.r.(io.Closer); ok {
		c.Close()
	}
}

// Close stops the idle timer and closes the underlying reader. It is called
// when the stream completes normally so the watchdog can't fire afterwards.
func (i *idleTimeoutReader) Close() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return nil
	}
	i.closed = true
	if i.timer != nil {
		i.timer.Stop()
		i.timer = nil
	}
	if c, ok := i.r.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

func (p *Proxy) streamSSE(w io.Writer, flusher http.Flusher, body io.Reader) (string, []streamToolCall, error) {
	var eventBuf bytes.Buffer
	scanner := bufio.NewScanner(body)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 16*1024*1024)

	// Buffer events until the first real content token, first tool call, or
	// [DONE] arrives. Upstream errors (SSE error events, or non-200 status
	// handled by the caller) almost always occur before any content, so
	// discarding the buffer on error means the client receives nothing and we
	// can fall back to the next model with no duplicated output.
	var buffered bytes.Buffer
	var acc strings.Builder
	tcs := []streamToolCall{}
	released := false

	flushBuffered := func() error {
		if buffered.Len() == 0 {
			return nil
		}
		accumulateDelta(&acc, &tcs, buffered.Bytes())
		if _, err := w.Write(buffered.Bytes()); err != nil {
			return err
		}
		buffered.Reset()
		flusher.Flush()
		return nil
	}

	// processEvent handles one fully-formed SSE event (raw bytes, ending in a
	// blank line). It extracts errors, decides whether the stream is now
	// "released" (safe to flush), and either flushes or buffers the event.
	processEvent := func(raw []byte) error {
		if err := ExtractSSEError(raw); err != nil {
			// Early upstream error: discard buffered output so far and signal the
			// caller to fall back.
			return err
		}
		if !released && sseEventIsRelease(raw) {
			released = true
		}
		if released {
			accumulateDelta(&acc, &tcs, raw)
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
		return nil
	}

	for scanner.Scan() {
		line := scanner.Bytes()

		if len(line) == 0 {
			if eventBuf.Len() > 0 {
				raw := eventBuf.Bytes()
				if err := processEvent(raw); err != nil {
					return acc.String(), tcs, err
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
		return acc.String(), tcs, err
	}

	// Flush a trailing event that was not terminated by a blank line (some
	// providers omit the final blank line before EOF).
	if eventBuf.Len() > 0 {
		raw := eventBuf.Bytes()
		if err := processEvent(raw); err != nil {
			return acc.String(), tcs, err
		}
	}

	// Flush any trailing buffered events (e.g. a final [DONE] with no content).
	if err := flushBuffered(); err != nil {
		return acc.String(), tcs, err
	}
	return acc.String(), tcs, nil
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
				Content   string `json:"content"`
				ToolCalls []struct {
					Index int `json:"index"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &obj); err != nil {
		// Unparseable: release to avoid stalling (fail open).
		return true
	}
	if len(obj.Choices) > 0 {
		if obj.Choices[0].Delta.Content != "" {
			return true
		}
		if len(obj.Choices[0].Delta.ToolCalls) > 0 {
			return true
		}
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

// streamToolCall accumulates a streamed tool_call across multiple SSE deltas so
// it can be replayed as part of an assistant message when resuming after a
// mid-stream failure. Arguments are concatenated across deltas for the same
// index (matching how providers stream them).
type streamToolCall struct {
	Index     int
	ID        string
	Type      string
	Name      string
	Arguments string
}

// accumulateDelta extracts content and tool_call deltas from already-flushed SSE
// events and appends them to acc / tcs, so a mid-stream fallback can resume on
// the next model by replaying what the client already received as an assistant
// message. It safely ignores [DONE] sentinels and unparseable events.
func accumulateDelta(acc *strings.Builder, tcs *[]streamToolCall, raw []byte) {
	for _, ev := range bytes.Split(raw, []byte("\n\n")) {
		if len(bytes.TrimSpace(ev)) == 0 {
			continue
		}
		payload := sseDataPayload(ev)
		if bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		var obj struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(payload, &obj); err != nil {
			continue
		}
		for _, ch := range obj.Choices {
			if ch.Delta.Content != "" {
				acc.WriteString(ch.Delta.Content)
			}
			for _, tc := range ch.Delta.ToolCalls {
				var target *streamToolCall
				for i := range *tcs {
					if (*tcs)[i].Index == tc.Index {
						target = &(*tcs)[i]
						break
					}
				}
				if target == nil {
					*tcs = append(*tcs, streamToolCall{Index: tc.Index})
					target = &(*tcs)[len(*tcs)-1]
				}
				if tc.ID != "" {
					target.ID = tc.ID
				}
				if tc.Type != "" {
					target.Type = tc.Type
				}
				if tc.Function.Name != "" {
					target.Name = tc.Function.Name
				}
				target.Arguments += tc.Function.Arguments
			}
		}
	}
}

func extractSSEErrorFromBuffer(buf *bytes.Buffer) error {
	if buf.Len() == 0 {
		return nil
	}
	return ExtractSSEError(buf.Bytes())
}
