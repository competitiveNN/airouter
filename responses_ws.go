package main

import (
	"context"
	"encoding/json"
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
	ctx, cancel := context.WithTimeout(r.Context(), wsTurnTimeout)
	defer cancel()

	chainLen := g.router.ChainLength(req.Model)
	maxAttempts := chainLen*3 + 1
	if maxAttempts <= 1 {
		maxAttempts = 1
	}
	attempts := 0
	tried := map[string]bool{}

	// wsEvents is a flushWriter that ships each Responses event to the client
	// as a single WebSocket text message, tagged with the correlation id.
	wsEvents := &responsesWSEventSink{conn: conn, corrID: corrID}

	for {
		if ctx.Err() != nil {
			g.wsSendError(conn, corrID, "Request cancelled", "server_error", "cancelled")
			return
		}
		if attempts >= maxAttempts {
			g.wsSendError(conn, corrID, "All models are currently unavailable", "rate_limit_error", "all_models_unavailable")
			return
		}
		attempts++

		ep, wait := g.router.SelectEndpoint(req.Model, sessionID, requestHasVision(body), tried)
		if ep == nil {
			if wait > 0 {
				select {
				case <-ctx.Done():
					g.wsSendError(conn, corrID, "Request cancelled", "server_error", "cancelled")
					return
				case <-time.After(minDuration(wait, 30*time.Second)):
				}
				continue
			}
			g.wsSendError(conn, corrID, "All models are currently unavailable", "rate_limit_error", "all_models_unavailable")
			return
		}

		// Buffer this attempt: if it fails before producing output, nothing has
		// reached the client and the turn can still fail over cleanly.
		var buf strings.Builder
		attempt := newResponsesStreamWriter(&buf, nopFlusher{}, req.Model)
		_, _, err := g.streamResponsesAttempt(ctx, body, *ep, sessionID, attempt)
		if err != nil {
			tried[ep.Key()] = true
			g.router.ApplyCooldownFromErrorForSession(ep, err, sessionID)
			// ApplyCooldownFromErrorForSession already advances the circuit
			// breaker state machine; the redundant RecordFailure call would
			// double-count this failure and trip the circuit at half the
			// configured threshold.
			if g.testCooldown > 0 {
				g.router.ApplyCooldownWithDuration(ep, 0, err.Error(), sessionID, g.testCooldown)
			}
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
