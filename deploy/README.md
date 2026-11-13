# llmgate demo stack

`docker-compose.yml` runs the gateway with Redis, Prometheus and Grafana.
It works with both `docker compose` and `podman compose`.

| service    | port | notes                                              |
|------------|------|----------------------------------------------------|
| llmgate    | 8080 | config `deploy/config.demo.yaml`, admin endpoints on |
| redis      | 6379 | budget and rate-limit state, no persistence         |
| prometheus | 9090 | scrapes `llmgate:8080/metrics` every 5s             |
| grafana    | 3000 | anonymous Viewer, `llmgate` dashboard is home       |

Grafana admin login is `admin` / `admin` (override with `GRAFANA_ADMIN_PASSWORD`).
Real providers are optional: export `ANTHROPIC_API_KEY` / `OPENAI_API_KEY`
before `make up` and the `default` route uses them. Without keys it falls
through to the mock.

## Start and stop

```sh
make up      # or: docker compose -f deploy/docker-compose.yml up -d --build
make down    # or: docker compose -f deploy/docker-compose.yml down
```

Replace `docker` with `podman` if that is your runtime.

Smoke test:

```sh
curl -s localhost:8080/v1/chat/completions \
  -H 'Authorization: Bearer demo-key' -H 'Content-Type: application/json' \
  -d '{"model":"demo","messages":[{"role":"user","content":"hello"}]}' -i
```

## Demo script (failover GIF)

About 90 seconds of footage. Screen layout: Grafana on the left, terminal on
the right.

1. `make up`, wait until `docker compose -f deploy/docker-compose.yml ps`
   shows all services healthy.
2. Open http://localhost:3000. The `llmgate` dashboard loads as home with no
   login. Set the time range to "Last 5 minutes"; refresh is 5s.
3. In the terminal run:
   ```sh
   k6 run bench/failover.js
   # or: podman run --rm -i --network host -v "$PWD/bench:/bench:ro,z" \
   #       docker.io/grafana/k6:0.54.0 run /bench/failover.js
   ```
4. Start recording. What to point at:
   - t=0-20s: "Request rate by status" all green, latency p50 near 80ms
     (served by `mock-primary`).
   - t=20s: k6 logs `admin: mock-primary fail_rate=1.0`.
     "Fallbacks / s" jumps (`mock-primary -> mock-secondary`), latency steps
     up to about 150ms, and "Circuit breaker state" turns `mock-primary` red
     (open) after 5 failures. Request rate stays green: no client errors.
   - t=40s: k6 logs `fail_rate=0`. After `open_for` (10s in the demo config)
     the breaker goes half-open (yellow), then closed (green), and latency
     drops back to about 80ms.
5. k6 ends with `checks{scenario:load}` above 99%: the outage was invisible
   to clients.

Manual kill switch, without k6:

```sh
curl -X POST 'localhost:8080/admin/mock/fail?provider=mock-primary&rate=1.0'
curl -X POST 'localhost:8080/admin/mock/fail?provider=mock-primary&rate=0'
```

## Files

- `config.demo.yaml`: routes `demo`, `bench-zero`, `default`; mock instances.
- `prometheus/prometheus.yml`: scrape config.
- `grafana/provisioning/`: Prometheus datasource (uid `prometheus`) and the
  file dashboard provider.
- `grafana/dashboards/llmgate.json`: the dashboard. It uses the metric names
  from `SPEC.md` exactly.
