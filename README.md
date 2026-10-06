# airouter

An OpenAI-compatible gateway that exposes four logical models — `smart`,
`work`, `fast`, `large` — over any number of upstream providers, and
speaks both the Chat Completions and the Responses API.

Each logical model is a **fallback chain**: a list of provider/model
endpoints tried in order. The gateway is built to be invisible to the
client, so:

- **Sessions are sticky.** The same session is always routed to the same
  model.
- **Errors are absorbed, not returned.** A 429, 5xx, or mid-stream
  failure moves the session to the next endpoint in the chain and
  continues. The client sees a slight delay, never an error.
- **Cooldowns are error-graded.** A model that errors gets a cooldown
  whose length depends on the error: a 429 is short, a 404 is long, a
  400 is not a cooldown at all — the three mean very different things
  about the upstream.
- **Failures are caught mid-stream too.** If the upstream dies after the
  first chunk, the gateway rewinds and resumes on the next endpoint, so
  the client receives one coherent stream.
- **The fallback walk is bounded.** Attempts are capped
  (`chainLen*3 + 1`) *and* a wall-clock budget (12s for small requests,
  20s mid-size, more for large) bounds elapsed time, because a hanging
  attempt costs far more than a fast 429.
- **When every endpoint is on cooldown, the gateway fails fast.** The
  configured cooldowns run 30m for auth errors and up to 24h for
  hardened rate limits, so a request that waits them out is guaranteed
  to fail again — it returns `503 all_models_unavailable` immediately.

## API surface

| Endpoint | What it is |
|---|---|
| `POST /v1/chat/completions` | OpenAI Chat Completions (streaming and non-streaming) |
| `POST /v1/responses` | OpenAI Responses API, translated to the upstream's shape |
| `POST /v1/responses/ws` | Responses API over WebSocket (one turn per socket) |
| `GET /v1/models` | The four logical models; `max_context_tokens` advertises the **smallest** context window in the chain, so a client never plans for more input than every backend the profile might fall back to can accept |
| `GET /health`, `GET /metrics` | Liveness; Prometheus metrics (authenticated) |
| `GET /v1/airouter/state` | Unauthenticated snapshot of every endpoint's circuit state and remaining cooldown |
| `/admin/*` | Authenticated: config (GET/POST), sessions, cooldowns, providers, and an HTML dashboard at `/admin/` |

### Protocol-aware routing

Which upstream shape answers is a per-**endpoint** property, not a
per-provider one: a provider's catalog can be mixed, measured live —
opencode's `muse-spark-1.{2,3}-contributor-free` answer
`400 ModelProtocolUnsupported` on `/chat/completions` while answering
`200` on `/responses`, and `big-pickle` is the other way round. An
endpoint may therefore declare `protocol: responses` (absence means
Chat Completions). When an upstream refuses the shape the gateway is
speaking, the router flips to the other protocol and retries the *same*
endpoint — once per endpoint per request — so a missing field costs one
round trip, not a failed request.

### Capability-aware routing

Every concrete endpoint carries a `vision:` boolean from the source
model's capabilities. Image requests are routed only to vision-capable
endpoints, and an endpoint that rejects an image at runtime is learned
as text-only for subsequent vision requests (runtime-only, not
persisted).

## Running it

```sh
go build -o airouter .
./airouter -config config.yaml -port 8080 -api-key "$AIROUTER_API_KEY"
```

| Flag | Default | What it does |
|---|---|---|
| `-config` | `config.yaml` | Config path; `cooldowns.json` is read/written next to it |
| `-port` | `8080` | Listen port |
| `-api-key` | — | Bearer token clients present. Also read from `GATEWAY_API_KEY`. Required unless `-allow-no-auth` |
| `-allow-no-auth` | `false` | Run unauthenticated (insecure: every endpoint, admin included) |
| `-warmup` | `false` | Fire test requests at each provider on startup |
| `-tool-calls` | `false` | Synthesize tool-call events in streaming responses |

Provider credentials come from the environment, named by `api_key_env`
in the config — secrets are never written into the config file.

Validate a config before restarting onto it:

```sh
python3 scripts/validate-config.py config.yaml
```

