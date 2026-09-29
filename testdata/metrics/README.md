# Golden metrics fixtures

Exposition text that a regression must never reproduce. Each file is the
*output*, not input, so a test can assert on what the guard says about it
without needing the registry state that produced it.

These are committed rather than left in the fuzz cache for two reasons: the
fuzz corpus is gitignored because it is gigabytes of generated input, and
because the most valuable case here is a specific historical output that no
fuzzer is likely to stumble on. It is transcribed from a real scrape of the
pre-fix gateway, reduced to the affected family.

| File | What it is | What must happen |
|------|-----------|------------------|
| `overflow-as-histogram.prom` | The `__overflow__` bucket exported as a histogram with zeroed finite bands | The guard must reject it. This is the defect that survived three audit rounds. |
| `valid-histograms.prom` | Well-formed output across both histogram families | The guard must accept it with zero violations. A guard that fires here would be wrong to ship. |

Run: `go test -run 'TestGolden' ./...`

## Provenance

### `overflow-as-histogram.prom`

Transcribed from a real scrape of `/metrics` on the pre-fix gateway, then
reduced to the one affected family. The affected family is
`airouter_endpoint_attempt_duration_seconds` carrying
`endpoint="__overflow__"`; every other line in that scrape was dropped, so the
file is 18 lines (2 comment, 13 bucket, 3 series) rather than a full exposition.
It is deliberately *not* regenerable: the bug it records no longer exists in the
code, so no code path produces this output any more. That is the point — it is
evidence, not a build artifact, and its only loss mode is deletion.

What makes it a violation, concretely: `_count` is 11 and all 11 observations
sit in the `+Inf` bucket, so the minimum possible `_sum` is 11 * 45 (the top
finite bound) = 495. The reported `_sum` is `0.11`. The sum is 4500x smaller
than the smallest value the bucket distribution admits.

### `valid-histograms.prom`

Generated from a live gateway driven under load, not hand-written, specifically
so it cannot be "fixed" into looking valid by the same edit that breaks the
guard. It was then reduced to the families the guard inspects, keeping the
`__overflow__` gauges and the per-endpoint histogram alongside the request
histogram. `fake-fast-1` is absent from the histogram because its label was
evicted and its history rolled into the `airouter_evicted_attempts_latency_*`
gauges — the gap is the evidence that eviction happened, not an omission.

Scale: 180 requests through the unlabelled request-duration histogram, 15
per-endpoint label sets on the attempt-duration histogram (7 `fake-fast-*`,
8 `fake-large-*`), for 16 histogram label sets total.

Note: the two `airouter_metrics_label_{overflow,evictions}_total` counters are
declared (`# HELP`/`# TYPE`) in this fixture but carry no sample line, even
though `metrics.go:978,988` emit them unconditionally. That is a gap in the
capture, not a deliberate case — a future regeneration from a live scrape will
include them. It does not weaken the fixture, because the guard does not read
those counters, but it does mean the header's eviction claim is carried by the
missing `fake-fast-1` label and the zeroed `*_evicted_attempts_*` gauges rather
than by the counter.

The two `*_evicted_attempts_latency_seconds_*` metrics are typed `gauge`, not
`histogram`, and that is load-bearing. The per-latency-band distribution for an
evicted endpoint is unknown and is not reported, so exposing them as a
histogram would be a lie; `histogram_quantile` over them is meaningless. The
guard has to accept that typing, which means this fixture covers a case the bad
one cannot.

## Regenerating `valid-histograms.prom`

Start the gateway against a config whose endpoints point at local fakes, drive
enough distinct endpoint labels to force evictions, and scrape:

    curl -s localhost:8080/metrics > testdata/metrics/valid-histograms.prom

Replacing the file is expected to require revisiting the numbers recorded in
`TestInvariantSixThresholdHasNoHeadroom` — that is the point of keeping the
measurement in a test rather than in a commit message. See
`docs/audit.md` for the round-by-round history behind both fixtures.

## A caveat worth keeping

On the committed `valid-histograms.prom`, all 16 label sets currently
short-circuit invariant (6) on `above<=0`: every observation lands in the top
finite bucket, so the comparison is never reached. The test logs this as
"0 reach invariant (6)'s comparison" rather than passing over it. Real traffic
that includes genuinely slow requests will eventually populate the top band, at
which point that number changes and the tightest-`k` log line starts carrying
real evidence. Do not treat the absence of a violation in that fixture as
evidence that the bound is loose — the test asserts eligibility for precisely
that reason.
