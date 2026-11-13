# llmgate

![Go](https://img.shields.io/badge/go-1.27-00ADD8?logo=go)

A small, dependency-light **LLM gateway** written in Go. One OpenAI-compatible
endpoint in front of Anthropic, OpenAI and Ollama, with provider fallback,
circuit breakers, per-key budgets and rate limits (Redis), real SSE streaming
and Prometheus metrics.

## Why

Apps that call LLM APIs directly inherit every provider outage, rate limit and
surprise bill. A gateway centralises the cross-cutting concerns: switch
providers without touching clients, survive a provider being down, cap spend
per API key, and see tokens, cost and latency per provider in one place.

## Architecture

```
                         +-----------------------------------------------+
  client                 |                    llmgate                    |
  (OpenAI SDK, curl) --> |  auth -> rate limit -> budget check           |
   Bearer <key>          |             |                                 |
                         |         router (route alias -> chain)         |        +-----------+
                         |   +---------+---------+---------+             | -----> | Anthropic |  (Messages API,
                         |   | breaker | breaker | breaker |             |        +-----------+   translated)
                         |   +----+----+----+----+----+----+             |        +-----------+
                         |        v         v         v                  | -----> |  OpenAI   |  (passthrough)
                         |   anthropic    openai    ollama / mock        |        +-----------+
                         |                                               |        +-----------+
                         |  usage + cost --> Redis (Lua, atomic)         | -----> |  Ollama   |  (passthrough)
                         |  /metrics ------> Prometheus --> Grafana      |        +-----------+
                         +-----------------------------------------------+
                                   |                      ^
                                   v                      |
                                 Redis  <-----------------+  rate-limit + daily budget counters
```

## Quickstart (no API keys needed)

```sh
make up        # gateway + Redis + Prometheus + Grafana (docker or podman compose)
```

Gateway on `:8080`, Grafana on `:3000` (dashboard is the home page),
Prometheus on `:9090`. The demo key is `demo-key`.

```sh
# Non-streaming. The `demo` route is mock-primary -> mock-secondary.
curl -si localhost:8080/v1/chat/completions \
  -H 'Authorization: Bearer demo-key' -H 'Content-Type: application/json' \
  -d '{"model":"demo","messages":[{"role":"user","content":"hello"}]}'

# Streaming (SSE, one flush per chunk, ends with data: [DONE])
curl -sN localhost:8080/v1/chat/completions \
  -H 'Authorization: Bearer demo-key' -H 'Content-Type: application/json' \
  -d '{"model":"demo","stream":true,"messages":[{"role":"user","content":"tell me a story"}]}'

# Kill the primary and watch the fallback (X-LLMGate-Provider flips, Attempts = 2)
curl -XPOST 'localhost:8080/admin/mock/fail?provider=mock-primary&rate=1.0'
curl -si localhost:8080/v1/chat/completions -H 'Authorization: Bearer demo-key' \
  -d '{"model":"demo","messages":[{"role":"user","content":"hi"}]}' | grep -i x-llmgate
curl -XPOST 'localhost:8080/admin/mock/fail?provider=mock-primary&rate=0'   # heal

make down
```

To use real providers export `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` before
`make up` and call the `default` route. Run without Docker: start a Redis, then
`make run` (uses `config.example.yaml`, key `demo-key`, admin enabled).

Response headers: `X-LLMGate-Provider` (who answered), `X-LLMGate-Attempts`
(real upstream calls), `X-Request-ID` (echoed or generated).

## Configuration

See [`config.example.yaml`](config.example.yaml). Path from `-config`,
`$LLMGATE_CONFIG` or `config.yaml`.

| Key | Meaning |
|---|---|
| `listen`, `redis_url` | Bind address, Redis URL. Overridden by `LLMGATE_LISTEN`, `LLMGATE_REDIS_URL`. |
| `request_timeout` | Per upstream attempt. For streams it bounds time-to-first-chunk only. |
| `routes.<alias>` | Ordered chain of `{provider, model}`. Clients send the alias as `model`. |
| `providers.<name>` | Named instance with `type: openai\|anthropic\|ollama\|mock`, `base_url`, `api_key_env`. Several instances of one type are fine. Mock adds `latency`, `chunk_delay`, `fail_rate`. |
| `circuit_breaker` | `failure_threshold` consecutive failures open the breaker for `open_for`. |
| `pricing` | Optional USD-per-1M-token overrides of the built-in table. Unknown models cost 0. |
| `keys[]` | `name`, secret via `key_env`, `tokens_per_day`, `usd_per_day`, `rpm` (0 = unlimited). |

`LLMGATE_ADMIN=1` enables `POST /admin/mock/fail?provider=<instance>&rate=<0..1>`
(unauthenticated, demo only). `llmgate healthcheck` probes `/healthz` for
container health checks.

### Endpoints

`POST /v1/chat/completions`, `GET /v1/models`, `GET /healthz`, `GET /readyz`
(Redis reachable), `GET /metrics`.
Errors use the OpenAI shape `{"error":{"message","type","code"}}`:
401 bad key, 429 `rate_limited` / `budget_exceeded` with `Retry-After`,
404 unknown model alias, 502 all providers failed, 504 timeout.

## Fallback semantics

| Situation | Behaviour |
|---|---|
| Connection error, timeout, HTTP 429, HTTP 5xx | Try the next provider in the chain. Counts toward the breaker. |
| Other 4xx (400, 404, 422...) | Returned to the client, **no fallback**; the next provider would reject it too. Does not count toward the breaker. |
| Upstream 401/403 | Our credentials are wrong: surfaced as 502, no fallback. |
| Breaker open | Provider skipped (`skipped_open`), not an attempt. After `open_for` one probe request goes through (half-open); success closes, failure re-opens. |
| Streaming, before first chunk | Full fallback, including errors that arrive as the first SSE event. |
| Streaming, after first chunk | **No fallback.** An error event is sent, the stream is closed without `[DONE]`, the failure is recorded. |
| Client disconnects | Request context cancels the upstream call. Not counted against the provider; usage so far is still billed. |
| Everything failed | 502, or 504 if the last failure was a timeout. |

Budget is checked before the call (reject if already over) and charged after
with real token counts. For streams the final usage event is used, otherwise
`chars/4` is estimated.

## Metrics

| Metric | Type | Labels |
|---|---|---|
| `llmgate_requests_total` | counter | `key, route, status` (`ok\|client_error\|budget_exceeded\|rate_limited\|upstream_error`) |
| `llmgate_upstream_requests_total` | counter | `provider, model, outcome` (`ok\|error\|timeout\|skipped_open`) |
| `llmgate_request_duration_seconds` | histogram | `route, stream` (total, including fallbacks) |
| `llmgate_upstream_duration_seconds` | histogram | `provider, model` |
| `llmgate_time_to_first_byte_seconds` | histogram | `provider` (streaming) |
| `llmgate_tokens_total` | counter | `key, provider, model, direction` (`input\|output`) |
| `llmgate_cost_usd_total` | counter | `key, provider, model` |
| `llmgate_fallbacks_total` | counter | `from_provider, to_provider` |
| `llmgate_circuit_state` | gauge | `provider` (0 closed, 1 half-open, 2 open) |

Logs are one JSON line per request via `log/slog` (request id, key, route,
provider, attempts, tokens, cost, duration).

## Design decisions and trade-offs

- **Standard library first.** `net/http` mux (Go 1.22 patterns), `log/slog`,
  `context`. Only go-redis, client_golang and yaml.v3 are dependencies. The
  circuit breaker is hand-written (about 100 lines, injectable clock, tested
  with `-race`).
- **State lives in Redis, not in the process.** Rate limit and budget are Lua
  scripts, so N gateway replicas share exact counters and a check-and-increment
  can never interleave. Keys carry a `{key}` hash tag (Redis Cluster safe) and
  daily keys expire after 48h. Cost is stored as integer micro-USD.
- **Rate limit: sliding-window counter.** `prev * (1 - elapsed) + current` over
  two one-minute buckets. It avoids the 2x burst of a fixed window at minute
  edges for O(1) memory, at the cost of being an approximation (a sliding log
  would be exact but O(requests)).
- **Budget is check-then-charge.** Concurrent in-flight requests can overshoot a
  limit by their own cost. A reserve/refund scheme would be exact but needs
  max-token estimates; not worth it for v0.1.
- **Fail open on Redis errors.** If Redis is down limits are not enforced and
  the error is logged loudly; `/readyz` goes unhealthy so orchestrators can act.
  Choose fail-closed if spend control matters more than availability.
- **Streaming primes the first chunk.** The router reads the first upstream
  chunk before the HTTP response starts. That makes "fallback only before the
  first byte" a structural property rather than a flag, and it measures real
  time-to-first-token. The Anthropic translator delays the role chunk until the
  first content so an early `overloaded_error` event still triggers fallback.
- **One adapter for OpenAI and Ollama** (both speak `/v1/chat/completions`);
  Anthropic Messages events are translated to `chat.completion.chunk`.
  Text-only: tools and images are not translated yet.
- **Timeouts without cutting streams.** No server `WriteTimeout`; the per-attempt
  timer stops at the first chunk.
- **Request-scoped quotas, not accounts.** Keys come from config; no key
  management API (intentional scope).

## Benchmarks

Results come from the k6 scripts in [`bench/`](bench/) (see its README for how
to run them).

**Failover** (`bench/failover.js`, `demo` route, mock providers): steady load;
at t=20s the primary is forced to fail 100%, at t=40s it is healed.

| Machine | Requests | Success | Fallbacks | p95 latency |
|---|---|---|---|---|
| Ryzen 7 5700X, 16 threads, podman compose | 1203 | 100% | 409 (mock-primary -> mock-secondary) | 151 ms |

Mock latencies are 80 ms (primary) and 150 ms (secondary), so p95 reflects the
secondary answering during the outage. Proxy overhead and streaming TTFB
(`overhead.js`, `stream.js`) are still to be measured.

## Development

```sh
make test    # go test -race -cover ./...   (Redis is faked with miniredis)
make lint    # gofmt, go vet, staticcheck
make build   # bin/llmgate
```

## Roadmap

- **v0.2** semantic cache (pgvector) keyed on embedding similarity, with
  per-route TTL and cache-hit metrics.
- **v0.2** PII redaction on requests (and optionally responses) before they
  leave the gateway.
- Tool calls and image parts in the Anthropic translation.
- Budget reservation for exact enforcement under concurrency.
- Key management API and an admin UI.
