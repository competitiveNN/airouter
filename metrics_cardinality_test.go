package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The metrics registry is keyed by label values that come from configuration,
// and configuration is not fixed for the process lifetime: HandleAdminConfig
// accepts an arbitrary new config and watchConfig hot-reloads the file on a 3 s
// poll. Nothing evicted a series, and each endpoint cost a counter set plus a
// 12-bucket histogram, so a config churn loop (or a model sync that keeps
// inventing model ids) grew the process until it was OOM-killed. These tests
// pin the bound so that regression cannot ship silently.

// TestMetricsAttemptCardinalityIsBounded pushes far more distinct endpoint
// labels than the cap allows and asserts every backing map stops growing at the
// cap. It checks the maps directly rather than only the rendered output,
// because output-only truncation would still consume the memory.
func TestMetricsAttemptCardinalityIsBounded(t *testing.T) {
	m := NewMetrics()
	const flood = 5000
	for i := 0; i < flood; i++ {
		m.Attempt(fmt.Sprintf("churn:model-%d", i), time.Millisecond, i%2 == 0)
	}

	if got := len(m.attemptsByEndpoint); got > maxLabelValues {
		t.Errorf("attemptsByEndpoint grew to %d, above the cap of %d", got, maxLabelValues)
	}
	if got := len(m.attemptFailuresByEnd); got > maxLabelValues {
		t.Errorf("attemptFailuresByEnd grew to %d, above the cap of %d", got, maxLabelValues)
	}
	if got := len(m.attemptLatencyNSByEnd); got > maxLabelValues {
		t.Errorf("attemptLatencyNSByEnd grew to %d, above the cap of %d", got, maxLabelValues)
	}
	// The histogram is the expensive one (a slice per entry), so it is the
	// reason the cap matters at all.
	if got := len(m.attemptHistByEnd); got > maxLabelValues {
		t.Errorf("attemptHistByEnd grew to %d, above the cap of %d", got, maxLabelValues)
	}

	// Every observation must still be accounted for somewhere. Collapsing is
	// only acceptable if the counts stay truthful, otherwise a churning
	// provider would render as a perfectly healthy endpoint.
	var total int64
	for _, c := range m.attemptsByEndpoint {
		total += c.Load()
	}
	if total != flood {
		t.Errorf("attempt totals do not add up: got %d, want %d — collapsing must not drop observations", total, flood)
	}
	if m.labelsOverflowed.Load() == 0 {
		t.Error("labelsOverflowed is 0 after flooding past the cap; overflow is happening but is invisible")
	}
}

// TestMetricsOverflowKeepsHotEndpointsDistinct verifies the cap does not
// collapse the endpoints that actually matter. An already-tracked value must
// keep its own series even at capacity, so the hot endpoints of a churning
// config stay individually visible and only the tail collapses.
func TestMetricsOverflowKeepsHotEndpointsDistinct(t *testing.T) {
	m := NewMetrics()

	// Fill to exactly the cap with the endpoints we intend to keep visible.
	for i := 0; i < maxLabelValues; i++ {
		m.Attempt(fmt.Sprintf("keep:%d", i), time.Millisecond, true)
	}
	// Flood past it.
	for i := 0; i < 1000; i++ {
		m.Attempt(fmt.Sprintf("churn:%d", i), time.Millisecond, true)
	}
	// Record again on a value that was admitted before the cap was hit.
	m.Attempt("keep:0", time.Millisecond, true)

	if got := m.attemptsByEndpoint["keep:0"]; got == nil || got.Load() != 2 {
		t.Errorf("pre-cap endpoint keep:0 was not kept distinct: got %v, want count 2", got)
	}
	if _, ok := m.attemptsByEndpoint[overflowLabelValue]; !ok {
		t.Error("no overflow bucket was created; the tail is being dropped rather than collapsed")
	}

	body := renderMetricsBody(m).String()
	if !strings.Contains(body, `airouter_endpoint_attempts_total{endpoint="keep:0"} 2`) {
		t.Errorf("keep:0 missing or wrong in output:\n%s", firstLines(body, 40))
	}
	if !strings.Contains(body, fmt.Sprintf(`endpoint="%s"`, overflowLabelValue)) {
		t.Error("overflow bucket missing from /metrics output")
	}
}

