# llmgate benchmarks

k6 scripts that measure the gateway itself, using the built-in mock provider
so no API keys or network egress are needed. Start the stack first:

```sh
make up            # or: docker compose -f deploy/docker-compose.yml up -d --build
```

The demo config (`deploy/config.demo.yaml`) defines the routes used here:

| route        | chain                                        | purpose                          |
|--------------|----------------------------------------------|----------------------------------|
| `bench-zero` | `mock-zero` (latency 0s)                      | pure gateway overhead            |
| `demo`       | `mock-primary` (80ms) -> `mock-secondary` (150ms) | streaming, failover demo     |

The demo key is `demo-key` (env `LLMGATE_DEMO_KEY`), with limits high enough
that the budget does not throttle the benchmark.

## Running

With a local k6 binary:

```sh
k6 run bench/overhead.js
k6 run bench/stream.js
k6 run bench/failover.js
```

With a container (no install). `--network host` lets k6 reach `localhost:8080`:

```sh
podman run --rm -i --network host -v "$PWD/bench:/bench:ro,z" \
  docker.io/grafana/k6:0.54.0 run /bench/overhead.js
# docker: same command with `docker run`.
```

Or join the compose network and target the service name:

```sh
podman run --rm -i --network llmgate_default -e BASE_URL=http://llmgate:8080 \
  -v "$PWD/bench:/bench:ro,z" docker.io/grafana/k6:0.54.0 run /bench/failover.js
```

Common env vars (pass with `-e NAME=value`):

| var        | default                 | used by                 |
|------------|-------------------------|-------------------------|
| `BASE_URL` | `http://localhost:8080` | all                     |
| `API_KEY`  | `demo-key`              | all                     |
| `MODEL`    | per script              | all                     |
| `RATE`     | 200 / 50 / 20 req/s     | overhead / stream / failover |
| `DURATION` | `60s`                   | overhead, stream        |
| `P99_MS`   | `50`                    | overhead threshold      |
| `TTFB_P95_MS` | `250`                | stream threshold        |
| `PRIMARY`  | `mock-primary`          | failover                |

## What each script measures

### `overhead.js` - non-streaming proxy overhead

Constant arrival rate (open model, so latency does not throttle load) of
non-streaming requests to `bench-zero`. The mock answers instantly, so the
reported `http_req_duration` is gateway overhead: auth, Redis budget and rate
limit scripts, routing, metrics, JSON handling and loopback networking.
Threshold: `p(99) < P99_MS`, error rate < 0.1%.

Run it with `-e MODEL=demo` to see the same numbers with an 80ms upstream.

### `stream.js` - streaming TTFB

`stream: true` against `demo`. k6 buffers SSE bodies, so TTFB is taken from
`res.timings.waiting` (request sent until first response byte). The gateway
flushes headers with the first chunk, so this is the client-visible TTFB.
Custom metrics: `sse_ttfb`, `sse_total` (time to `data: [DONE]`),
`sse_chunks`, `sse_done_seen`. Compare `sse_ttfb` with the mock's 80ms
latency to get the streaming overhead.

### `failover.js` - resilience under provider failure

60s of steady load on `demo`:

- t=20s: `POST /admin/mock/fail?provider=mock-primary&rate=1.0` kills the primary.
- t=40s: `POST /admin/mock/fail?provider=mock-primary&rate=0` heals it.

Pass criteria: client success rate > 99% for the whole run, because the
gateway falls back to `mock-secondary` and the circuit breaker then skips the
primary. Custom metrics: `served_by{provider}` and `fell_back`. In Grafana,
watch "Fallbacks / s", "Circuit breaker state" and the p99 latency step from
~80ms to ~150ms and back. Needs `LLMGATE_ADMIN=1` (set in the compose file).

### `baseline.sh` - overhead methodology

The mock lives inside the gateway process, so there is no standalone mock
endpoint to call "directly". `baseline.sh [N]` measures overhead two ways:

1. Client side: N sequential `curl` requests on `bench-zero` (latency 0s,
   observed time is overhead) and on `demo` (subtract the configured 80ms).
2. Server side: delta of `/metrics` histograms over the run,
   `mean(llmgate_request_duration_seconds) - mean(llmgate_upstream_duration_seconds)`,
   which is the time spent in the gateway outside the upstream call.

Requires `curl`, `awk` and `bc`.

## Results

Fill in after running. Record hardware, OS and whether Redis ran in a
container on the same host.

Environment: _CPU / RAM / OS / container runtime_

| benchmark            | rate (req/s) | p50 | p95 | p99 | errors | notes |
|----------------------|--------------|-----|-----|-----|--------|-------|
| overhead (`bench-zero`) |           |     |     |     |        |       |
| overhead (`demo`, 80ms mock) |      |     |     |     |        |       |
| stream TTFB (`demo`) |              |     |     |     |        |       |
| failover (`demo`)    |              |     |     |     |        | success rate: |
| baseline.sh server-side overhead | sequential | mean: | | | | |
