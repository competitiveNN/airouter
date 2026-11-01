package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestImprovementNoClientVisible500Guard is a CI guard: no handler may write
// or pass through an HTTP 500 to a client. Upstream/transport failures are
// retryable and must surface as 499/502/503 (or a clean SSE terminal), and
// panic recovery uses 503.
func TestImprovementNoClientVisible500Guard(t *testing.T) {
	var files []string
	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			// The only legitimate use of the 500..599 range is classifying
			// an already-received upstream status in chatProxyStatus; a bare
			// comparison is allowed, a write or pass-through is not.
			if strings.Contains(trimmed, "http.StatusInternalServerError") &&
				!strings.Contains(trimmed, ">=") &&
				!strings.Contains(trimmed, "<=") {
				t.Fatalf("%s:%d: forbidden reference to http.StatusInternalServerError: %s", path, i+1, trimmed)
			}
			for _, bad := range []string{
				"WriteHeader(500)",
				"writeAPIError(w, 500",
				"w.WriteHeader(500)",
				`writeAPIError(w, http.StatusInternalServerError`,
			} {
				if strings.Contains(trimmed, bad) {
					t.Fatalf("%s:%d: forbidden client-visible 500: %s", path, i+1, trimmed)
				}
			}
		}
	}
}
