import http from 'k6/http';
import { check } from 'k6';
import { Counter } from 'k6/metrics';
import { SharedArray } from 'k6/data';

// Query pool from the index homework: 42 prefix pairs stratified by the number
// of matching rows, including the specification example Конст/Оси.
const queries = new SharedArray('queries', () => JSON.parse(open('../queries.json')));

const BASE_URL = __ENV.BASE_URL || 'http://127.0.0.1:8080';
const USERNAME = __ENV.USERNAME || 'user0000001';
const PASSWORD = __ENV.PASSWORD || 'password123';

const VUS = Number(__ENV.VUS || 1);
const DURATION = __ENV.DURATION || '30s';
// A ramp avoids a simultaneous TCP connect burst at high VU counts.
const RAMP = Number(__ENV.RAMP || 0);
// Share of GET /user/get/{id}; the rest is GET /user/search.
const GET_SHARE = Number(__ENV.GET_SHARE || 0.5);
const ID_MAX = Number(__ENV.ID_MAX || 1000000);

const getRequests = new Counter('get_requests');
const searchRequests = new Counter('search_requests');

const scenario = RAMP > 0
  ? {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: [
        { duration: `${RAMP}s`, target: VUS },
        { duration: DURATION, target: VUS },
      ],
      gracefulRampDown: '5s',
    }
  : {
      executor: 'constant-vus',
      vus: VUS,
      duration: DURATION,
      gracefulStop: '5s',
    };

export const options = {
  scenarios: { reads: scenario },
  summaryTrendStats: ['avg', 'min', 'med', 'max', 'p(50)', 'p(90)', 'p(95)', 'p(99)'],
  // Thresholds are deliberately loose: they create per-endpoint submetrics in
  // the summary JSON, which summarize.py reads. The real analysis is offline.
  thresholds: {
    'http_req_duration{endpoint:get}': ['p(99)<300000'],
    'http_req_duration{endpoint:search}': ['p(99)<300000'],
    'http_req_failed{endpoint:get}': ['rate<=1'],
    'http_req_failed{endpoint:search}': ['rate<=1'],
  },
};

export function setup() {
  const res = http.post(
    `${BASE_URL}/login`,
    JSON.stringify({ username: USERNAME, password: PASSWORD }),
    { headers: { 'Content-Type': 'application/json' } }
  );
  check(res, { 'login 200': (r) => r.status === 200 });
  return { token: res.json('token') };
}

export default function (data) {
  const headers = { Authorization: `Bearer ${data.token}` };

  if (Math.random() < GET_SHARE) {
    const id = 1 + Math.floor(Math.random() * ID_MAX);
    const res = http.get(`${BASE_URL}/user/get/${id}`, {
      headers,
      tags: { endpoint: 'get' },
      timeout: '120s',
      responseType: 'none',
    });
    getRequests.add(1);
    check(res, { 'get status 200': (r) => r.status === 200 });
    return;
  }

  const q = queries[Math.floor(Math.random() * queries.length)];
  const url =
    `${BASE_URL}/user/search?first_name=${encodeURIComponent(q.first)}` +
    `&last_name=${encodeURIComponent(q.last)}`;
  const res = http.get(url, {
    headers,
    tags: { endpoint: 'search' },
    timeout: '120s',
    responseType: 'none',
  });
  searchRequests.add(1);
  check(res, { 'search status 200': (r) => r.status === 200 });
}

export function handleSummary(data) {
  const out = __ENV.OUT;
  if (!out) {
    return { stdout: '' };
  }
  return { [out]: JSON.stringify(data, null, 2) };
}
