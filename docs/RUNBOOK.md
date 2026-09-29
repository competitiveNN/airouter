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
```

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
   is applied, and the loop walks the chain. `maxAttempts = chainLen*3 + 1`
   bounds the total iterations.

### Cooldowns

Cooldowns are persisted to `cooldowns.json` (atomic tmp+rename) next to the
config file. Durations by status code:

| Status | Base cooldown | Escalation | Cap |
|--------|---------------|------------|-----|
| 429    | 30 s          | +50 % per consecutive failure | 30 min |
| 500/502/503 | 30 s      | +50 % per consecutive failure | 30 min |
| 504    | 60 s          | +50 % per consecutive failure | 30 min |
| 401/403| 30 min        | — | 30 min |
| 404    | 7 days        | — | 7 days |
| default| 30 s          | +50 % | 30 min |

`ErrorCount` is capped at 100 and reset by `RecordSuccess` (any 200 response
clears the entry). Transient errors (429/5xx) receive up to 25% random jitter
(`cooldown_jitter` in preferences) to break thundering herds. A 429's upstream
`Retry-After` header is honored as a floor on the computed cooldown.

## Metrics

Prometheus text-format metrics are exposed at `/metrics` (authenticated like
admin). Counters: `airouter_requests_total{model,status}`, `airouter_fallbacks_total`,
`airouter_cooldowns_applied_total`. The `/metrics` handler is wired into all
four request surfaces (chat completions, responses, responses-stream, and
responses-websocket) plus the admin endpoints.

## Admin endpoints

All admin endpoints require `Authorization: Bearer <key>` (the gateway API key).

| Path | Method | Description |
|------|--------|-------------|
| `/admin/providers` | GET/POST | List/update providers |
| `/admin/sessions` | GET | List active sticky sessions |
| `/admin/cooldowns` | GET/DELETE | Inspect/reset cooldowns |
| `/admin/config` | GET/POST | View/edit config (JSON); save reloads |
| `/admin/` | GET | Dashboard HTML |

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