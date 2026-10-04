# airouter — Operator Runbook

This document describes how to operate the airouter gateway: configuration,
cooldowns, metrics, admin endpoints, and common failure modes.

## Configuration

The gateway reads `config.yaml` on startup and hot-reloads it every 3 s when
the file's mtime or size changes (see `watchConfig` in `main.go`). The reload
is atomic: gateway, router, and proxy share one `*atomic.Pointer[Config]`, so
an in-flight request never observes a mix of old and new pointers.

### Top-level structure

```yaml
providers:
  <name>:
    url: https://...            # http:// or https:// required
    api_key_env: ENV_VAR        # optional; reads from environment
    headers:                    # optional; sent on every upstream request
      X-Custom: value
preferences:
  newest_first_on_tie: true     # tie-break newer models first
  initial_rotation_window: 8    # 0 disables rotation
  cooldown_jitter: 0.25         # 0 disables; clamped to [0, 0.25]
models:
  smart:                       # logical model name
    chain:
      - provider: <name>
        model: <upstream-model>
        protocol: responses      # optional; absent means chat/completions
```

`protocol` is per ENDPOINT, not per provider, because a provider's catalog is
not uniformly Responses-capable: measured 2026-10-02, opencode's
`muse-spark-1.{2,3}-contributor-free` answer `400 ModelProtocolUnsupported` on
`/chat/completions` while answering 200 on `/responses`, and `big-pickle` is the
other way round. Without the field the router still recovers — it flips and
retries the same endpoint — at the cost of one failed round trip per endpoint
per process start. `fetch-free-models.py` records the shape its probe proved,
`regenerate_config.py` writes it, and `check-rules.py` rule 13 compares the two.
Never write `protocol: chat`: absence already means it.

### Logical models

The four logical models are fixed: `smart`, `work`, `fast`, `large`. Each
maps to a fallback chain. The client always requests a logical model; the
gateway resolves it to a concrete endpoint at request time.

### Provider API keys

Keys are read from environment variables named by `api_key_env`. The gateway
refuses to start without a gateway API key (`GATEWAY_API_KEY` or `-api-key`)
unless `-allow-no-auth` is passed (insecure).

## Routing

1. A request arrives for a logical model.
2. `bodySessionID` derives a stable session ID from the model + messages
   (content-addressed, 16-byte hex). This is the sticky-session key.
3. `SelectEndpoint` returns the pinned endpoint for that session, or the
   first eligible endpoint of a new session (with optional rotation across
   the head of the chain).
4. On failure, the endpoint is marked `tried` (per-request set), a cooldown
   is applied, and the loop walks the chain.

The walk is bounded twice, by `budgetState`:

- `maxAttempts = chainLen*3 + 1` bounds the total iterations.
- A **wall-clock budget** bounds elapsed time, because attempts alone is not a
  bound — an attempt that hangs until its per-attempt timeout costs far more
  than a fast 429. The budget is `fallbackBudget(timeout)`: 12s for small
  requests, 20s for mid-size, and `maxFallbackWallClock` otherwise, floored at
  twice the per-attempt timeout. The first attempt is always allowed, so a
  request is never failed before it has been tried once.

All five fallback loops (chat completion, chat stream, Responses, Responses
stream, Responses WebSocket turn) share this policy, and
`TestAllFallbackLoopsAreBudgetBounded` parses their source and fails if any
loop ships without a `budgetState`. That test exists because a loop was found
shipping unbounded, bound only by a loose 10-minute timeout.

### Cooldowns

Cooldowns are persisted to `cooldowns.json` (atomic tmp+rename) next to the
config file. `ErrorCount` is the number of CONSECUTIVE failures: it is reset by
`RecordSuccess` (any successful response deletes the entry) and capped at 100.
With a 30 s base the transient ladder is 30 s, 1 m, 2 m, 4 m, 8 m, 16 m, and
then the 30-minute ceiling for every failure after the sixth.