The config hot-reloads: a file watcher picks up every change within 3s
(`POST /admin/config` does the same), atomically, so an in-flight
request never observes a mix of old and new config. Sessions and
cooldowns survive a reload.

## systemd services

Two user units ship in `systemd/`. They assume the checkout lives at
`~/dev/airouter` (the units use `%h`), the binary is built
(`go build -o airouter .`), and a `.envrc` sits next to it holding the
provider keys as `export KEY=value` lines (direnv format; gitignored).

```sh
# one-time: install the units into the user manager
mkdir -p ~/.config/systemd/user
ln -s ~/dev/airouter/systemd/airouter.service ~/.config/systemd/user/
ln -s ~/dev/airouter/systemd/airouter-model-sync.{service,timer} ~/.config/systemd/user/
systemctl --user daemon-reload

systemctl --user enable --now airouter.service           # the gateway
systemctl --user enable --now airouter-model-sync.timer  # nightly config refresh
```

`airouter.service` runs the daemon via `scripts/run-airouter.sh`, which
sources `.envrc` and `exec`s the binary so systemd supervises the real
PID. `airouter-model-sync.timer` fires `airouter-model-sync.service`
daily at 06:00 (±15 min, `Persistent=true`), refreshing the free-model
list and rewriting `config.yaml` — which the daemon then hot-reloads.
Run the sync by hand with `bash scripts/sync-models.sh`.

**Why not systemd's `EnvironmentFile=`:** it is not a shell parser. It
silently ignores the `export KEY=value` form direnv writes and resolves
no intra-file references (e.g. `CLOUDFLARE_API_TOKEN=$CF_API_KEY`).
Verified against systemd 252: pointing `EnvironmentFile=` at `.envrc`
imports zero variables, with no warning. Sourcing it through bash is
the one approach that works, and `scripts/export-envrc.sh` imports the
allowlisted key names into the systemd user manager for interactive
`systemctl --user` use.

The sync unit is deliberately a good citizen: `Type=oneshot`,
`TimeoutStartSec=30min`, `Nice=10`, `IOSchedulingClass=idle`, because
it issues a few dozen API calls and rewrites caches while the daemon
serves. It reads `cooldowns.json` while the daemon may be writing it —
safe by construction, since cooldown saves are atomic tmp-file renames,
so the sync sees either the old envelope or the new one, never a
half-written one.

## Configuration

Top-level keys are `providers` and `models`; everything else is optional
tuning under `preferences`.

### `providers`

```yaml
providers:
  openrouter:
    url: https://openrouter.ai/api/v1
    api_key_env: OPENROUTER_API_KEY
    headers:            # optional, sent verbatim upstream
      X-Custom: value
```

A provider named `opencode`, `opencode-go` or `opencode-zen` is treated
as an OpenCode gateway and gets client-attribution headers
(`X-Session-ID`, a versioned `User-Agent`) automatically, without which
the upstream rejects free-tier models from non-OpenCode clients.

### `models`

```yaml
models:
  smart:
    chain:
      - provider: openrouter
        model: anthropic/claude-sonnet-4.5
        vision: true            # capability-aware routing
        intelligence: 52.3      # read for initial-session rotation
        context_length: 200000  # surfaced via /v1/models
        protocol: responses     # optional; absent means chat/completions
      - provider: openai
        model: gpt-5
```

The chain is tried in order. `smart` should hold the most intelligent
model available, `work` the coding workhorse, `fast` something small
for quick or verbose-but-simple work, and `large` something with a very
large context window for big inputs and context compression.

Endpoints are tried in chain order unless session rotation is enabled
(below). The router places models in the chain by intelligence score
where it can, so the declared order is a fallback rather than the whole
routing policy.