// TestMetricsCardinalityBoundedAcrossAllLabelMaps covers the maps that are not
// reached through Attempt: the client-supplied model name and the per-endpoint
// fallback counter. The model label is the more interesting of the two, since
// the model string arrives from the request before validation on several paths.
func TestMetricsCardinalityBoundedAcrossAllLabelMaps(t *testing.T) {
	m := NewMetrics()
	const flood = 5000
	for i := 0; i < flood; i++ {
		m.Request(fmt.Sprintf("attacker-model-%d", i), 200, time.Millisecond)
		m.Fallback(fmt.Sprintf("churn:%d", i))
		m.CircuitTransition(CircuitClosed, CircuitOpen, fmt.Sprintf("churn:%d", i))
	}

	for name, got := range map[string]int{
		"requestsByModel":         len(m.requestsByModel),
		"fallbacksByFromEndpoint": len(m.fallbacksByFromEndpoint),
		"circuitTransitions":      len(m.circuitTransitions),
	} {
		if got > maxLabelValues {
			t.Errorf("%s grew to %d, above the cap of %d", name, got, maxLabelValues)
		}
	}
	if m.requestsTotal.Load() != flood {
		t.Errorf("requestsTotal = %d, want %d — the total must survive label collapsing", m.requestsTotal.Load(), flood)
	}
}

// TestMetricsNormalConfigIsUnderCap guards the other direction: the cap must be
// loose enough that the real deployment is never collapsed. The production
// config currently carries 60 distinct endpoint keys, and a nightly free-model
// sync legitimately grows that list, so the cap needs real headroom. If this
// fails, the cap is too tight and is silently destroying per-endpoint
// observability in production.
func TestMetricsNormalConfigIsUnderCap(t *testing.T) {
	if maxLabelValues < 256 {
		t.Errorf("maxLabelValues = %d, too tight: the live config already has 60 endpoint keys and the nightly model sync adds more", maxLabelValues)
	}
}

// TestMetricsLabelOverflowCounterIsExported asserts the overflow condition is
// visible on the endpoint. Without it, collapsing would be silent, which is the
// one outcome these series must never have.
func TestMetricsLabelOverflowCounterIsExported(t *testing.T) {
	m := NewMetrics()
	for i := 0; i < maxLabelValues+10; i++ {
		m.Attempt(fmt.Sprintf("ep:%d", i), time.Millisecond, true)
	}
	body := renderMetricsBody(m).String()
	if !strings.Contains(body, "airouter_metrics_label_overflow_total") {
		t.Errorf("overflow counter is not exported:\n%s", firstLines(body, 20))
	}
	if m.labelsOverflowed.Load() == 0 {
		t.Error("overflow counter is 0 despite flooding past the cap")
	}
}

// TestMetricsModelLabelIsTheBareModelName pins the label *value* format. The
// store used to be keyed by an internal composite ("requests_by_model_" +
// model) that was then interpolated straight into the label, so every series
// read model="requests_by_model_smart". The key and the label are now separate
// concerns and the label must carry only the model name, or every dashboard
// query and alert has to know about a storage detail.
func TestMetricsModelLabelIsTheBareModelName(t *testing.T) {
	m := NewMetrics()
	m.Request("smart", 200, time.Millisecond)
	m.Request("work", 502, time.Millisecond)

	body := renderMetricsBody(m).String()
	for _, want := range []string{
		`airouter_requests_total{model="smart"} 1`,
		`airouter_requests_total{model="work"} 1`,
		`airouter_failures_total{status="502"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in /metrics output:\n%s", want, body)
		}
	}
	// The composite key must not leak into any label.
	for _, leak := range []string{
		`model="requests_by_model_`,
		`status="requests_by_status_`,
		`status="failures_by_status_`,
	} {
		if strings.Contains(body, leak) {
			t.Errorf("internal store key leaked into a metric label (%q):\n%s", leak, body)
		}
	}
}

// TestMetricsCircuitTransitionLabelsAreReadable pins the from/to label values.
// CircuitState is an int-backed enum, so formatting it directly emits the rune
// for the state number rather than the state name. go vet catches that
// conversion, but only if someone is running vet; this pins the output itself.
func TestMetricsCircuitTransitionLabelsAreReadable(t *testing.T) {
	m := NewMetrics()
	m.CircuitTransition(CircuitClosed, CircuitOpen, "p/m1")
	body := renderMetricsBody(m).String()
	if !strings.Contains(body, `airouter_circuit_state_transitions{from="closed",to="open",endpoint="p/m1"} 1`) {
		t.Errorf("circuit transition labels are not the state names:\n%s", body)
	}
	// A raw rune conversion would emit a NUL or SOH byte here.
	if strings.ContainsAny(body, "\x00\x01\x02") {
		t.Error("circuit transition labels contain raw state integers; CircuitState was converted instead of using String()")
	}
}