| Status | Base cooldown | Escalation | Ceiling |
|--------|---------------|------------|---------|
| 429    | 30 s          | ×2 per consecutive failure | 30 min |
| 500/502/503 | 30 s      | ×2 per consecutive failure | 30 min |
| 504    | 60 s          | ×2 per consecutive failure | 30 min |
| 0 (timeout, transport, upstream SSE) | 30 s | ×2 per consecutive failure | 30 min |
| 401/403 | 30 min       | 24 h at 3 failures, 7 days at 10 | 7 days |
| 402    | 30 s          | 24 h at 3 failures, 7 days at 10 | 7 days |
| 404    | 7 days        | — (never shortens) | 7 days |
| 400/413/422 | none (client error) | — | none |
| 400 `ModelProtocolUnsupported` | 24 h | — | 24 h |

Three properties of that table are deliberate, and each one was a bug once:

- **A cooldown never shortens as failures accumulate.** A 404's base is already
  longer than the 24 h soft ban, so the soft ban is only applied when it is
  longer than the base. Returning it unconditionally made a model not found sit
  out seven days on the first two failures and one day on the third.
- **The transient ceiling is reachable.** It used to be `base × errorCount` with
  the factor capped at 10, which with a 30 s base cannot exceed five minutes —
  a twelfth of the pre-2026-08-30 behaviour and unreachable in practice, so a
  model that never answered sat on a 5-minute bench indefinitely.
- **"No HTTP status" is transient, not fatal.** Status 0 is a transport error, a
  parse failure or an upstream SSE event; treating it as "probably permanent"
  handed three consecutive timeouts a 24-hour ban.

Transient errors (429/5xx) receive up to 25% random jitter (`cooldown_jitter` in
preferences) to break thundering herds. A 429's upstream `Retry-After` header is
honored as a floor on the computed cooldown — but not on a client error, where
the provider is not asking us to back off.

#### Cooldowns are also the free-tier oracle

A cooldown is not only a backoff: it is the only record of what happens when
real client traffic reaches an endpoint, and no provider API answers that
question. `fetch-free-models.py` reads this file on every sync (step 0 of
`scripts/sync-models.sh`, `--cooldowns-report`) and vetoes any endpoint it finds
recurrently refused for free-tier reasons — 402 billing, 403 with free-tier
wording, 404 gone — persisting the verdict to `free-tier-denied.json` so a quiet
week does not erase it.

Inspect what a sync will act on, without touching the network:

    python3 fetch-free-models.py --cooldowns-report

| In cooldowns.json | Verdict |
|-------------------|---------|
| 402 / 403 with free-tier wording, ≥3 consecutive | `not-free` → vetoed |
| 404 | `gone` → vetoed |
| 429, 5xx, `context deadline exceeded`, `status_code: 0` | transient → ignored |
| the `circuits` section | ignored (open/half-open are probes in flight) |

Two things follow for operations. A model in the chains with an active denial
will fail: clear it by fixing the entitlement, or wait out the 168 h window
(`AIROUTER_DENIAL_TTL_HOURS`). And a chain that stops growing after a provider
drops a free model is the veto working, not a bug — `free-tier-denied.json`
names the endpoint and the error that removed it.

## Metrics

Prometheus text-format metrics are exposed at `GET /metrics`, authenticated
with `Authorization: Bearer <key>` like the admin surface. The series are
labelled by provider and model and describe routing behaviour, so an
unauthenticated scrape is an information leak, not a health check — use
`/health` (below) for liveness.

