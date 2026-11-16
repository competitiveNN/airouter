package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// WebSocket transport for the Responses surface.
//
// A WebSocket connection carries a sequence of Responses requests and their
// responses over one socket, instead of one HTTP request per turn. That suits
// the Responses model, where a conversation is a sequence of typed items and
// previous_response_id carries state between turns.
//
// Framing: each WebSocket text message is one JSON object of the form
//
//	{"id": "<client correlation id>",
//	 "type": "response.create",
//	 "response": { ...a /v1/responses request body... }}
//
// The server replies on the same socket with one text message per
// Responses event, each carrying the client's `id` so a client multiplexing
// several in-flight turns on one socket can correlate them:
//
//	{"id": "<client correlation id>",
//	 "event": "response.output_text.delta",
//	 "data":  { ...the Responses event payload... }}
//
// and a final message with "done": true when the turn is finished. Errors are
// delivered as an `error` event rather than by closing the socket, so one
// failed turn does not tear down a conversation.
//
// The upstream call still goes through the normal HTTP proxy and fallback
// chain: a WebSocket client gets exactly the same routing, cooldowns and
// model failover as an HTTP client, it just uses a different client transport.
var responsesUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	// The gateway authenticates in checkAuth before the upgrade, so the
	// origin check here is not an authentication boundary. We allow any
	// origin because the gateway is an API server reached by API clients,
	// not a browser-served endpoint with ambient credentials.
	CheckOrigin: func(r *http.Request) bool { return true },
}

// wsTurnTimeout bounds a single turn so one stuck upstream cannot hold the
// socket open indefinitely.
const wsTurnTimeout = 10 * time.Minute

// HandleResponsesWebSocket upgrades a connection to WebSocket and serves
// Responses turns until the peer closes.
func (g *GatewayContext) HandleResponsesWebSocket(w http.ResponseWriter, r *http.Request) {
	if !g.checkAuth(r) {
		writeAPIError(w, 401, "Invalid API key", "authentication_error", "invalid_api_key")
		return
	}

	conn, err := responsesUpgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote an error response.
		log.Printf("[responses-ws] upgrade failed: %v", err)
		return
	}
	defer conn.Close()
	// A generous read deadline covers idle periods between turns; it is reset
	// on every message received. Writes have no deadline because a long
	// generation legitimately writes for a long time.
	_ = conn.SetReadDeadline(time.Now().Add(wsTurnTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(wsTurnTimeout))
	})

	// Start a ping loop so an otherwise idle socket is detected as dead
	// rather than leaking a goroutine and a file descriptor forever.
	stopPing := make(chan struct{})
	defer close(stopPing)
	go wsPingLoop(conn, stopPing)

	for {
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("[responses-ws] read error: %v", err)
			}
			return
		}
		if msgType != websocket.TextMessage {
			// Binary frames are not part of the framing contract.
			g.wsSendError(conn, "", "unsupported frame type", "invalid_request_error", "unsupported_frame")
			continue
		}
		_ = conn.SetReadDeadline(time.Now().Add(wsTurnTimeout))

		var env struct {
			ID       string           `json:"id"`
			Type     string           `json:"type"`
			Response *json.RawMessage `json:"response"`
		}
		if err := json.Unmarshal(data, &env); err != nil {
			g.wsSendError(conn, "", "Invalid JSON", "invalid_request_error", "bad_json")
			continue
		}
		if env.Type != "" && env.Type != "response.create" {
			g.wsSendError(conn, env.ID, "Unsupported message type: "+env.Type, "invalid_request_error", "unsupported_type")
			continue
		}
		if env.Response == nil {
			g.wsSendError(conn, env.ID, "Missing response payload", "invalid_request_error", "missing_response")
			continue
		}

		g.serveResponsesWSTurn(conn, r, env.ID, *env.Response)
	}
}