// TestMetricsLabelValuesCannotForgeSeries covers the exposition-format
// injection path. Provider and model names come from config, and config is
// writable through HandleAdminConfig and rewritten by the model-sync job, so a
// label value was previously interpolated into the quoted string with no
// escaping. A provider named `x" 1\nairouter_requests_total{model="victim`
// produced two syntactically valid series that both read as genuine.
func TestMetricsLabelValuesCannotForgeSeries(t *testing.T) {
	hostile := "x\" 1\nairouter_requests_total{model=\"victim"
	m := NewMetrics()
	m.Request(hostile, 200, time.Millisecond)
	m.Attempt(hostile, time.Millisecond, true)
	m.Fallback(hostile)
	m.CircuitTransition(CircuitClosed, CircuitOpen, hostile)

	body := renderMetricsBody(m).String()

	// The injected payload must not appear as a real series line.
	if strings.Contains(body, `airouter_requests_total{model="victim"}`) {
		t.Errorf("label value forged an extra series line:\n%s", body)
	}
	// It must be escaped, not stripped: the observation still has to be
	// reported, just safely.
	if !strings.Contains(body, `\"`) {
		t.Errorf("label value was not escaped:\n%s", body)
	}
	// The real assertion: every non-comment line must parse as exactly one
	// series under the text exposition format. A forged series or a raw
	// newline in a label value shows up here as a line with trailing garbage
	// or an unparseable value.
	//
	// Naive brace counting is not good enough, and I got that wrong first:
	// it flags every legitimately unlabelled series (airouter_fallbacks_total
	// 1) and would happily pass a forgery that stays on one line, which is
	// exactly the shape of this attack. The parser below is the honest check.
	series, err := parseExposition(body)
	if err != nil {
		t.Fatalf("exposition output does not parse: %v\n%s", err, body)
	}
	// Exactly one model-labelled request series, carrying the hostile value
	// verbatim as a label. If the injection had succeeded there would be two.
	modelSeries := 0
	for _, s := range series {
		if s.name == "airouter_requests_total" && s.labels["model"] != "" {
			modelSeries++
			if !strings.HasPrefix(s.labels["model"], hostile) {
				t.Errorf("hostile model label was altered instead of escaped: %q", s.labels["model"])
			}
		}
	}
	if modelSeries != 1 {
		t.Errorf("got %d model-labelled request series, want exactly 1 — the label forged or lost a series", modelSeries)
	}
}

// TestMetricsCardinalityBoundIsRaceFree exercises the bound under concurrency.
// The cap check and the insert happen in one critical section, so N goroutines
// racing on brand-new endpoints could each see "room" before any of them
// inserts. This asserts the stronger property that matters operationally, namely
// that the cap holds under contention and no observation is lost.
func TestMetricsCardinalityBoundIsRaceFree(t *testing.T) {
	m := NewMetrics()
	const workers = 16
	const perWorker = 400
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				m.Attempt(fmt.Sprintf("w%d:ep%d", w, i), time.Millisecond, true)
			}
		}(w)
	}
	wg.Wait()

	if got := len(m.attemptsByEndpoint); got > maxLabelValues {
		t.Errorf("attemptsByEndpoint exceeded the cap under concurrency: %d > %d", got, maxLabelValues)
	}
	if got := len(m.attemptHistByEnd); got > maxLabelValues {
		t.Errorf("attemptHistByEnd exceeded the cap under concurrency: %d > %d", got, maxLabelValues)
	}
	var total int64
	for _, c := range m.attemptsByEndpoint {
		total += c.Load()
	}
	if want := int64(workers * perWorker); total != want {
		t.Errorf("attempt total under concurrency = %d, want %d", total, want)
	}
}

