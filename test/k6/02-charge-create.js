// k6 — payment-gateway PaymentIntent 创建吞吐.
//
// 模拟商户用 OAuth2 拿 token, 持续创建 charge.
// SLO: p99 < 500ms, < 0.1% err, 200 rps.

import http from 'k6/http';
import { check, sleep } from 'k6';
import { Trend, Rate } from 'k6/metrics';

const chargeLatency = new Trend('charge_create_ms', true);
const chargeErrors = new Rate('charge_create_errors');

const OAUTH = __ENV.OAUTH_URL  || 'http://localhost:18087';
const API   = __ENV.API_BASE   || 'http://localhost:18091';
const CID   = __ENV.CLIENT_ID  || 'mer_demo_merchant_01';
const SEC   = __ENV.CLIENT_SECRET || 'dev_secret_merchant_001';

let cachedToken = null;
let tokenExpAt = 0;

function getToken() {
  if (cachedToken && Date.now() < tokenExpAt) return cachedToken;
  const r = http.post(`${OAUTH}/oauth2/token`,
    `grant_type=client_credentials&client_id=${CID}&client_secret=${SEC}&scope=charge:write`,
    { headers: { 'Content-Type': 'application/x-www-form-urlencoded' } });
  const t = r.json('access_token');
  cachedToken = t;
  tokenExpAt = Date.now() + (r.json('expires_in') || 3600) * 1000 - 60_000; // 提前 1min 续
  return t;
}

export const options = {
  stages: [
    { duration: '30s', target: 50 },
    { duration: '2m',  target: 200 },
    { duration: '3m',  target: 200 },
    { duration: '30s', target: 0 },
  ],
  thresholds: {
    'http_req_duration{endpoint:pi}': ['p(99)<500'],
    'http_req_failed':                ['rate<0.001'],
    'charge_create_errors':           ['rate<0.005'],
  },
};

export default function () {
  const tok = getToken();
  const amount = 100 + Math.floor(Math.random() * 50000);
  const body = JSON.stringify({
    amount_minor: amount,
    currency: 'USD',
    payment_method: 'card',
    description: 'k6 perf test',
    metadata: { test: 'k6-02' },
  });
  const r = http.post(`${API}/api/v1/payment-intents`, body, {
    headers: {
      'Authorization': `Bearer ${tok}`,
      'Content-Type': 'application/json',
      'Idempotency-Key': `k6-${__VU}-${__ITER}-${Date.now()}`,
    },
    tags: { endpoint: 'pi' },
  });
  chargeLatency.add(r.timings.duration);
  const ok = check(r, {
    'status 200/201': (resp) => resp.status === 200 || resp.status === 201,
    'has id':         (resp) => (resp.json('id') || '').length > 0,
  });
  chargeErrors.add(!ok);
}