// serveResponsesWSTurn runs one response.create message to completion,
// emitting every Responses event back on the socket.
func (g *GatewayContext) serveResponsesWSTurn(conn *websocket.Conn, r *http.Request, corrID string, raw json.RawMessage) {
	var req responsesRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		g.wsSendError(conn, corrID, "Invalid JSON", "invalid_request_error", "bad_json")
		return
	}
	if !IsValidModel(req.Model) {
		g.wsSendError(conn, corrID, fmt.Sprintf("Unknown model: %s. Available: %s", req.Model, strings.Join(LogicalModels, ", ")), "invalid_request_error", "model_not_found")
		return
	}

	chatReq, err := toChatCompletionRequest(&req)
	if err != nil {
		g.wsSendError(conn, corrID, "Invalid input items", "invalid_request_error", "bad_input")
		return
	}
	body, err := buildChatBody(chatReq)
	if err != nil {
		g.wsSendError(conn, corrID, "Failed to encode upstream request", "server_error", "encode_failed")
		return
	}

	// Default max_tokens if the client omitted it (same rule as the HTTP
	// surfaces). chatReq.MaxTokens mirrors req.MaxOutputTokens.
	if chatReq.MaxTokens == nil {
		if def := g.defaultMaxTokensForModel(req.Model); def > 0 {
			body = injectMaxTokens(body, def)
		}
	}
	sessionID := bodySessionID(body)

	// A WebSocket turn is always streamed: the socket is a persistent
	// transport and the client expects incremental events.
	turnStart := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), wsTurnTimeout)
	defer cancel()

	// Wall-clock budget for this turn's fallback walk, using the same policy as
	// the HTTP surfaces. wsTurnTimeout (10 minutes) is only a backstop for a
	// genuinely long single generation; without a fallback budget a chain of
	// failing providers holds the socket for all ten minutes, which is far
	// beyond what any of those attempts could justify.
	// The per-attempt timeout the fallback budget is scaled against also
	// bounds each upstream attempt, so a stalled provider cannot hold the
	// socket for longer than the turn's own budget allows.
	timeout := requestAttemptTimeout(estimateTokens(chatReq))
	walk := newBudgetState(turnStart, timeout)
	tokens := estimateTokens(chatReq)

	chainLen := g.router.ChainLength(req.Model)
	maxAttempts := chainLen*3 + 1
	if maxAttempts <= 1 {
		maxAttempts = 1
	}
	tried := map[string]bool{}

	// wsEvents is a flushWriter that ships each Responses event to the client
	// as a single WebSocket text message, tagged with the correlation id.
	wsEvents := &responsesWSEventSink{conn: conn, corrID: corrID}

	log.Printf("[serveResponsesWSTurn] START for corrID=%s model=%s", corrID, req.Model)

	for {
		if ctx.Err() != nil {
			g.wsSendError(conn, corrID, "Request cancelled", "server_error", "cancelled")
			return
		}
		if walk.exhaustedNow() {
			log.Printf("[debug] session=%s model=%s -> ws turn fallback budget exhausted after %d attempts in %v (budget %v)", sessionID, req.Model, walk.attempts, walk.elapsed().Round(time.Millisecond), walk.budget)
			g.wsSendDegraded(conn, corrID)
			return
		}
		if walk.attempts >= maxAttempts {
			g.wsSendDegraded(conn, corrID)
			return
		}
		walk.next()

		ep, wait := g.router.SelectEndpointForTokens(req.Model, sessionID, requestHasVision(body), tried, tokens)
		if ep == nil {
			if wait > 0 {
				sleep, ok := cycleBackSleep(wait, walk, g.router.MaxInterruptionFor(req.Model))
				if !ok {
					g.wsSendDegraded(conn, corrID)
					return
				}
				select {
				case <-ctx.Done():
					g.wsSendError(conn, corrID, "Request cancelled", "server_error", "cancelled")
					return
				case <-time.After(sleep):
				}
				continue
			}
			g.wsSendDegraded(conn, corrID)
			return
		}

		// Buffer this attempt: if it fails before producing output, nothing has
		// reached the client and the turn can still fail over cleanly.
		//
		// The provider's own interruption ceiling bounds how much silence this
		// endpoint may put in front of the client. It is the smaller of the
		// ceiling and what the fallback walk has left, so a provider configured
		// for a short window (a local endpoint that answers in milliseconds)
		// does not hold a client open for 30s on its cooldown.
		// Command Code prefill is slower than the size-derived estimate;
		// floor the per-attempt budget at max_interruption (see api.go).
		perAttemptBase := timeout
		ceiling := g.providerMaxInterruption(ep.Provider)
		if tokens >= largeContextTokens && ceiling < timeout {
			ceiling = timeout
		}
		if g.proxy.effectiveProtocol(ep) == protocolCommandCode {
			if ceiling > perAttemptBase {
				perAttemptBase = ceiling
			}
		} else {
			perAttemptBase = minDuration(timeout, ceiling)
		}
		attemptTimeout := walk.attemptTimeout(minDuration(perAttemptBase, attemptCeiling(tokens)))
		if attemptTimeout <= 0 {
			g.wsSendDegraded(conn, corrID)
			return
		}
		var buf strings.Builder
		attempt := newResponsesStreamWriter(&buf, nopFlusher{}, req.Model)
		attempt.SetProgressCallback(walk.markProgress)
		var attemptReasoning strings.Builder
		_, _, err := g.streamResponsesAttempt(ctx, body, *ep, sessionID, attempt, attemptTimeout, walk.markProgress, &attemptReasoning)
		if err != nil {
			tried[ep.Key()] = true
			isSaturated := errors.Is(err, ErrUpstreamSaturated)
			if !isSaturated {
				if g.testCooldown > 0 {
					g.router.ApplyCooldownWithDuration(ep, 0, err.Error(), sessionID, g.testCooldown)
				} else {
					g.router.ApplyCooldownFromErrorForSession(ep, err, sessionID)
				}
				g.skipSameHostOnTransportFailure(ep, err, tried)
			}
			// ApplyCooldownFromErrorForSession already advances the circuit
			// breaker state machine; the redundant RecordFailure call would
			// double-count this failure and trip the circuit at half the
			// configured threshold.
			continue
		}

		g.router.RecordSuccess(ep)
		wsEvents.writeRaw(buf.String())
		wsEvents.done()
		return
	}
}

