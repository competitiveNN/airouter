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
