// Non-streaming overhead benchmark at a constant arrival rate.
//
// Default route `bench-zero` points at a mock with latency 0s, so observed
// latency is (almost) pure gateway overhead: auth, Redis budget/rate-limit
// scripts, routing, metrics, JSON encode/decode, loopback network.
//
//   k6 run bench/overhead.js
//   k6 run -e RATE=500 -e DURATION=60s -e MODEL=demo bench/overhead.js
import http from 'k6/http';
import { check } from 'k6';
import { chatBody, chatURL, jsonHeaders } from './lib.js';

const RATE = parseInt(__ENV.RATE || '200', 10); // requests per second
const DURATION = __ENV.DURATION || '60s';
const MODEL = __ENV.MODEL || 'bench-zero';
const P99_MS = __ENV.P99_MS || '50'; // threshold on p99 latency, ms

export const options = {
  discardResponseBodies: false,
  scenarios: {
    overhead: {
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: '1s',
      duration: DURATION,
      preAllocatedVUs: Math.max(20, Math.ceil(RATE / 10)),
      maxVUs: Math.max(200, RATE * 2),
    },
  },
  thresholds: {
    http_req_failed: ['rate<0.001'],
    http_req_duration: [`p(99)<${P99_MS}`],
    checks: ['rate>0.999'],
    dropped_iterations: ['count<10'],
  },
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
};

const body = chatBody(MODEL, false);

export default function () {
  const res = http.post(chatURL(), body, { headers: jsonHeaders, tags: { name: 'chat' } });
  check(res, {
    'status 200': (r) => r.status === 200,
    'has choices': (r) => {
      try {
        return r.json('choices.0.message.content') !== undefined;
      } catch (_) {
        return false;
      }
    },
    'has provider header': (r) => !!r.headers['X-Llmgate-Provider'],
  });
}
