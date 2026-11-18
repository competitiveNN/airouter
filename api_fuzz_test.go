package main

import (
	"testing"
)

// FuzzBodySessionID fizzes arbitrary request bodies through bodySessionID.
func FuzzBodySessionID(f *testing.F) {
	seeds := []string{
		`{"model":"smart","messages":[{"role":"user","content":"hello"}]}`,
		`{"model":"work","messages":[{"role":"user","content":"code"}]}`,
		`{"model":"","messages":[]}`,
		`{}`,
		`invalid json`,
		`{"model":123,"messages":"not an array"}`,
		`{"model":"smart","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"world"}]}`,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, bodyStr string) {
		body := []byte(bodyStr)

		// bodySessionID must never panic
		id1 := bodySessionID(body)

		// Must be deterministic
		id2 := bodySessionID(body)
		if id1 != id2 {
			t.Fatalf("bodySessionID is non-deterministic: %q != %q for input %q", id1, id2, bodyStr)
		}
	})
}
