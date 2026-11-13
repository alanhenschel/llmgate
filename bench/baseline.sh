#!/usr/bin/env bash
# Proxy overhead baseline.
#
# The mock provider runs inside the gateway process, so there is no separate
# mock endpoint to hit "directly". Overhead is measured two ways instead:
#
#  1. Client side: N sequential requests to route `bench-zero` (mock latency 0s)
#     and to route `demo` (mock-primary latency 80ms). For `demo`,
#     overhead ~= observed - 80ms. For `bench-zero`, observed is overhead.
#  2. Server side: delta of /metrics histograms before/after the run:
#     mean(llmgate_request_duration_seconds) - mean(llmgate_upstream_duration_seconds)
#     = time spent in the gateway outside the upstream call (auth, Redis
#     budget + rate limit, routing, metrics, encoding).
#
# Usage: bench/baseline.sh [N]
# Env:   BASE_URL (default http://localhost:8080), API_KEY (default demo-key)
set -euo pipefail

N="${1:-200}"
BASE_URL="${BASE_URL:-http://localhost:8080}"
API_KEY="${API_KEY:-demo-key}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

body() {
  printf '{"model":"%s","stream":false,"max_tokens":64,"messages":[{"role":"user","content":"Explain in one short paragraph what an LLM gateway does."}]}' "$1"
}

# sum_count <metrics file> <metric base name> <label filter regex>
# Prints "<sum> <count>" summed across all series matching the filter.
sum_count() {
  awk -v m="$2" -v f="$3" '
    $1 ~ "^"m"_sum" && $1 ~ f { s += $NF }
    $1 ~ "^"m"_count" && $1 ~ f { c += $NF }
    END { printf "%.9f %d\n", s, c }' "$1"
}

stats_ms() {
  # stdin: one latency per line, seconds. Prints avg p50 p95 p99 in ms.
  sort -n | awk '{ a[NR] = $1 * 1000; s += $1 * 1000 }
    END {
      if (NR == 0) { print "no data"; exit }
      i50 = int(NR * 0.50); if (i50 < 1) i50 = 1
      i95 = int(NR * 0.95); if (i95 < 1) i95 = 1
      i99 = int(NR * 0.99); if (i99 < 1) i99 = 1
      printf "avg=%.2fms p50=%.2fms p95=%.2fms p99=%.2fms n=%d\n", s / NR, a[i50], a[i95], a[i99], NR
    }'
}

run_route() {
  local route="$1" mock_ms="$2"
  local before="$TMP/before_$route" after="$TMP/after_$route" lat="$TMP/lat_$route"
  : > "$lat"
  local payload; payload="$(body "$route")"
  # Warm-up (connections, Redis scripts loaded).
  for _ in 1 2 3 4 5; do
    curl -fsS -o /dev/null -H "Authorization: Bearer $API_KEY" -H 'Content-Type: application/json' \
      -d "$payload" "$BASE_URL/v1/chat/completions"
  done
  curl -fsS "$BASE_URL/metrics" > "$before"
  for _ in $(seq "$N"); do
    curl -sS -o /dev/null -w '%{time_total}\n' -H "Authorization: Bearer $API_KEY" \
      -H 'Content-Type: application/json' -d "$payload" "$BASE_URL/v1/chat/completions" >> "$lat"
  done
  curl -fsS "$BASE_URL/metrics" > "$after"

  echo "== route=$route (configured mock latency ${mock_ms}ms), N=$N sequential =="
  printf 'client:   %s' "$(stats_ms < "$lat")"
  echo
  echo "client overhead vs mock setting: subtract ${mock_ms}ms from the values above"

  read -r rs0 rc0 < <(sum_count "$before" llmgate_request_duration_seconds "route=\"$route\"")
  read -r rs1 rc1 < <(sum_count "$after"  llmgate_request_duration_seconds "route=\"$route\"")
  read -r us0 uc0 < <(sum_count "$before" llmgate_upstream_duration_seconds "provider=\"mock")
  read -r us1 uc1 < <(sum_count "$after"  llmgate_upstream_duration_seconds "provider=\"mock")
  awk -v rs="$(echo "$rs1 - $rs0" | bc -l)" -v rc="$((rc1 - rc0))" \
      -v us="$(echo "$us1 - $us0" | bc -l)" -v uc="$((uc1 - uc0))" 'BEGIN {
    if (rc == 0 || uc == 0) { print "server:   no histogram delta (metrics missing?)"; exit }
    r = rs / rc * 1000; u = us / uc * 1000
    printf "server:   mean request=%.3fms mean upstream=%.3fms gateway overhead=%.3fms (n=%d)\n", r, u, r - u, rc
  }'
  echo
}

command -v bc >/dev/null || { echo "bc is required" >&2; exit 1; }
curl -fsS "$BASE_URL/healthz" >/dev/null || { echo "gateway not reachable at $BASE_URL" >&2; exit 1; }

run_route bench-zero 0
run_route demo 80
