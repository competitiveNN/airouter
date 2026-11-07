package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestConfigEditorPageServedWithAuth(t *testing.T) {
	cfg := &Config{Models: map[string]ModelConfig{}}
	g := dashboardTestGateway(t, cfg)

	req := httptest.NewRequest(http.MethodGet, "/admin/config/editor?api_key=secret", nil)
	w := httptest.NewRecorder()
	g.HandleAdminConfigEditor(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
	for _, h := range []string{"X-Content-Type-Options", "X-Frame-Options", "Content-Security-Policy"} {
		if w.Header().Get(h) == "" {
			t.Errorf("missing %s", h)
		}
	}
	body := w.Body.String()
	for _, want := range []string{"AI Router Config Editor", "highlightJson", "renderTree", "Load Config", "Save Config"} {
		if !contains(body, want) {
			t.Errorf("config editor page missing %q", want)
		}
	}
}

func TestConfigEditorPageRequiresAuth(t *testing.T) {
	cfg := &Config{Models: map[string]ModelConfig{}}
	g := dashboardTestGateway(t, cfg)

	req := httptest.NewRequest(http.MethodGet, "/admin/config/editor", nil)
	w := httptest.NewRecorder()
	g.HandleAdminConfigEditor(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}
