# llmgate — MVP spec (shared contract)

Single Go binary. Proxy in front of LLM providers. OpenAI-compatible API in, many providers out.

## Scope MVP (v0.1)
1. Provider routing with fallback + per-provider circuit breaker.
2. Per-API-key budget (tokens/day and USD/day) + request rate limit, state in Redis.
3. SSE streaming passthrough (no full-response buffering), including fallback only *before* first byte is sent.
4. Prometheus metrics + structured JSON logs (slog).

Out of scope v0.1: semantic cache, PII redaction (v0.2), admin UI.

## Layout
```
cmd/llmgate/main.go
internal/config      # YAML config + env override
internal/api         # HTTP handlers (net/http, Go 1.22+ mux)
internal/provider    # Provider interface + openai, anthropic, ollama, mock adapters
internal/router      # fallback chain + circuit breaker
internal/budget      # Redis budget + rate limit (Lua scripts, atomic)
internal/pricing     # USD cost per model per 1M tokens
internal/metrics     # Prometheus collectors
deploy/              # docker-compose, prometheus.yml, grafana provisioning + dashboard JSON
bench/               # k6 scripts
config.example.yaml
Dockerfile
Makefile
README.md
```

## HTTP API
- `POST /v1/chat/completions` — OpenAI request/response format. `stream: true` → SSE `data: {...}\n\n` chunks ending `data: [DONE]`.
- `GET /v1/models` — models from config.
- `GET /healthz` (liveness), `GET /readyz` (Redis reachable).
- `GET /metrics` — Prometheus.
- Auth: `Authorization: Bearer <gateway key>`. Keys defined in config, each with `name`, `tokens_per_day`, `usd_per_day`, `rpm`.
- Response headers: `X-LLMGate-Provider`, `X-LLMGate-Attempts`, `X-Request-ID`.
- `llmgate healthcheck` subcommand: GET `http://127.0.0.1:<listen port>/healthz`, exit 0/1 (container HEALTHCHECK in distroless).
- Errors in OpenAI error JSON shape: `{"error":{"message","type","code"}}`. 401 bad key, 429 budget/rate (`type: "budget_exceeded"` or `"rate_limited"`, with `Retry-After`), 502 all providers failed, 504 timeout.

## Config (config.example.yaml)
```yaml
listen: ":8080"
redis_url: "redis://localhost:6379/0"
request_timeout: 60s     # per upstream attempt (for streams: time to first chunk)
routes:                  # model alias -> ordered fallback chain; `provider` = instance name
  default:
    - {provider: anthropic, model: claude-haiku-5-5}
    - {provider: openai, model: gpt-4o-mini}
    - {provider: ollama, model: llama3.2}
  demo:
    - {provider: mock-primary, model: mock-echo}
    - {provider: mock-secondary, model: mock-echo}
providers:               # key = instance name, `type` = mock|anthropic|openai|ollama (defaults to the key)
  anthropic:      {type: anthropic, base_url: "https://api.anthropic.com", api_key_env: ANTHROPIC_API_KEY}
  openai:         {type: openai, base_url: "https://api.openai.com", api_key_env: OPENAI_API_KEY}
  ollama:         {type: ollama, base_url: "http://localhost:11434"}
  mock-primary:   {type: mock, latency: 50ms, chunk_delay: 30ms, fail_rate: 0.0}
  mock-secondary: {type: mock, latency: 120ms, fail_rate: 0.0}
circuit_breaker: {failure_threshold: 5, open_for: 30s}
pricing:                 # optional overrides, USD per 1M tokens; unknown models cost 0
  gpt-4o-mini: {input_per_1m: 0.15, output_per_1m: 0.60}
keys:                    # limit 0 = unlimited; secret from key_env (or inline `key`)
  - {name: demo, key_env: LLMGATE_DEMO_KEY, tokens_per_day: 200000, usd_per_day: 2.0, rpm: 60}
```
Env override: `LLMGATE_LISTEN`, `LLMGATE_REDIS_URL`, `LLMGATE_CONFIG` (path, default `config.yaml`), `LLMGATE_ADMIN=1`.
A provider whose api key env is empty stays registered but every call fails retryably (503), so chains fall through to the next hop.
Mock accepts any model name. Requests use the route alias as `model`; unknown alias -> 404 `model_not_found`.

## Retry / fallback rules
- Retryable → try next in chain: connection error, timeout, HTTP 429, 5xx. Not retryable: other 4xx (return to client as-is mapped to OpenAI error).
- Breaker open → skip provider (metric `skipped_open`; not a failure and not counted in `X-LLMGate-Attempts`, which is the number of real upstream calls).
- Upstream 401/403 are not retryable and are surfaced as 502 (gateway credential problem, not the client's).
- Per-key rate limit = sliding-window counter (previous + current minute, weighted), atomic in one Lua script. Budgets = UTC-day counters with 48h TTL; cost stored as integer micro-USD. If Redis errors, limits fail open (logged).
- Streaming: once first chunk is written to client, no fallback; on mid-stream error, close stream and record error.
- Budget check before call (reject if already over). Usage recorded after call with real token counts (from provider usage; for streaming, from final usage event, else estimate chars/4).

## Metrics (names are contract — Grafana dashboard depends on them)
- `llmgate_requests_total{key,route,status}` counter (status = ok|client_error|budget_exceeded|rate_limited|upstream_error)
- `llmgate_upstream_requests_total{provider,model,outcome}` counter (outcome = ok|error|timeout|skipped_open)
- `llmgate_request_duration_seconds{route,stream}` histogram (total incl. fallbacks)
- `llmgate_upstream_duration_seconds{provider,model}` histogram
- `llmgate_time_to_first_byte_seconds{provider}` histogram (streaming)
- `llmgate_tokens_total{key,provider,model,direction}` counter (direction = input|output)
- `llmgate_cost_usd_total{key,provider,model}` counter
- `llmgate_fallbacks_total{from_provider,to_provider}` counter
- `llmgate_circuit_state{provider}` gauge (0 closed, 1 half-open, 2 open)

## Mock provider
Required for tests, demo and benchmark without API keys. Deterministic echo response, configurable latency and fail_rate, supports streaming (word per chunk). Also admin toggle `POST /admin/mock/fail?provider=<instance>&rate=<0..1>` (no auth; `provider` omitted = all mock instances; 200 on success, 404 unknown instance, 400 bad rate), registered only when `LLMGATE_ADMIN=1` — used for demo "kill primary provider, watch fallback".

## Ports (docker-compose)
llmgate 8080, redis 6379, prometheus 9090, grafana 3000 (anonymous viewer, dashboard provisioned as home).