| Series | Type | Use |
|--------|------|-----|
| `airouter_requests_total{model,status}` | counter | Traffic per profile |
| `airouter_failures_total{status}` | counter | Client-visible failure codes |
| `airouter_request_duration_seconds` | histogram | End-to-end latency |
| `airouter_fallbacks_total`, `airouter_fallbacks_total_by_endpoint{endpoint}` | counter | Which endpoint is failing over |
| `airouter_cooldowns_applied_total` | counter | Cooldown pressure |
| `airouter_circuit_state_transitions{from,to,endpoint}` | counter | Circuit flapping |
| `airouter_endpoint_attempts_total{endpoint}` | counter | How often each upstream is reached |
| `airouter_endpoint_attempt_failures_total{endpoint}` | counter | Failure ratio per upstream |
| `airouter_endpoint_attempt_duration_seconds{endpoint}` | histogram | Per-attempt cost per upstream |
| `airouter_sse_comments_routed_out` | counter | Upstream keepalive chatter filtered out of the event path |
| `airouter_metrics_label_overflow_total` | counter | Label observations folded into `__overflow__` because a map was full with nothing recyclable |
| `airouter_metrics_label_evictions_total` | counter | Endpoint series recycled out of the window into `__overflow__` to make room for a new label |
| `airouter_evicted_attempts_latency_seconds_sum` | gauge | Latency of attempts whose endpoint was evicted. **Not** a histogram — see below |
| `airouter_evicted_attempts_latency_seconds_count` | gauge | Count of attempts whose endpoint was evicted. **Not** a histogram — see below |

The `airouter_endpoint_attempt_*` series are the ones to read when deciding
whether a fallback chain needs reordering: `fallbacks_total` says an endpoint
misbehaved, but only the attempt series say whether it is worth reaching at
all. A high attempt count with a high failure ratio means the endpoint is
reached and broken (demote it); attempts piling into the 12s+ buckets mean the
chain pays a real timeout for it on every request (reorder or shorten the
budget). `airouter_sse_comments_routed_out` should normally be non-zero — it
counts `: keepalive` lines stripped before they could be misread as events.

All four request surfaces (chat completions, responses, responses-stream, and
responses-websocket) record the per-endpoint attempt telemetry, so the series
cover fallback traffic regardless of which surface the client used.

### Label cardinality is bounded

Endpoint and model labels are derived from config, and config is **not** fixed
for the process lifetime: `POST /admin/config` accepts an arbitrary new config
and the file watcher hot-reloads every 3s. Without a bound, a config churn loop
(or a model sync that keeps inventing model ids) grows the registry until the
gateway is OOM-killed — the metrics endpoint becomes a way to kill the process.

So every label map is capped in `metrics.go`. The endpoint map (which backs
`airouter_endpoint_attempt_*`, the series you actually diagnose with)
**recycles**; the smaller maps (`requests_by_model`, `requests_by_status`,
`failures_by_status`, `fallbacks_by_from_endpoint`, `circuit_transitions`)
**collapse**. Both end in a single `__overflow__` bucket.

**Recycling** (`attemptsByEndpoint`): once the map is full, a new endpoint label
evicts the least-recently-admitted tracked endpoint and rolls its attempt count,
failure count and latency sum into `__overflow__`. Recycling rather than
first-come admission is the whole point: with a never-releasing registry, a
burst of labels from a config that is no longer live holds its slots
permanently, and a genuinely hot endpoint that first appears later is
collapsed forever. The gateway would then report a broken upstream as "no
data" indefinitely — the worst possible failure for the series whose entire job
is answering "is this upstream healthy?". Admission is FIFO, not LRU:
re-touching an endpoint does not refresh its position, which avoids taking the
global write lock on every single observation.

Two consequences specific to the endpoint map:

- **Attempt counts, failure counts and latency sums are never lost.** They roll
  into the bucket, and the totals are exported as the
  `airouter_evicted_attempts_latency_seconds_{sum,count}` gauges.
- **Per-band histogram data is *not* merged**, and cannot honestly be. Once the
  endpoint is gone, "the 0.25s band held 40 observations, all from a label we
  evicted" is not representable.

That second point is why **`__overflow__` is not exported as a histogram at
all** — no `_bucket`, `_sum` or `_count` series exist for it under
`airouter_endpoint_attempt_duration_seconds`. An earlier version did export
one, and it was actively corrupt rather than merely imprecise:

