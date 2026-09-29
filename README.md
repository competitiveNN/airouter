# airouter

An OpenAI-compatible gateway that exposes four logical models — `smart`, `work`,
`fast`, `large` — over any number of upstream providers.

Each logical model is a **fallback chain**: a list of provider/model endpoints
tried in order. The gateway is built to be invisible to the client, so:

- **Sessions are sticky.** The same session is always routed to the same model.
- **Errors are absorbed, not returned.** A 429, 5xx, or mid-stream failure moves
  the session to the next endpoint in the chain and continues. The client sees a
  slight delay, never an error.
- **Cooldowns are error-graded.** A model that errors gets a cooldown whose length
  depends on the error: a 429 is short, a 404 is long, because the two mean very
  different things about the upstream.
- **Failures are caught mid-stream too.** If the upstream dies after the first
  chunk, the gateway rewinds and resumes on the next endpoint, so the client
  receives one coherent stream.

## Running it

```sh
go build -o airouter .
./airouter -config config.yaml -port 8080 -api-key "$AIROUTER_API_KEY"
```

The `-api-key` is the bearer token clients present. Provider credentials come
from the environment, named by `api_key_env` in the config — secrets are never
written into the config file.

Validate a config before restarting onto it:

```sh
python3 scripts/validate-config.py config.yaml
```

## Configuration

Top-level keys are `providers` and `models`; everything else is optional tuning
under `preferences`.

### `providers`

```yaml
providers:
  openrouter:
    url: https://openrouter.ai/api/v1
    api_key_env: OPENROUTER_API_KEY
    headers:            # optional, sent verbatim upstream
      X-Custom: value
```

A provider named `opencode`, `opencode-go` or `opencode-zen` is treated as an
OpenCode gateway and gets client-attribution headers automatically, without
which the upstream rejects free-tier models from non-OpenCode clients.

### `models`

```yaml
models:
  smart:
    chain:
      - provider: openrouter
        model: anthropic/claude-sonnet-4.5
      - provider: openai
        model: gpt-5
```

The chain is tried in order. `smart` should hold the most intelligent model
available, `work` the coding workhorse, `fast` something small for quick or
verbose-but-simple work, and `large` something with a very large context window
for big inputs and context compression.

Endpoints are tried in chain order unless session rotation is enabled (below).
The router places models in the chain by intelligence score where it can, so the
declared order is a fallback rather than the whole routing policy.

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

Every default in the table is the value used when the key is absent. For some
keys a written `0` is deliberately distinguishable from an omitted key:
`circuit_breaker_threshold: 0` disables the circuit breaker, where omitting it
would use the default of 5.

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

`docs/RUNBOOK.md` ("Label cardinality is bounded") covers what to watch, and
`docs/audit.md` records the bugs found along the way — including three that a
green test suite did not catch.

## Observability

`GET /metrics` (Prometheus text format) and `GET /health`. The metrics endpoint
requires the same bearer token as the API — an unauthenticated scrape would leak
which providers are configured and how they are behaving.

## Repository layout

| Path | What it is |
|---|---|
| `main.go` | Bootstrap, flag parsing, route table |
| `api.go` | Chat Completions surface, admin, health, metrics |
| `responses*.go` | The Open Responses surface (`/v1/responses`, streaming, WebSocket) and the translation layer from the Chat Completions request shape |
| `router.go` | Chain construction, session stickiness, cooldowns, circuit breaker |
| `proxy.go` | Upstream request/stream handling, SSE framing and mid-stream failover |
| `metrics.go` | Metric registry, cardinality bounding, exposition |
| `config.go` | Config schema, defaults, validation |
| `scripts/` | Config validation, SSE contract checks, secret scan, audit drift check |
| `docs/RUNBOOK.md` | Operational runbook: metrics, cooldowns, failure modes |
| `docs/audit.md` | Audit findings, with the regression tests that pin each one |

## Unpushed commits

The local `master` is **13 commits ahead of `origin/master`**. The token in use
authenticates but has `push: false`, so those commits are not on the remote.
`dist/` carries them as both a git bundle and a patch, with instructions in
`dist/README.md`. Both were verified by applying them to `origin/master` in a
fresh clone and running the suite.

## Development

```sh
go test -race -count=1 ./...   # the full suite; run it before committing
gofmt -l .                     # must print nothing
go vet ./...
python3 scripts/validate-config.py config.yaml
bash scripts/secret-scan.sh
python3 scripts/audit-drift-check.py
```

`scripts/sse-negative-check.sh` mutates the SSE ordering logic and asserts the
tests fail, then restores it — a guard that passes both ways guards nothing.

Fuzzing (the binary must be built somewhere writable, and `-test.fuzz` needs both
a matching `-test.run` and a cache directory):

```sh
go test -c -o /tmp/fuzz/fuzz.bin . && cd /tmp/fuzz &&
  ./fuzz.bin -test.run '^FuzzExpositionRoundTrip$' -test.fuzz '^FuzzExpositionRoundTrip$' \
             -test.fuzzcachedir /tmp/fuzz/cache -test.fuzztime 60s
```