// TestHandleMetricsServesBoundedOutputEndToEnd runs the cap through the real
// handler, so the guarantee is verified on the path an operator actually scrapes
// rather than only against the internal maps.
func TestHandleMetricsServesBoundedOutputEndToEnd(t *testing.T) {
	cfg := &Config{
		Providers: map[string]ProviderConfig{"p": {URL: "https://example.invalid"}},
		Models:    map[string]ModelConfig{"smart": {Chain: []ModelEndpoint{{Provider: "p", Model: "m"}}}},
	}
	g := NewGatewayContext(NewRouter(cfg, ""), &Proxy{}, cfg, "", "secret-key")
	for i := 0; i < maxLabelValues+500; i++ {
		g.metrics.Attempt(fmt.Sprintf("churn:%d", i), time.Millisecond, true)
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer secret-key")
	rec := httptest.NewRecorder()
	g.HandleMetrics(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	series := 0
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "airouter_endpoint_attempts_total{") {
			series++
		}
	}
	if series > maxLabelValues {
		t.Errorf("served %d attempt series, above the cap of %d", series, maxLabelValues)
	}
	if series < 2 {
		t.Errorf("served only %d attempt series; the overflow bucket is missing", series)
	}
	// Before the cap this was ~7 MB of response for 5000 labels. It should
	// now be bounded, and a scrape should not be able to be used to amplify
	// the response size arbitrarily.
	if rec.Body.Len() > 2<<20 {
		t.Errorf("/metrics body is %d bytes; the scrape response is still amplifiable", rec.Body.Len())
	}
}

// firstLines returns the first n lines of s, for bounded failure output.
func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// exposedSeries is one parsed sample from the text exposition format.
type exposedSeries struct {
	name   string
	labels map[string]string
	value  float64
}

// parseExposition parses Prometheus text exposition format into one entry per
// sample, unescaping label values according to the spec (\" \\ \n).
//
// This exists because "does /metrics still parse" is the property that actually
// matters for a label-injection guard, and eyeballing the output is how the
// original bug survived. It is deliberately strict: a line it cannot parse is
// an error rather than a skip, because a scraper would reject the whole
// response and an operator would see a silently dead dashboard.
//
// Written by hand rather than pulled in as a dependency: the repo has two
// dependencies, both for non-test concerns, and vendoring a Prometheus client
// purely so one test can parse text is a poor trade for a single-file Go
// program.
func parseExposition(body string) ([]exposedSeries, error) {
	var out []exposedSeries
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, labels, rest, err := splitSampleLine(line)
		if err != nil {
			return nil, fmt.Errorf("line %q: %w", line, err)
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
		if err != nil {
			return nil, fmt.Errorf("line %q: bad sample value %q", line, rest)
		}
		out = append(out, exposedSeries{name: name, labels: labels, value: v})
	}
	return out, nil
}

// splitSampleLine splits `name{a="1",b="2"} 42` into its parts.
func splitSampleLine(line string) (string, map[string]string, string, error) {
	brace := strings.IndexByte(line, '{')
	if brace < 0 {
		name, rest, found := strings.Cut(line, " ")
		if !found {
			return "", nil, "", errors.New("no sample value")
		}
		return name, map[string]string{}, rest, nil
	}
	name := line[:brace]
	labels := map[string]string{}
	i := brace + 1
	for {
		// key
		eq := strings.IndexByte(line[i:], '=')
		if eq < 0 {
			return "", nil, "", errors.New("label without =")
		}
		key := line[i : i+eq]
		i += eq + 1
		// value must be quoted
		if i >= len(line) || line[i] != '"' {
			return "", nil, "", fmt.Errorf("label %q value is not quoted", key)
		}
		i++
		var val strings.Builder
		closed := false
		for i < len(line) {
			c := line[i]
			if c == '\\' {
				// Escape sequence: consume the next byte.
				if i+1 >= len(line) {
					return "", nil, "", fmt.Errorf("label %q ends in a dangling escape", key)
				}
				switch line[i+1] {
				case 'n':
					val.WriteByte('\n')
				case '"':
					val.WriteByte('"')
				case '\\':
					val.WriteByte('\\')
				default:
					return "", nil, "", fmt.Errorf("label %q has invalid escape \\%c", key, line[i+1])
				}
				i += 2
				continue
			}
			if c == '"' {
				closed = true
				i++
				break
			}
			if c == '\n' {
				return "", nil, "", fmt.Errorf("label %q contains a raw newline", key)
			}
			val.WriteByte(c)
			i++
		}
		if !closed {
			return "", nil, "", fmt.Errorf("label %q value is unterminated", key)
		}
		labels[key] = val.String()
		// separator
		if i >= len(line) {
			return "", nil, "", errors.New("labels are not closed")
		}
		if line[i] == '}' {
			rest := line[i+1:]
			if strings.TrimSpace(rest) == "" {
				return "", nil, "", errors.New("no sample value after labels")
			}
			return name, labels, rest, nil
		}
		if line[i] != ',' {
			return "", nil, "", fmt.Errorf("expected , or } after label %q, got %q", key, line[i])
		}
		i++
	}
}
