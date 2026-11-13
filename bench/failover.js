// Failover benchmark / demo.
//
// Steady load on route `demo` (mock-primary -> mock-secondary) for 60s.
//   t=20s  POST /admin/mock/fail?provider=mock-primary&rate=1.0  (kill primary)
//   t=40s  POST /admin/mock/fail?provider=mock-primary&rate=0    (heal primary)
// Expectation: client success rate stays ~100% because the gateway falls back
// to mock-secondary, then the breaker opens and primary is skipped; after the
// heal and the breaker's open_for window, traffic returns to mock-primary.
//
// Requires the gateway to run with LLMGATE_ADMIN=1.
//
//   k6 run bench/failover.js
import http from 'k6/http';
import { check } from 'k6';
import { Counter, Rate } from 'k6/metrics';
import { BASE_URL, chatBody, chatURL, jsonHeaders } from './lib.js';

const RATE = parseInt(__ENV.RATE || '20', 10);
const MODEL = __ENV.MODEL || 'demo';
const PRIMARY = __ENV.PRIMARY || 'mock-primary';

const servedBy = new Counter('served_by');
const fellBack = new Rate('fell_back');

export const options = {
  scenarios: {
    load: {
      executor: 'constant-arrival-rate',
      exec: 'load',
      rate: RATE,
      timeUnit: '1s',
      duration: '60s',
      preAllocatedVUs: Math.max(10, RATE),
      maxVUs: Math.max(100, RATE * 4),
    },
    kill_primary: {
      executor: 'per-vu-iterations',
      exec: 'killPrimary',
      vus: 1,
      iterations: 1,
      startTime: '20s',
    },
    heal_primary: {
      executor: 'per-vu-iterations',
      exec: 'healPrimary',
      vus: 1,
      iterations: 1,
      startTime: '40s',
    },
  },
  thresholds: {
    // Client-visible success must stay ~100% across the outage.
    'checks{scenario:load}': ['rate>0.99'],
    'http_req_failed{scenario:load}': ['rate<0.01'],
    // Admin calls must succeed, otherwise the test proves nothing.
    'checks{scenario:kill_primary}': ['rate==1'],
    'checks{scenario:heal_primary}': ['rate==1'],
  },
};

const body = chatBody(MODEL, false);

export function load() {
  const res = http.post(chatURL(), body, { headers: jsonHeaders, tags: { name: 'chat' } });
  const provider = res.headers['X-Llmgate-Provider'] || 'none';
  const attempts = parseInt(res.headers['X-Llmgate-Attempts'] || '1', 10);
  servedBy.add(1, { provider: provider });
  fellBack.add(attempts > 1 || provider !== PRIMARY);
  check(res, { 'status 200': (r) => r.status === 200 });
}

function setFailRate(rate) {
  const url = `${BASE_URL}/admin/mock/fail?provider=${encodeURIComponent(PRIMARY)}&rate=${rate}`;
  const res = http.post(url, null, { headers: jsonHeaders, tags: { name: 'admin_mock_fail' } });
  check(res, { 'admin 2xx': (r) => r.status >= 200 && r.status < 300 });
  console.log(`admin: ${PRIMARY} fail_rate=${rate} -> HTTP ${res.status}`);
}

export function killPrimary() {
  setFailRate('1.0');
}

export function healPrimary() {
  setFailRate('0');
}
