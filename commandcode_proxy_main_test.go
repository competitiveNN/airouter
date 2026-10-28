package main

// End-to-end test for the SECOND binary.
//
// `make commandcode-proxy` compiles this package with the commandcode_proxy
// build tag, which swaps main.go for commandcode_proxy_main.go. Nothing in the
// ordinary test binary covers that artifact: commandcode_api_test.go drives the
// shim's http.Handler in-process, which proves the handler works but not that
// the shipped standalone binary starts a listener, loads a config, or authorises
// a request. This test builds the tagged binary, runs it against a fake Command
// Code upstream, and drives the real socket -- so a regression in the tag wiring
// (a moved helper, a flag that no longer exists, a shim that is never mounted)
// fails the suite instead of shipping a binary nobody started.

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ccProxyBuildTag is the tag that selects commandcode_proxy_main.go.
const ccProxyBuildTag = "commandcode_proxy"

// buildCommandCodeProxy compiles the standalone binary into dir.
func buildCommandCodeProxy(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(dir, "commandcode-proxy")
	cmd := exec.Command("go", "build", "-tags", ccProxyBuildTag, "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build -tags %s failed: %v\n%s", ccProxyBuildTag, err, out)
	}
	if fi, err := os.Stat(bin); err != nil || fi.Size() == 0 {
		t.Fatalf("built binary is missing or empty: %v", err)
	}
	return bin
}

// freePort reserves and releases a port, so the child process can bind it.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// TestCommandCodeProxyBinaryServesItsOwnPort builds and starts the standalone
// binary and checks the surface it is supposed to expose: /health, /v1/models,
// /v1/chat/completions (streaming and not), authentication, and the deliberate
// 404 on /v1/responses.
func TestCommandCodeProxyBinaryServesItsOwnPort(t *testing.T) {
	dir := t.TempDir()
	bin := buildCommandCodeProxy(t, dir)

	upstream := &ccShimUpstream{respond: func(w http.ResponseWriter, r *http.Request, gen int) {
		_, _ = io.WriteString(w, ccShimNDJSON)
	}}
	ups := httptest.NewServer(upstream.handler())
	t.Cleanup(ups.Close)

	// The child process loads this config; the model must be reachable through
	// a Command Code provider, otherwise the shim refuses to start.
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf(`providers:
  commandcode:
    url: %s
    api_key_env: CC_TEST_KEY
models:
  smart:
    chain:
      - provider: commandcode
        model: cc/one
`, ups.URL)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	port := freePort(t)
	cmd := exec.Command(bin,
		"-config", cfgPath,
		"-port", strconv.Itoa(port),
		"-api-key", "sk-proxy-test",
		"-providers", "commandcode",
	)
	cmd.Env = append(os.Environ(), "CC_TEST_KEY=sk-upstream-test")
	var logs strings.Builder
	cmd.Stdout = &logs
	cmd.Stderr = &logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("start standalone proxy: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	// The process is ready when /health answers; anything else is a startup
	// failure, and the captured log is the only diagnostic the child offers.
	deadline := time.Now().Add(15 * time.Second)
	var health *http.Response
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/health")
		if err == nil {
			health = resp
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if health == nil {
		t.Fatalf("standalone proxy never answered /health on %s\n%s", base, logs.String())
	}
	defer health.Body.Close()
	body, _ := io.ReadAll(health.Body)
	if health.StatusCode != http.StatusOK {
		t.Fatalf("/health = %d: %s", health.StatusCode, body)
	}
	if !strings.Contains(string(body), "commandcode") {
		t.Fatalf("/health does not report the provider: %s", body)
	}

	do := func(method, path, payload string, auth bool) (*http.Response, []byte) {
		t.Helper()
		var rd io.Reader
		if payload != "" {
			rd = strings.NewReader(payload)
		}
		req, err := http.NewRequest(method, base+path, rd)
		if err != nil {
			t.Fatalf("build %s %s: %v", method, path, err)
		}
		if payload != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if auth {
			req.Header.Set("Authorization", "Bearer sk-proxy-test")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp, out
	}

	// Models: the chain entry carried by the config, not a hard-coded list.
	resp, out := do(http.MethodGet, "/v1/models", "", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/v1/models = %d: %s", resp.StatusCode, out)
	}
	var listed struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &listed); err != nil {
		t.Fatalf("/v1/models returned invalid JSON: %v: %s", err, out)
	}
	if listed.Object != "list" || len(listed.Data) != 1 || listed.Data[0].ID != "cc/one" {
		t.Fatalf("/v1/models = %s, want the single cc/one entry", out)
	}

	// Authentication is enforced by the binary, not only by the handler.
	resp, out = do(http.MethodGet, "/v1/models", "", false)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /v1/models = %d, want 401: %s", resp.StatusCode, out)
	}

	// A real completion, with the upstream fake proving the request was
	// translated and forwarded rather than answered locally.
	resp, out = do(http.MethodPost, "/v1/chat/completions",
		`{"model":"cc/one","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("non-streaming completion = %d: %s", resp.StatusCode, out)
	}
	if !strings.Contains(string(out), "Hello") {
		t.Fatalf("completion body does not carry the upstream text: %s", out)
	}
	if !strings.Contains(string(out), `"cc/one"`) {
		t.Fatalf("completion does not echo the requested model: %s", out)
	}
	if !upstream.sawGenerate() {
		t.Fatal("the fake upstream never saw /alpha/generate: the binary did not proxy the request")
	}

	// Streaming must work through the same listener.
	resp, out = do(http.MethodPost, "/v1/chat/completions",
		`{"model":"cc/one","stream":true,"messages":[{"role":"user","content":"hi"}],"max_tokens":8}`, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("streaming completion = %d: %s", resp.StatusCode, out)
	}
	if !strings.Contains(string(out), "Hello") || !strings.Contains(string(out), "data: ") {
		t.Fatalf("streaming body is not an SSE stream carrying the upstream text: %s", out)
	}

	// The Responses surface is deliberately unsupported on this binary.
	resp, out = do(http.MethodPost, "/v1/responses", `{"model":"cc/one","input":"hi"}`, true)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/v1/responses = %d, want 404: %s", resp.StatusCode, out)
	}
}
