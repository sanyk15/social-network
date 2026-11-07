import http from 'k6/http';
import { check } from 'k6';
import { Counter } from 'k6/metrics';

const BASE_URL = __ENV.BASE_URL || 'http://127.0.0.1:8080';
const RATE = Number(__ENV.RATE || 200);
const DURATION = __ENV.DURATION || '20s';
const PRE_VUS = Number(__ENV.PRE_VUS || 50);
const MAX_VUS = Number(__ENV.MAX_VUS || 200);

const writesOk = new Counter('writes_ok');
const writesFailed = new Counter('writes_failed');

export const options = {
  scenarios: {
    writes: {
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: '1s',
      duration: DURATION,
      preAllocatedVUs: PRE_VUS,
      maxVUs: MAX_VUS,
    },
  },
  summaryTrendStats: ['avg', 'min', 'med', 'max', 'p(50)', 'p(90)', 'p(95)', 'p(99)'],
};

export default function () {
  const res = http.post(`${BASE_URL}/test/write`, null, {
    tags: { endpoint: 'write' },
    timeout: '30s',
    responseType: 'none',
  });

  if (res.status === 201) {
    writesOk.add(1);
  } else {
    writesFailed.add(1);
  }
  check(res, { 'write 201': (r) => r.status === 201 });
}

export function handleSummary(data) {
  const out = __ENV.OUT;
  if (!out) {
    return { stdout: '' };
  }
  return { [out]: JSON.stringify(data, null, 2) };
}
