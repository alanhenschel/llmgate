// Streaming (SSE) benchmark: measures time to first byte (TTFB) and total
// stream duration through the gateway.
//
// k6 buffers the whole response body, but `timings.waiting` is the time from
// request sent until the first response byte arrives. Because the gateway
// flushes headers + the first SSE chunk as soon as the upstream produces it,
// `waiting` is the client-side TTFB. `duration` is time to the last byte
// (`data: [DONE]`).
//
//   k6 run bench/stream.js
//   k6 run -e RATE=50 -e MODEL=demo bench/stream.js
import http from 'k6/http';
import { check } from 'k6';
import { Trend, Rate } from 'k6/metrics';
import { chatBody, chatURL, jsonHeaders } from './lib.js';

const RATE = parseInt(__ENV.RATE || '50', 10);
const DURATION = __ENV.DURATION || '60s';
const MODEL = __ENV.MODEL || 'demo';
const TTFB_P95_MS = __ENV.TTFB_P95_MS || '250';

const ttfb = new Trend('sse_ttfb', true);
const streamTotal = new Trend('sse_total', true);
const chunks = new Trend('sse_chunks');
const doneSeen = new Rate('sse_done_seen');

export const options = {
  scenarios: {
    stream: {
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: '1s',
      duration: DURATION,
      preAllocatedVUs: Math.max(20, RATE),
      maxVUs: Math.max(200, RATE * 4),
    },
  },
  thresholds: {
    http_req_failed: ['rate<0.001'],
    sse_ttfb: [`p(95)<${TTFB_P95_MS}`],
    sse_done_seen: ['rate>0.999'],
  },
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
};

const body = chatBody(MODEL, true);

export default function () {
  const res = http.post(chatURL(), body, {
    headers: Object.assign({ Accept: 'text/event-stream' }, jsonHeaders),
    tags: { name: 'chat_stream' },
    timeout: '60s',
  });
  const text = res.body || '';
  const dataLines = text.split('\n').filter((l) => l.startsWith('data: '));
  const done = dataLines.length > 0 && dataLines[dataLines.length - 1].trim() === 'data: [DONE]';

  check(res, {
    'status 200': (r) => r.status === 200,
    'content-type is SSE': (r) => (r.headers['Content-Type'] || '').startsWith('text/event-stream'),
    'ends with [DONE]': () => done,
  });

  if (res.status === 200) {
    ttfb.add(res.timings.waiting);
    streamTotal.add(res.timings.duration);
    chunks.add(Math.max(0, dataLines.length - 1));
  }
  doneSeen.add(done);
}