// responsesWSEventSink ships a buffered Responses SSE payload to a WebSocket
// client. The payload is a sequence of `event:`/`data:` framed SSE events; we
// re-frame each into one JSON message tagged with the client's correlation id
// so a client multiplexing turns on one socket can tell them apart.
type responsesWSEventSink struct {
	conn   *websocket.Conn
	corrID string
}

// writeRaw re-frames an SSE blob into individual WebSocket messages.
func (s *responsesWSEventSink) writeRaw(sse string) {
	for _, block := range strings.Split(sse, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var event string
		var data string
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event:"):
				event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
		if data == "" {
			continue
		}
		// The data payload is already a JSON object carrying its own `type`;
		// forward it verbatim as a raw message so the client sees the exact
		// Responses event the HTTP surface would deliver.
		s.send(event, data)
	}
}

// send writes one event frame.
func (s *responsesWSEventSink) send(event, data string) {
	msg := map[string]string{
		"id":    s.corrID,
		"event": event,
		"data":  data,
	}
	blob, err := json.Marshal(msg)
	if err != nil {
		return
	}
	_ = s.conn.WriteMessage(websocket.TextMessage, blob)
}

// done marks the turn complete.
func (s *responsesWSEventSink) done() {
	blob, _ := json.Marshal(map[string]any{"id": s.corrID, "done": true})
	_ = s.conn.WriteMessage(websocket.TextMessage, blob)
}