The shipped `config.yaml` is generated, not hand-maintained — see
[Model sync](#model-sync).

### `preferences`

| Key | Default | What it does |
|---|---|---|
| `newest_first_on_tie` | `true` | Breaks intelligence-score ties in favour of the newer model. `false` keeps the provider's enumeration order. |
| `initial_rotation_window` | `8` | Rotate across the first N endpoints of a chain for the *first* pick of each new session, so load spreads over healthy upstreams. `0` disables. Retries still walk the chain in order, so rotation never delays escalation. |
| `cooldown_jitter` | `0` | Random fraction `[0, 0.25]` added to transient cooldowns so concurrent clients don't wake at the same instant and stampede a recovering provider. `0` disables. |
| `circuit_breaker_threshold` | `5` | Consecutive failures that trip a circuit from `closed` to `open`. An explicit `0` disables the breaker and leaves failure handling to ad-hoc cooldown escalation. |
| `circuit_half_open_probes` | `1` | Probes allowed in `half-open` before a circuit is treated as still failing. |
| `default_max_tokens` | unset | Overrides the request `max_tokens` when the client does not send one. |
| `max_label_cardinality` | `512` | How many distinct label values each `/metrics` map may hold. Range `[16, 65536]`; out-of-range values are **rejected**, not clamped. See below. |

Every default in the table is the value used when the key is absent. For
some keys a written `0` is deliberately distinguishable from an omitted
key: `circuit_breaker_threshold: 0` disables the circuit breaker, where
omitting it would use the default of 5.

### `max_label_cardinality`

Endpoint and model labels come from the config, and the config is not fixed for
the process lifetime: `POST /admin/config` accepts a new one and the file
watcher hot-reloads every 3s. Without a bound, a config churn loop — or a model
sync that keeps introducing new model ids — grows the metrics registry until the
gateway is OOM-killed. A metrics endpoint that can be used to kill the process
is a self-inflicted DoS.

So every label map is capped, and the cap is applied at startup *and* on every
reload, so changing it does not need a restart.

Once the cap is reached:

- **`airouter_endpoint_attempt_*`** (the per-endpoint attempt telemetry)
  *recycles*: a new endpoint displaces the least-recently-admitted one and rolls
  its attempt count, failure count and latency sum into an `__overflow__` bucket.
  Recycling rather than first-come admission is the point — otherwise a burst of
  labels from a config that is no longer live holds its slots forever and a
  genuinely hot endpoint is collapsed for the rest of the process's life.
- **The other maps** *collapse*: an unknown label is counted directly against
  `__overflow__` without taking a slot.

Two counters make this visible rather than silent:

- `airouter_metrics_label_evictions_total` — a real series was displaced. A
  steady climb means the cap is too small for the deployment.
- `airouter_metrics_label_overflow_total` — an observation was folded into the
  bucket. Should be `0` in a healthy deployment.

`__overflow__` keeps each series' own label name (`status="__overflow__"`,
`model="__overflow__"`, `endpoint="__overflow__"`) so collapsing never changes
the label schema of healthy series.

**The overflow bucket is not a histogram.** Eviction can roll a victim's attempt
count and latency sum into `__overflow__`, but not its per-latency-band counts:
once the endpoint is gone, "the 0.25s band held 40 observations from a label we
evicted" is not representable. So `__overflow__` publishes no
`airouter_endpoint_attempt_duration_seconds_*` series at all, and its totals
appear instead as two gauges:

```text
airouter_evicted_attempts_latency_seconds_sum{endpoint="__overflow__"}    0.095540
airouter_evicted_attempts_latency_seconds_count{endpoint="__overflow__"}  84
```

Both are emitted for every endpoint label (zero for live ones), so the series
neither appears and disappears as the cap is crossed nor needs a special case in
a "total minus live" query. Do not pass them to `histogram_quantile` — the
distribution genuinely does not exist, only the count and the sum. Mean latency
of evicted attempts is `sum / count` by hand.

They are gauges rather than counters because an eviction *adds* to them, and a
`rate()` over a series that can decrease is worse than no series. Read them as
totals since process start.

`docs/RUNBOOK.md` ("Label cardinality is bounded") covers what to watch, and
`docs/audit.md` records the bugs found along the way — including four that a
green test suite did not catch, among them an `__overflow__` bucket that was
being exported as a histogram whose finite buckets were all `0` while its own
`_sum` said every observation was nearly instant. It parsed, declared
`histogram`, and made every dashboard look healthy.

## Model sync

