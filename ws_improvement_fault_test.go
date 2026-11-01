package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func wsFaultGateway(t *testing.T, cfg *Config) (*GatewayContext, *httptest.Server) {
	t.Helper()
	gw := faultGateway(t, cfg)
	srv := httptest.NewServer(http.HandlerFunc(gw.HandleResponsesWebSocket))
	t.Cleanup(srv.Close)
	return gw, srv
}

func wsDial(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestImprovementWebSocketFaultInjectionNo500(t *testing.T) {
	t.Run("midstream-reset-failover", func(t *testing.T) {
		first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"part \"},\"finish_reason\":null}]}\n\n"))
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			conn, ok := w.(http.Hijacker)
			if ok {
				c, _, _ := conn.Hijack()
				c.Close()
			}
		}))
		defer first.Close()

		second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"id\":\"2\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":null}]}\n\n"))
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}))
		defer second.Close()

		cfg := &Config{
			Providers: map[string]ProviderConfig{
				"first":  {URL: first.URL},
				"second": {URL: second.URL},
			},
			Models: map[string]ModelConfig{
				"smart": {Chain: []ModelEndpoint{
					{Provider: "first", Model: "m-first"},
					{Provider: "second", Model: "m-second"},
				}},
			},
		}
		_, srv := wsFaultGateway(t, cfg)
		conn := wsDial(t, srv)

		msg, _ := json.Marshal(map[string]any{
			"id":   "turn-1",
			"type": "response.create",
			"response": map[string]any{
				"model": "smart",
				"input": "hi",
			},
		})
		if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			t.Fatalf("write: %v", err)
		}

		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		var sawDone bool
		var sawError bool
		for i := 0; i < 20; i++ {
			_, data, err := conn.ReadMessage()
			if err != nil {
				break
			}
			var frame map[string]any
			if err := json.Unmarshal(data, &frame); err != nil {
				continue
			}
			if ev, _ := frame["event"].(string); ev == "error" {
				sawError = true
			}
			if done, _ := frame["done"].(bool); done {
				sawDone = true
				break
			}
		}
		if sawError {
			t.Fatalf("websocket turn reported an error")
		}
		if !sawDone {
			t.Fatalf("websocket turn never completed")
		}
	})

	t.Run("all-dead", func(t *testing.T) {
		cfg := &Config{
			Providers: map[string]ProviderConfig{
				"dead": {URL: releasedPortUrl(t)},
			},
			Models: map[string]ModelConfig{
				"smart": {Chain: []ModelEndpoint{
					{Provider: "dead", Model: "m-dead"},
				}},
			},
		}
		_, srv := wsFaultGateway(t, cfg)
		conn := wsDial(t, srv)

		msg, _ := json.Marshal(map[string]any{
			"id":   "turn-2",
			"type": "response.create",
			"response": map[string]any{
				"model": "smart",
				"input": "hi",
			},
		})
		if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			t.Fatalf("write: %v", err)
		}

		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		var sawError bool
		var sawDone bool
		for i := 0; i < 20; i++ {
			_, data, err := conn.ReadMessage()
			if err != nil {
				break
			}
			var frame map[string]any
			if err := json.Unmarshal(data, &frame); err != nil {
				continue
			}
			if ev, _ := frame["event"].(string); ev == "error" {
				sawError = true
			}
			if done, _ := frame["done"].(bool); done {
				sawDone = true
				break
			}
		}
		if !sawError || !sawDone {
			t.Fatalf("all-dead websocket turn did not report a terminal error")
		}
	})
}
