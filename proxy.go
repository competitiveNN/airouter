package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Proxy struct {
	client            *http.Client
	config            atomic.Pointer[Config]
	toolCalls         atomic.Bool
	streamIdleTimeout time.Duration
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
		streamIdleTimeout: 60 * time.Second,
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

func (p *Proxy) SetStreamIdleTimeout(d time.Duration) {
	if d > 0 {
		p.streamIdleTimeout = d
	}
}

func (p *Proxy) StreamIdleTimeout() time.Duration {
	return p.streamIdleTimeout
}

func (p *Proxy) SetToolCalls(enabled bool) {
	p.toolCalls.Store(enabled)
}

func (p *Proxy) ToolCallsEnabled() bool {
	return p.toolCalls.Load()
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

func (p *Proxy) Warmup(ctx context.Context) {
	cfg := p.config.Load()
	const warmupPrompt = `Answer with ` + "`ready.`" + ` and nothing else.`

	reqBody := map[string]interface{}{
		"messages": []map[string]string{
			{"role": "user", "content": warmupPrompt},
		},
		"max_tokens": 10,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		log.Printf("[warmup] failed to marshal request: %v", err)
		return
	}

	for _, modelName := range LogicalModels {
		chain, ok := cfg.GetChain(modelName)
		if !ok || len(chain) == 0 {
			log.Printf("[warmup] no chain for model %s, skipping", modelName)
			continue
		}

		// Pick endpoints at depth 0, 0.33, and 0.66 of the fallback chain.
		// Use math.Round so depth=0.66 on a 3-element chain gives index 2, not 1.
		depths := []float64{0, 0.33, 0.66}
		type slot struct {
			depth float64
			ep    ModelEndpoint
		}
		var slots []slot
		for _, depth := range depths {
			idx := int(math.Round(float64(len(chain)-1) * depth))
			if idx >= len(chain) {
				idx = len(chain) - 1
			}
			slots = append(slots, slot{depth: depth, ep: chain[idx]})
		}

		// Fire 3 rounds of warmups at each of those depths, sequentially per
		// depth but the three depth-slots run in parallel. This stresses every
		// model in the chain enough that a dead/slow provider shows up as a
		// cooldown before the first user request.
		var wg sync.WaitGroup
		for _, s := range slots {
			wg.Add(1)
			go func(s slot) {
				defer wg.Done()
				for round := 1; round <= 3; round++ {
					if ctx.Err() != nil {
						return
					}
					warmupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
					resp, err := p.Forward(warmupCtx, body, s.ep)
					cancel()
					if err != nil {
						log.Printf("[warmup] %s/%s depth=%.2f round=%d: %v", s.ep.Provider, s.ep.Model, s.depth, round, err)
						continue
					}
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					if resp.StatusCode != 200 {
						log.Printf("[warmup] %s/%s depth=%.2f round=%d: status %d", s.ep.Provider, s.ep.Model, s.depth, round, resp.StatusCode)
						continue
					}
					log.Printf("[warmup] %s/%s depth=%.2f round=%d: ok", s.ep.Provider, s.ep.Model, s.depth, round)
				}
			}(s)
		}
		wg.Wait()
	}
}

func (p *Proxy) StreamToClient(ctx context.Context, w io.Writer, flusher http.Flusher, body []byte, endpoint ModelEndpoint, timeout time.Duration) (string, []streamToolCall, int, error) {
	providerCfg, ok := p.config.Load().Providers[endpoint.Provider]
	if !ok {
		return "", nil, 0, fmt.Errorf("unknown provider: %s", endpoint.Provider)
	}

	// Use the parent context for the request so the connection stays alive for
	// the whole (potentially long) generation. The size-based timeout only
	// guards the time-to-first-token via firstByteReader below.
	req, err := p.buildRequest(ctx, body, endpoint, &providerCfg, true)
	if err != nil {
		return "", nil, 0, err
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return "", nil, 0, &ProviderError{Err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", nil, 0, &ProviderError{StatusCode: resp.StatusCode, Body: bodyBytes}
	}

	// A 200 without an SSE content type means the provider ignored stream:true
	// and returned a plain JSON body. Forwarding that as SSE would garble the
	// client's stream (abrupt / malformed termination), so treat it as a failure
	// and fall back to the next model in the chain.
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return "", nil, 0, &ProviderError{StatusCode: resp.StatusCode, Body: bodyBytes}
	}

	// Guard only the first byte with the size-based timeout. Once the stream
	// starts producing output we stream the rest under the parent context so a
	// long generation is never cut off mid-stream. The idleTimeoutReader on top
	// catches upstream stalls that neither emit an SSE error nor close the
	// connection: if no chunk arrives within the configured stream idle timeout
	// we treat it as a failure and fall back to the next model instead of
	// hanging the client indefinitely.
	firstCtx, firstCancel := context.WithTimeout(ctx, timeout)
	defer firstCancel()
	guarded := &firstByteReader{r: resp.Body, firstCtx: firstCtx}
	idle := newIdleTimeoutReader(guarded, p.streamIdleTimeout)
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
		// The goroutine spawned above may still be blocked in f.r.Read. Close the
		// underlying reader so that Read returns and the goroutine can exit,
		// avoiding a leak. The deferred idle.Close()/resp.Body.Close() will close
		// it again (idempotent for a net/http body).
		//
		// SAFETY: f.r is always a *http.Response.Body (set in StreamToClient),
		// which implements io.Closer. If a future change wraps f.r in a
		// non-closer reader, this type assertion will fail and the goroutine
		// will leak until the connection is closed by some other means. Always
		// ensure f.r implements io.Closer.
		if c, ok := f.r.(io.Closer); ok {
			c.Close()
		}
		return 0, f.firstCtx.Err()
	case res := <-ch:
		// Only release the first-byte timeout once we actually received data.
		// An io.Reader may return (0, nil) which doesn't count as "first byte".
		if res.n > 0 || res.err != nil {
			f.once.Do(func() { f.released = true })
		}
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
// model instead of hanging the client indefinitely. The actual value is
// configurable via Proxy.streamIdleTimeout (default 60s).
const defaultStreamIdleTimeout = 60 * time.Second

// idleTimeoutReader wraps an io.Reader and returns an error if no data is read
// for longer than timeout. It is used to detect mid-stream stalls that produce
// neither an SSE error event nor a connection close. On idle expiry it closes
// the underlying reader (when it implements io.Closer) to unblock any in-flight
// Read so the error can propagate and trigger a fallback.
//
// SAFETY: A dedicated watchdog goroutine waits on the timer channel. On each
// successful Read we send a reset signal; the watchdog drains the timer channel
// and resets it. This avoids the classic timer.Reset-after-fire race: with
// time.AfterFunc there is no channel to drain, so Reset on a fired timer is
// undefined. Using time.NewTimer + an explicit drain in the watchdog goroutine
// makes the reset safe. The goroutine exits when the timer fires (timeout) or
// when Close() is called (stream done).
type idleTimeoutReader struct {
	r       io.Reader
	timeout time.Duration
	mu      sync.Mutex
	timer   *time.Timer
	closed  bool
	resetCh chan struct{}
}

func newIdleTimeoutReader(r io.Reader, timeout time.Duration) *idleTimeoutReader {
	return &idleTimeoutReader{
		r:       r,
		timeout: timeout,
		resetCh: make(chan struct{}, 1),
	}
}

func (i *idleTimeoutReader) Read(p []byte) (int, error) {
	i.mu.Lock()
	if i.closed {
		i.mu.Unlock()
		return 0, io.EOF
	}
	if i.timer == nil {
		i.timer = time.NewTimer(i.timeout)
		go i.watchdog()
	} else {
		// Notify the watchdog that data arrived so it resets the timer.
		// Non-blocking send: if the buffer is full, a reset is already pending.
		select {
		case i.resetCh <- struct{}{}:
		default:
		}
	}
	i.mu.Unlock()

	n, err := i.r.Read(p)

	i.mu.Lock()
	if err != nil {
		// Read failed: stop the timer so the watchdog can exit promptly.
		if i.timer != nil {
			i.timer.Stop()
		}
	}
	i.mu.Unlock()
	return n, err
}

// watchdog blocks on the timer channel. On timeout it closes the reader
// (triggering a fallback). On reset signal it drains the timer channel and
// resets for another cycle. Exits on timeout or when Close() sets i.closed.
func (i *idleTimeoutReader) watchdog() {
	for {
		select {
		case <-i.timer.C:
			i.onIdle()
			return
		case <-i.resetCh:
			i.mu.Lock()
			if i.closed {
				i.mu.Unlock()
				return
			}
			// Drain the timer channel before resetting. If the timer fired
			// between our select cases, the channel has a value we must consume
			// before Reset, otherwise the next cycle could fire immediately.
			select {
			case <-i.timer.C:
			default:
			}
			i.timer.Reset(i.timeout)
			i.mu.Unlock()
		}
	}
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
// It also signals the watchdog goroutine to exit.
func (i *idleTimeoutReader) Close() error {
	i.mu.Lock()
	if i.closed {
		i.mu.Unlock()
		return nil
	}
	i.closed = true
	if i.timer != nil {
		i.timer.Stop()
	}
	i.mu.Unlock()

	// Wake the watchdog so it sees i.closed == true and exits.
	select {
	case i.resetCh <- struct{}{}:
	default:
	}

	if c, ok := i.r.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

func (p *Proxy) streamSSE(w io.Writer, flusher http.Flusher, body io.Reader) (string, []streamToolCall, int, error) {
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
	sawDone := false
	completionTokens := 0

	// When toolCalls is disabled, forward all events verbatim without any
	// coalescing. This preserves compatibility with clients that handle raw
	// tool-call deltas themselves.
	toolCalls := p.toolCalls.Load()

	// Capture the model name from the stream so synthesized tool call events
	// can mirror it back to the client.
	streamModel := ""

	// Per-index tool call accumulator. Providers stream tool calls as multiple
	// deltas (name, then arguments across chunks). We suppress tool-only delta
	// events from the client and only emit the complete tool call once at
	// [DONE] time. Half-formed or truncated tool calls produce artifacts like
	// `maki_unknown_tool` on the client side; coalescing them atomically
	// avoids that entirely.
	emittedTC := map[int]bool{}
	pendingToolCalls := map[int]*pendingTC{}

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

	// synthesizeToolCallEvent builds a single SSE event that represents a
	// complete tool call, ready to be flushed to the client. It mirrors the
	// shape of the upstream deltas so client-side parsers see the same
	// schema, just with the full payload already present. The choice carries
	// finish_reason: "tool_calls" so the client parser recognizes this as a
	// terminal tool-call response (without it the client keeps waiting for
	// more deltas and eventually drops the stream).
	synthesizeToolCallEvent := func(index int, tc *pendingTC, model string) []byte {
		delta := map[string]any{
			"role": "assistant",
			"content": nil,
			"tool_calls": []map[string]any{{
				"index": index,
				"id":    tc.id,
				"type":  "function",
				"function": map[string]any{
					"name":      tc.name,
					"arguments": tc.args,
				},
			}},
		}
		choice := map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": "tool_calls",
		}
		payload, err := json.Marshal(map[string]any{
			"id":      "airouter-tc-" + strconv.Itoa(index),
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []map[string]any{choice},
		})
		if err != nil {
			// Should never happen with a literal map; fall back to the
			// upstream raw bytes the caller already had buffered.
			return nil
		}
		return []byte("data: " + string(payload) + "\n\n")
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
		// [DONE] is intercepted below (when toolCalls is enabled) so that
		// synthesized tool calls can be emitted before it. When toolCalls
		// is disabled, [DONE] flows through normally and we record that we
		// saw it so the post-loop code doesn't emit a duplicate.
		if bytes.Equal(sseDataPayload(raw), []byte("[DONE]")) {
			if !toolCalls {
				sawDone = true
			}
		}
		if !released && sseEventIsRelease(raw) {
			released = true
		}
		// Capture completion token usage from SSE usage events emitted before
		// [DONE] so we can report TPS for streaming requests.
		if ct := extractStreamUsage(raw); ct > 0 {
			completionTokens = ct
		}
		// Capture the model name from the first chunk that carries it, so
		// synthesized tool call events can mirror it back to the client.
		if streamModel == "" {
			var mv struct {
				Model string `json:"model"`
			}
			if err := json.Unmarshal(sseDataPayload(raw), &mv); err == nil && mv.Model != "" {
				streamModel = mv.Model
			}
		}
		if released {
			isDone := bytes.Equal(sseDataPayload(raw), []byte("[DONE]"))
			if toolCalls {
				// Fold any tool call deltas from this event into our per-index
				// accumulators. Tool-only delta events are suppressed from the
				// client; any event that also carries content (or usage) is
				// forwarded verbatim. We emit synthesized complete tool call
				// events only at [DONE] time, when the final accumulated
				// arguments are known. This guarantees the client only ever
				// receives complete, well-formed tool call events (no partial
				// deltas, no duplicates).
				mergeToolCallDeltas(raw, pendingToolCalls)
				if isDone {
					// Don't forward [DONE] yet. The post-loop code will
					// emit synthesized tool calls first, then emit [DONE]
					// after them. Clients finalize parsing at [DONE], so a
					// tool call arriving after it would be silently
					// ignored.
					return nil
				}
				if !sseEventIsToolDeltaOnly(raw) {
					accumulateDelta(&acc, &tcs, raw)
					// If this content-bearing event also carries tool call
					// deltas, strip them before forwarding. The client would
					// otherwise see a partial delta (e.g. empty arguments)
					// followed by the synthesized complete event — which
					// produces duplicate/confusing tool call output. The
					// accumulated deltas will be synthesized at [DONE] time.
					forwarded := stripToolCallDeltas(raw)
					// forwarded already ends with '\n' (from eventBuf.WriteByte('\n')),
					// so add just one more '\n' to form the SSE blank-line delimiter.
					if _, err := w.Write(forwarded); err != nil {
						return err
					}
					if _, err := w.Write([]byte("\n")); err != nil {
						return err
					}
					flusher.Flush()
				} else {
					accumulateDelta(&acc, &tcs, raw)
				}
			} else {
				// Tool calls disabled: forward all events verbatim to the client.
				accumulateDelta(&acc, &tcs, raw)
				if _, err := w.Write(raw); err != nil {
					return err
				}
				if _, err := w.Write([]byte("\n")); err != nil {
					return err
				}
				flusher.Flush()
			}
		} else {
			buffered.Write(raw)
			buffered.Write([]byte("\n"))
		}
		return nil
	}

	for scanner.Scan() {
		line := scanner.Bytes()

		if len(line) == 0 {
			if eventBuf.Len() > 0 {
				raw := eventBuf.Bytes()
				if err := processEvent(raw); err != nil {
					return acc.String(), tcs, completionTokens, err
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
		return acc.String(), tcs, completionTokens, err
	}

	// Process any remaining event in the buffer. The upstream may close the
	// connection without a trailing blank line, leaving the last SSE event in
	// eventBuf. Without this, the final event (which could be the only content
	// or a usage payload) would be silently dropped.
	if eventBuf.Len() > 0 {
		raw := eventBuf.Bytes()
		if err := processEvent(raw); err != nil {
			return acc.String(), tcs, completionTokens, err
		}
		eventBuf.Reset()
	}

	// Flush any trailing buffered events (content that arrived before the
	// release point and was held back). [DONE] is never in the buffer when
	// toolCalls is enabled because processEvent intercepts it; for non-tool
	// mode it passes through normally.
	if err := flushBuffered(); err != nil {
		return acc.String(), tcs, completionTokens, err
	}

	// Emit synthesized tool calls BEFORE [DONE] so the client parser still
	// processes them as part of the stream. Arriving after [DONE] they would
	// be silently dropped. Iterate in deterministic index order so the
	// client receives tool calls in the same order the upstream intended.
	if toolCalls {
		var indices []int
		for idx := range pendingToolCalls {
			indices = append(indices, idx)
		}
		sort.Ints(indices)
		for _, idx := range indices {
			ptc := pendingToolCalls[idx]
			if emittedTC[idx] {
				continue
			}
			// Skip incomplete tool calls: a name with no arguments (or no name
			// at all) produces an invalid tool call event. If the upstream was
			// simply slow, the next Read would have given us more data; if it
			// genuinely omitted them, emitting empty args would break the
			// client parser.
			if ptc.name == "" || !ptc.complete {
				continue
			}
			synth := synthesizeToolCallEvent(idx, ptc, streamModel)
			if synth != nil {
				if _, err := w.Write(synth); err != nil {
					return acc.String(), tcs, completionTokens, err
				}
				emittedTC[idx] = true
				flusher.Flush()
			}
		}
	}

	// Emit the [DONE] sentinel. Clients finalize parsing here, after they
	// have received any synthesized tool calls.
	if !sawDone {
		if _, err := w.Write([]byte("data: [DONE]\n\n")); err != nil {
			return acc.String(), tcs, completionTokens, err
		}
		flusher.Flush()
	}
	return acc.String(), tcs, completionTokens, nil
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

// extractStreamUsage returns the completion token count from an SSE event that
// carries an OpenAI-style usage payload. Some providers emit this after [DONE]
// so we can measure TPS for streaming requests.
func extractStreamUsage(raw []byte) int {
	payload := sseDataPayload(raw)
	if bytes.Equal(payload, []byte("[DONE]")) {
		return 0
	}
	var out struct {
		Usage *struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return 0
	}
	if out.Usage != nil {
		return out.Usage.CompletionTokens
	}
	return 0
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

// pendingTC is the in-flight accumulator used by streamSSE to coalesce tool
// call deltas before forwarding them to the client. A pending tool call is
// only emitted to the client once complete=true, which means we have the
// name and have received at least one arguments chunk (even if it's empty
// string — some functions legitimately take no arguments).
type pendingTC struct {
	id, name, args string
	argsReceived   bool
	complete       bool
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

// mergeToolCallDeltas parses tool call deltas from an SSE event and merges
// them into the per-index pendingToolCalls map. Returns the number of
// individual tool-call deltas processed (one per tool call entry in the
// event, not per event) and a slice of indices that were newly marked
// complete (i.e. they were not complete before this call). The caller
// decides when to forward accumulated events to the client.
func mergeToolCallDeltas(raw []byte, pending map[int]*pendingTC) (int, []int) {
	payload := sseDataPayload(raw)
	if bytes.Equal(payload, []byte("[DONE]")) {
		return 0, nil
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
		return 0, nil
	}
	count := 0
	var newlyCompleted []int
	for _, tc := range obj.Choices {
		for _, d := range tc.Delta.ToolCalls {
			count++
			ptc, ok := pending[d.Index]
			if !ok {
				ptc = &pendingTC{}
				pending[d.Index] = ptc
			}
			wasComplete := ptc.complete
			if d.ID != "" {
				ptc.id = d.ID
			}
			if d.Function.Name != "" {
				ptc.name = d.Function.Name
			}
			// Any delta with a "function" field counts as an arguments chunk,
			// even if Arguments is "". This lets no-arg tool calls complete.
			if d.Function.Name != "" || d.Function.Arguments != "" {
				ptc.argsReceived = true
			}
			ptc.args += d.Function.Arguments
			// A tool call is complete once we have the name and have received
			// at least one arguments chunk (which may be empty).
			if ptc.name != "" && ptc.argsReceived {
				ptc.complete = true
			}
			if ptc.complete && !wasComplete {
				newlyCompleted = append(newlyCompleted, d.Index)
			}
		}
	}
	return count, newlyCompleted
}

// sseEventIsToolDeltaOnly returns true when the SSE event carries only a
// tool_call delta and no content, usage, or other fields. These events are
// buffered in-process until the tool call is complete and should not be
// forwarded to the client as-is (the caller in processEvent does that).
func sseEventIsToolDeltaOnly(raw []byte) bool {
	payload := sseDataPayload(raw)
	if bytes.Equal(payload, []byte("[DONE]")) {
		return false
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
		Usage *struct{} `json:"usage"`
	}
	if err := json.Unmarshal(payload, &obj); err != nil {
		return false
	}
	if len(obj.Choices) == 0 {
		return false
	}
	delta := obj.Choices[0].Delta
	if delta.Content != "" {
		return false
	}
	if len(delta.ToolCalls) > 0 {
		return true
	}
	return false
}

// stripToolCallDeltas removes tool_call deltas from an SSE event's JSON
// payload. This is used when a content-bearing event also carries tool call
// deltas: we forward the content to the client but suppress the partial tool
// call deltas, which will be synthesized as a complete event at [DONE] time.
// Returns the original raw bytes unchanged if there are no tool call deltas
// or if parsing fails (fail-open so we never drop a valid event).
func stripToolCallDeltas(raw []byte) []byte {
	payload := sseDataPayload(raw)
	var obj struct {
		Choices []struct {
			Delta *struct {
				ToolCalls []json.RawMessage `json:"tool_calls"`
			} `json:"delta,omitempty"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &obj); err != nil {
		return raw
	}
	hasToolCalls := false
	for _, ch := range obj.Choices {
		if ch.Delta != nil && len(ch.Delta.ToolCalls) > 0 {
			hasToolCalls = true
			break
		}
	}
	if !hasToolCalls {
		return raw
	}
	// Remove tool_calls from each choice's delta.
	var out map[string]json.RawMessage
	if err := json.Unmarshal(payload, &out); err != nil {
		return raw
	}
	var choices []json.RawMessage
	if err := json.Unmarshal(out["choices"], &choices); err != nil {
		return raw
	}
	for i, ch := range choices {
		var chObj map[string]json.RawMessage
		if err := json.Unmarshal(ch, &chObj); err != nil {
			continue
		}
		if delta, ok := chObj["delta"]; ok {
			var deltaObj map[string]json.RawMessage
			if err := json.Unmarshal(delta, &deltaObj); err != nil {
				continue
			}
			delete(deltaObj, "tool_calls")
			newDelta, err := json.Marshal(deltaObj)
			if err != nil {
				continue
			}
			chObj["delta"] = newDelta
		}
		newCh, err := json.Marshal(chObj)
		if err != nil {
			continue
		}
		choices[i] = newCh
	}
	newChoices, err := json.Marshal(choices)
	if err != nil {
		return raw
	}
	out["choices"] = newChoices
	result, err := json.Marshal(out)
	if err != nil {
		return raw
	}
	// Reconstruct the SSE event with the modified payload.
	// raw may contain multiple lines (e.g. "data: ..." plus event/id lines).
	var lines [][]byte
	for _, line := range bytes.Split(raw, []byte("\n")) {
		lines = append(lines, line)
	}
	for i, line := range lines {
		trimmed := bytes.TrimSpace(line)
		if bytes.HasPrefix(trimmed, []byte("data:")) {
			// Replace the data line with the stripped payload.
			prefix := line[:len(line)-len(trimmed)]
			lines[i] = append(prefix, "data: "+string(result)...)
			break
		}
	}
	return bytes.Join(lines, []byte("\n"))
}