// wsSendError reports a turn-level error without closing the socket, so the
// client can retry or continue the conversation on the same connection.
func (g *GatewayContext) wsSendError(conn *websocket.Conn, corrID, message, errType, code string) {
	blob, _ := json.Marshal(map[string]any{
		"id":    corrID,
		"event": evError,
		"data": respError{
			Code:    code,
			Message: message,
			Type:    errType,
		},
		"done": true,
	})
	_ = conn.WriteMessage(websocket.TextMessage, blob)
}

// wsSendDegraded sends a degraded response over WebSocket when the fallback
// chain is exhausted. It emits a text delta with the degraded message followed
// by a completed event, mirroring the degraded SSE path on the HTTP surface.
func (g *GatewayContext) wsSendDegraded(conn *websocket.Conn, corrID string) {
	log.Printf("[wsSendDegraded] START for corrID=%s", corrID)
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[wsSendDegraded] PANIC: %v", rec)
		}
		log.Printf("[wsSendDegraded] END for corrID=%s", corrID)
	}()
	message := ""
	msgID := newCorrelationID("msg")

	// Send text delta
	// Send text delta
	deltaBlob, _ := json.Marshal(map[string]any{
		"id":    corrID,
		"event": "response.output_text.delta",
		"data": map[string]any{
			"item_id": msgID,
			"delta":   message,
		},
		"done": false,
	})
	if err := conn.WriteMessage(websocket.TextMessage, deltaBlob); err != nil {
		log.Printf("[wsSendDegraded] write delta error: %v", err)
		return
	}

	// Send text done
	doneBlob, _ := json.Marshal(map[string]any{
		"id":    corrID,
		"event": "response.output_text.done",
		"data": map[string]any{
			"item_id": msgID,
			"text":    message,
		},
		"done": false,
	})
	if err := conn.WriteMessage(websocket.TextMessage, doneBlob); err != nil {
		log.Printf("[wsSendDegraded] write done error: %v", err)
		return
	}

	// Send content part done
	partDoneBlob, _ := json.Marshal(map[string]any{
		"id":    corrID,
		"event": "response.content_part.done",
		"data": map[string]any{
			"item_id":    msgID,
			"part_index": 0,
			"part": map[string]any{
				"type": "output_text",
				"text": message,
			},
		},
		"done": false,
	})
	if err := conn.WriteMessage(websocket.TextMessage, partDoneBlob); err != nil {
		log.Printf("[wsSendDegraded] write partDone error: %v", err)
		return
	}

	// Send output item done
	itemDoneBlob, _ := json.Marshal(map[string]any{
		"id":    corrID,
		"event": "response.output_item.done",
		"data": map[string]any{
			"output_index": 0,
			"item": map[string]any{
				"type":    "message",
				"id":      msgID,
				"role":    "assistant",
				"content": []map[string]any{{"type": "output_text", "text": message}},
			},
		},
		"done": false,
	})
	if err := conn.WriteMessage(websocket.TextMessage, itemDoneBlob); err != nil {
		log.Printf("[wsSendDegraded] write itemDone error: %v", err)
		return
	}

	// Send completed event
	completedBlob, _ := json.Marshal(map[string]any{
		"id":    corrID,
		"event": "response.completed",
		"data": map[string]any{
			"response": map[string]any{
				"id":     newCorrelationID("resp"),
				"status": "completed",
				"output": []map[string]any{{"type": "message", "id": msgID, "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": message}}}},
				"usage":  nil,
			},
		},
		"done": true,
	})
	if err := conn.WriteMessage(websocket.TextMessage, completedBlob); err != nil {
		log.Printf("[wsSendDegraded] write completed error: %v", err)
		return
	}
}

// wsPingLoop sends periodic pings so a dead peer is detected and the
// connection's resources are released.
func wsPingLoop(conn *websocket.Conn, stop <-chan struct{}) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second))
		}
	}
}