```text
airouter_endpoint_attempt_duration_seconds_bucket{endpoint="__overflow__",le="0.25"} 0
airouter_endpoint_attempt_duration_seconds_bucket{endpoint="__overflow__",le="45"}   0
airouter_endpoint_attempt_duration_seconds_bucket{endpoint="__overflow__",le="+Inf"} 11
airouter_endpoint_attempt_duration_seconds_sum{endpoint="__overflow__"} 0.110000
airouter_endpoint_attempt_duration_seconds_count{endpoint="__overflow__"} 11
```

The 11 observations are reported as *entirely above 45 seconds* while their own
sum says they averaged 10ms. A Prometheus histogram is cumulative, so every
finite bound must be `<= +Inf`; here the largest is 0. Nothing rejects it — the
scrape parses, the family declares `histogram`, every dashboard is green, and
`histogram_quantile` returns a plausible number with no relationship to
reality. A silent wrong answer is worse than no answer.

So the overflow totals are published as **gauges** under names that no quantile
function will accept by accident. Two consequences when writing queries:

- **Do not** pass `airouter_evicted_attempts_latency_seconds_*` to
  `histogram_quantile`. The distribution genuinely does not exist; only the
  count and sum do.
- Mean latency of evicted attempts, if you want it, is
  `airouter_evicted_attempts_latency_seconds_sum / airouter_evicted_attempts_latency_seconds_count`.
  Both series are emitted for **every** endpoint label (zero for live ones), so
  a ratio over the whole set needs no special case and the metric does not
  appear and disappear as the cap is crossed.

The gauges are counters in spirit but are declared `gauge` deliberately: an
eviction *adds* to them, and a `rate()` over a series that can decrease is worse
than no series. Read them as totals since process start, not as a rate.

**Collapsing** (the other maps): a label that is not already tracked and finds
the map full is counted directly against `__overflow__`. This costs no series,
so `airouter_metrics_label_overflow_total` increments and should be `0` in a
healthy deployment.

- **Already-tracked labels stay distinct.** A churning config never displaces
  the endpoints that are actually hot; only the tail collapses.
- **If `airouter_metrics_label_overflow_total` is non-zero**, either the cap is
  too low (see below) or something is feeding unbounded label values (check for
  a config rewrite loop). Treat the bucket as "unknown endpoints" rather than
  as one specific provider.
- **If `airouter_metrics_label_evictions_total` is climbing steadily**, real
  endpoints are losing their series to newer ones. Raise the cap.

`__overflow__` always appears under the **same label name the series already
uses**: `requestsByModel` collapses to `model="__overflow__"`,
`requestsByStatus` and `failuresByStatus` to `status="__overflow__"`, and the
endpoint-keyed maps to `endpoint="__overflow__"`. That is deliberate. A bucket
that arrived under a different label name would change the metric's label
schema, which Prometheus reads as a new series — so a query grouping by status
would silently lose the bucket, and an alert watching a status series would
stop covering it. The cap is a safety valve; it must not reshape healthy data.

Grep for `__overflow__` across the whole exposition to find every bucket
regardless of which dimension collapsed.

The cap is a memory bound, not a tuning knob with a correct value: the live
config has 60 distinct endpoint keys, so the 512 default leaves ~8x headroom.

### Tuning the label cap

`preferences.max_label_cardinality` in `config.yaml` overrides the default, in
`[16, 65536]`. It is applied at construction and re-applied on every config
reload, so changing it does not need a restart. Out-of-range values are
**rejected**, not clamped: `scripts/validate-config.py` fails before a restart,
and `Config.validate` fails again after one. Below the floor destroys the
per-endpoint series this exists to protect; above the ceiling defeats the memory
bound, and a setting that silently does something other than what it says is
worse than a startup failure. `TestValidateConfigRangeMatchesGo` asserts the
script and Go agree on the bounds, so they cannot drift apart silently.

## SSE framing contract

A client stops parsing at the first `data: [DONE]`, so a sentinel that appears
ahead of the content silently truncates the response while the request still
returns HTTP 200. `proxy.go streamSSE` therefore guarantees:

