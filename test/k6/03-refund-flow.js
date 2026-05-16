// 03-refund-flow.js — refund 端到端压测.
//
// 流程: charge.create → wait for capture → refund.create → assert refunded.
//
// 跑法:
//   k6 run --vus 50 --duration 5m test/k6/03-refund-flow.js
//
// 基线 (m6i.4xlarge × 3 payment-core, MySQL 主从):
//   p95 charge: 180ms / p95 refund: 220ms / error rate < 0.1%

import http from 'k6/http';
import { sleep, check } from 'k6';
import { Rate } from 'k6/metrics';

export const errorRate = new Rate('errors');

export const options = {
  vus: 50,
  duration: '5m',
  thresholds: {
    http_req_duration: ['p(95)<500', 'p(99)<1000'],
    errors: ['rate<0.01'],
    'http_req_duration{stage:charge}': ['p(95)<300'],
    'http_req_duration{stage:refund}': ['p(95)<400'],
  },
  scenarios: {
    constant: {
      executor: 'constant-vus',
      vus: 50,
      duration: '5m',
    },
  },
};

const BASE = __ENV.API_BASE || 'https://api.staging.payment.example.com';
const API_KEY = __ENV.K6_API_KEY || 'sk_test_xxx';

function uid() {
  return `k6_${__VU}_${__ITER}_${Date.now()}`;
}

export default function () {
  const idemCharge = uid();
  const idemRefund = uid();

  // 1) Create charge
  const chargeRes = http.post(`${BASE}/v1/charges`, JSON.stringify({
    amount: 1000,
    currency: 'USD',
    source: 'tok_test_visa',
    idempotency_key: idemCharge,
    metadata: { scenario: 'refund_flow' },
  }), {
    headers: {
      'Authorization': `Bearer ${API_KEY}`,
      'Content-Type': 'application/json',
    },
    tags: { stage: 'charge' },
  });

  const ok1 = check(chargeRes, {
    'charge 200/201': r => r.status === 200 || r.status === 201,
    'charge has id': r => r.json('id') !== undefined,
  });
  if (!ok1) {
    errorRate.add(1);
    return;
  }
  const chargeId = chargeRes.json('id');

  sleep(1);  // 模拟商户业务延迟

  // 2) Refund (partial 30%)
  const refundRes = http.post(`${BASE}/v1/refunds`, JSON.stringify({
    charge_id: chargeId,
    amount: 300,
    reason: 'requested_by_customer',
    idempotency_key: idemRefund,
  }), {
    headers: {
      'Authorization': `Bearer ${API_KEY}`,
      'Content-Type': 'application/json',
    },
    tags: { stage: 'refund' },
  });

  const ok2 = check(refundRes, {
    'refund 200/201': r => r.status === 200 || r.status === 201,
    'refund status succeeded/processing': r => ['succeeded', 'processing'].includes(r.json('status')),
  });
  if (!ok2) errorRate.add(1);
}
