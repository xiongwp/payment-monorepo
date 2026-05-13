// 06-mixed-workload.js — 真实生产流量画像 (mixed).
//
// 拟合: 70% charge / 20% refund / 5% chargeback / 5% query.
// 总目标: 10K TPS 持续 30min.
//
// 跑法:
//   k6 run -e API_BASE=https://api.staging.payment.example.com \
//          -e API_KEY=sk_test_xxx \
//          test/k6/06-mixed-workload.js

import http from 'k6/http';
import { sleep, check, group } from 'k6';
import { Rate, Counter } from 'k6/metrics';

export const errors = new Rate('errors');
export const chargeOk = new Counter('charge_ok');
export const refundOk = new Counter('refund_ok');

export const options = {
  scenarios: {
    charge: {
      executor: 'constant-arrival-rate',
      rate: 7000,
      timeUnit: '1s',
      duration: '30m',
      preAllocatedVUs: 500,
      maxVUs: 2000,
      exec: 'doCharge',
    },
    refund: {
      executor: 'constant-arrival-rate',
      rate: 2000,
      timeUnit: '1s',
      duration: '30m',
      preAllocatedVUs: 200,
      maxVUs: 800,
      exec: 'doRefund',
    },
    chargeback: {
      executor: 'constant-arrival-rate',
      rate: 500,
      timeUnit: '1s',
      duration: '30m',
      preAllocatedVUs: 50,
      maxVUs: 200,
      exec: 'doChargeback',
    },
    query: {
      executor: 'constant-arrival-rate',
      rate: 500,
      timeUnit: '1s',
      duration: '30m',
      preAllocatedVUs: 50,
      maxVUs: 200,
      exec: 'doQuery',
    },
  },
  thresholds: {
    http_req_duration: ['p(99)<1000'],
    errors: ['rate<0.001'],
  },
};

const BASE = __ENV.API_BASE || 'https://api.staging.payment.example.com';
const API_KEY = __ENV.K6_API_KEY || 'sk_test_xxx';
const headers = {
  'Authorization': `Bearer ${API_KEY}`,
  'Content-Type': 'application/json',
};

function uid() { return `k6_${__VU}_${__ITER}_${Date.now()}`; }

export function doCharge() {
  const r = http.post(`${BASE}/v1/charges`, JSON.stringify({
    amount: 1000 + (__ITER % 5000),
    currency: 'USD',
    source: 'tok_test_visa',
    idempotency_key: uid(),
  }), { headers });
  const ok = check(r, { 'charge 2xx': r => r.status >= 200 && r.status < 300 });
  if (ok) chargeOk.add(1); else errors.add(1);
}

export function doRefund() {
  // 这里简化:基于 hash 找最近某 charge id
  const r = http.post(`${BASE}/v1/refunds`, JSON.stringify({
    charge_id: `ch_test_${__VU}`,    // 假 ID,生产用 baseline 跑出来的真实 charge id
    amount: 300,
    idempotency_key: uid(),
  }), { headers });
  const ok = check(r, { 'refund 2xx/404': r => (r.status >= 200 && r.status < 300) || r.status === 404 });
  if (ok) refundOk.add(1); else errors.add(1);
}

export function doChargeback() {
  http.post(`${BASE}/internal/dispute/test-fixture`, JSON.stringify({
    charge_id: `ch_test_${__VU}`,
    network: 'visa',
    reason_code: '4855',
  }), { headers });
}

export function doQuery() {
  http.get(`${BASE}/v1/charges/ch_test_${__VU}`, { headers });
}