- exactly one `data: [DONE]`, always last, emitted by the gateway even when
  the upstream closes without sending one;
- upstream `: keepalive` comment lines are dropped before they can be
  misparsed as an event (counted by `airouter_sse_comments_routed_out`);
- the pre-release prefix (role-only / reasoning-only openers) is flushed
  *before* the event that triggers the release, so a late `role` delta cannot
  follow the first content token;
- with `--tool-calls`, partial tool-call deltas are coalesced into one complete
  call emitted immediately before `[DONE]`, never after it.

Verify against a running gateway — this checks captured bytes, not status
codes:

```sh
AIROUTER_API_KEY=$KEY python3 scripts/sse-check.py --expect "PING OK" --rounds 3
```

This is wired into CI as the `sse-contract` job, which boots a real gateway
against `scripts/fake-sse-upstream.py` (a deliberately hostile SSE upstream
that interleaves comment lines and omits the trailing blank line) so the
contract is asserted on every push without provider credentials. The fixture
config is `testdata/ci-config.yaml`.

### Required status checks

CI enforces what it can from the workflow file: every job sets
`continue-on-error: false` (so a red X is a failure, not a "known flake"), jobs
use `fail-fast: false` (so one broken guard does not cancel the others and hide
their diagnostics), and a nonzero exit fails the job.

What a workflow **cannot** enforce is whether a red X actually blocks a merge.
That is a repository setting, and until it is configured the SSE contract job
is advisory. This is not reachable from the repo — it is an org-level setting —
so it has to be done once by an admin:

> Settings → Branches → Branch protection rules → `master` →
> "Require status checks to pass before merging".

Add these three required contexts (names must match the job ids exactly):

| Required status check | Job |
|----------------------|-----|
| `go` | build, vet, gofmt, tests, SSE negative check, secret scan, audit anchors |
| `config` | config validation and generator regression tests |
| `sse-contract` | end-to-end SSE termination contract, metrics assertions |

Note the check name is the **job id**, not a step name. If `sse-contract` is
left out of this list, the whole point of the job — asserting a bug that
returns HTTP 200 and is invisible to every other check — is that a green build
still ships a broken stream. Mark it required when adding the rule.

## Health

`GET /health` is unauthenticated and returns `{"status":"ok"}`. It exposes no
config, sessions, cooldowns, provider URLs, or keys — safe for load balancers.

## Common failure modes

- **"All models are currently unavailable" (503):** every endpoint in the
  chain is in cooldown. Wait for the shortest cooldown to expire (the handler
  sleeps, bounded by `maxAttempts`), or reset cooldowns via `/admin/cooldowns`.

  ```sh
  # one endpoint
  curl -X DELETE -H "Authorization: Bearer $KEY" \
    'http://127.0.0.1:9090/admin/cooldowns?model=opencode:muse-spark-1.2-contributor-free'
  # everything (drops all backoff protection at once)
  curl -X DELETE -H "Authorization: Bearer $KEY" \
    'http://127.0.0.1:9090/admin/cooldowns'
  ```

  Returns `{"cleared":N,"model":...}`, 404 if the named model has no cooldown.

  **Do not hand-edit `cooldowns.json` while the daemon is running.** The router
  holds the authoritative state in memory and rewrites the whole file from it on
  the next failure, so your edit is reverted within seconds. If you must edit,
  stop the service first. Note `circuits.*.state` is an **int** (0 closed, 1
  open, 2 half-open), not a string — a bad value there used to abort the whole
  file parse and silently reset every cooldown.
- **Sticky session pinned to a dead model:** the session ID is derived from the
  request body, so changing the messages or model moves to a new session.
- **Cooldowns not expiring:** verify `cooldowns.json` is writable; the router
  debounces writes (one per 500 ms) and uses atomic tmp+rename, so a crash
  mid-write cannot corrupt the file.
- **Provider 429 with Retry-After ignored:** the header is parsed in
  `ParseRetryAfter` (delta-seconds and HTTP-date) and honored as a floor.