`config.yaml` is the output of a nightly pipeline, not a hand-edited
file. `scripts/sync-models.sh` (the timer's unit) runs:

1. **Cooldown report** — `fetch-free-models.py --cooldowns-report` reads
   `cooldowns.json`, the gateway's own record of what real client traffic
   did to each endpoint, and prints which endpoints the gateway is being
   *refused* on for free-tier reasons. Touches no network.
2. **Fetch** — `fetch-free-models.py --json --probe-auto` pulls the free
   model listings from every provider (NVIDIA NIM, Kilocode, OpenCode,
   Ollama Cloud, Google AI Studio, CommandCode), enriches each candidate
   with an intelligence score (Artificial Analysis index, or a normalized
   arena.ai code-leaderboard ELO when AA has none), and **proves every
   candidate with a real 1-token completion** — a listing is not evidence
   a model is callable. It also probes both chain terminators
   (kilocode `kilo-auto/free`, opencode `big-pickle`) and records a
   live/dead verdict per router, prunes models the providers no longer
   list, and vetoes every endpoint step 1 found refused, persisting the
   denial to `free-tier-denied.json` (168h TTL,
   `AIROUTER_DENIAL_TTL_HOURS`) so a quiet week does not forget it.
   First-seen dates are committed in `first-seen-cache.json`, so they
   survive a fresh clone.
3. **Regenerate** — `regenerate_config.py --write` rewrites the
   `models:` section deterministically (same input, same output, ~0.1s,
   no agent, no commits): profile membership (smart ≥ 25, work ≥ 15,
   fast < 25 or small-model ids capped at 10, large ≥ 200k context),
   best-first ordering, the NVIDIA three-key and CommandCode two-key
   expansions, and the auto-fallback terminator chosen on the probe
   verdict — never on a count, because counts are not a health signal.
4. **Verify** — `validate-config.py`, then `scripts/check-rules.py`,
   which enforces the distribution rules against the fetched list. Any
   failure restores the previous `config.yaml` and fails the sync.

Two properties of the veto are worth internalising. It needs a *recurring*
refusal with free-tier wording (402 billing, 403 free-tier wording, 404
gone) — a 429, a 5xx, a timeout, and the `circuits` section are never
evidence, because vetoing on those would strip the chains during an
outage, which is far worse than the rate limit it replaced. And it lives
in the fetcher, not the rules checker: a checker that rejects a denied
model fails the sync, the sync restores the previous config, and that
config still contains the denied model. A restored entitlement comes back
by itself when the TTL expires.

The daemon hot-reloads the result within 3s, so a sync needs no restart.
`SKIP_AUTO_PROBE=1` makes the fetcher touch no network (offline /
fixture runs); `NO_COOLDOWN_VETO=1` reads the report without acting on it.

## Cooldowns

Cooldowns persist to `cooldowns.json` (atomic tmp+rename) next to the
config, so they survive restarts. `ErrorCount` is the number of
*consecutive* failures — reset by any success, capped at 100.

| Status | Base cooldown | Escalation | Ceiling |
|--------|---------------|------------|---------|
| 429 | 30 s | ×2 per consecutive failure | 30 min |
| 500/502/503 | 30 s | ×2 per consecutive failure | 30 min |
| 504 | 60 s | ×2 per consecutive failure | 30 min |
| 0 (timeout, transport, upstream SSE error) | 30 s | ×2 per consecutive failure | 30 min |
| 401/403 | 30 min | 24 h at 3 failures, 7 days at 10 | 7 days |
| 402 | 30 s | 24 h at 3 failures, 7 days at 10 | 7 days |
| 404 | 7 days | — (never shortens) | 7 days |
| 400/413/422 | none (client error) | — | none |
| 400 `ModelProtocolUnsupported` | 24 h | — | 24 h |

Three properties of that table are deliberate, and each one was a bug
once:

- **A cooldown never shortens as failures accumulate.** A 404's base is
  already longer than the 24 h soft ban, so the soft ban is only applied
  when it is *longer* than the base.
- **The transient ceiling is reachable.** The old `base × errorCount` with
  the factor capped at 10 could not exceed five minutes from a 30 s base,
  so a model that never answered sat on a 5-minute bench indefinitely.
- **"No HTTP status" is transient, not fatal.** Status 0 is a transport
  error, a parse failure or an upstream SSE event; treating it as
  "probably permanent" handed three consecutive timeouts a 24-hour ban.

A timeout backs off harder than a rate limit (it is rerouted onto the
escalation curve two steps up), because a model that never answers costs
the full per-attempt timeout on every retry while a 429 costs a fast
round trip. Transient errors get up to 25% random jitter
(`cooldown_jitter`) to break thundering herds, and a 429's upstream
`Retry-After` is honored as a floor — but not on a client error, where
the provider is not asking us to back off. A client error (400/413/422)
takes no cooldown at all: a retry is byte-identical and will be rejected
identically.

## Observability

`GET /metrics` (Prometheus text format) and `GET /health`. The metrics
endpoint requires the same bearer token as the API — an unauthenticated
scrape would leak which providers are configured and how they are
behaving. Use `/health` for liveness and `/v1/airouter/state` for an
unauthenticated operational snapshot (circuit states and remaining
cooldowns); it carries no secrets.

## Repository layout

| Path | What it is |
|---|---|
| `main.go` | Bootstrap, flag parsing, route table, config watcher |
| `api.go` | Chat Completions surface, admin, health, metrics |
| `responses*.go` | The Responses API surface (`/v1/responses`, streaming, WebSocket) and the translation layer from the Chat Completions request shape |
| `router.go` | Chain construction, session stickiness, cooldowns, circuit breaker |
| `proxy.go` | Upstream request/stream handling, SSE framing, mid-stream failover, OpenCode attribution |
| `metrics.go` | Metric registry, cardinality bounding, exposition |
| `config.go` | Config schema, defaults, validation |
| `fetch-free-models.py` | Fetches, enriches, probes and vetoes the free-model list |
| `regenerate_config.py` | Deterministic `config.yaml` generation from the fetched list |
| `model_utils.py`, `model-tester.py` | Shared model utilities, ad-hoc model testing |
| `systemd/` | User units: the daemon and the model-sync service + timer |
| `scripts/` | Config validation, rules checker, mutation harness, sync, gates |
| `docs/RUNBOOK.md` | Operational runbook: metrics, cooldowns, failure modes |
| `docs/audit.md` | Audit findings, with the regression tests that pin each one |

## Handing off unpushed commits

`scripts/export-unpushed.sh` carries the commits `origin/master` does not
have out as a git bundle and a patch (generated, not committed —
committing them is self-referential), with instructions in
`dist/README.md`. The unpushed set moves — it was 37 commits one round
and 0 the next, with no edit to the commits — so never read a count from
a document; ask git (`git rev-list --count origin/master..HEAD`) or run
the gates:

```sh
./scripts/dist-freshness-check.sh --verbose   # are the artifacts current w.r.t. HEAD?
./scripts/recovery-check.sh                   # do they actually recover the tree?
```

**Do not embed a token in a remote URL to push.** `.git/config` is
plaintext, and the token then appears in every `git remote -v` and in the
process table of every push. Use a credential helper, or let git prompt.

## Development

```sh
bash scripts/gate.sh                # the full serialized gate (flock + tree fingerprint)
go test -race -count=1 ./...        # the Go suite; run it before committing
gofmt -l .                          # must print nothing
go vet ./...
python3 scripts/run-config-tests.py # config suites; asserts a collection floor
python3 scripts/mutation-check.py   # plants defects, proves the suites go red
python3 scripts/validate-config.py config.yaml
bash scripts/secret-scan.sh         # manual: scans local .git/config too, so it is not a gate stage
python3 scripts/audit-drift-check.py
```

`scripts/sse-negative-check.sh` mutates the SSE ordering logic and asserts
the tests fail, then restores it — a guard that passes both ways guards
nothing.

CI (`.github/workflows/ci.yml`) runs the Go suite, the config suites
and the mutation harness, and an end-to-end `sse-contract` job that
boots the gateway against a fake SSE upstream and asserts on the
captured bytes — the framing contract (exactly one `data: [DONE]`, no
upstream comment lines leaking) is invisible to every unit test because
a violation still returns HTTP 200.

Fuzzing (the binary must be built somewhere writable, and `-test.fuzz`
needs both a matching `-test.run` and a cache directory):

```sh
go test -c -o /tmp/fuzz/fuzz.bin . && cd /tmp/fuzz &&
  ./fuzz.bin -test.run '^FuzzExpositionRoundTrip$' -test.fuzz '^FuzzExpositionRoundTrip$' \
             -test.fuzzcachedir /tmp/fuzz/cache -test.fuzztime 60s
```
