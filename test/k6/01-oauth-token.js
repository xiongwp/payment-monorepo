// k6 — OAuth2 token issue 吞吐压测.
//
// 目标: 500 rps × 5min 持续, p99 < 200ms, 0 错误.
//
// 跑:
//   BASE_URL=http://localhost:18087 \
//   CLIENT_ID=mer_demo_merchant_01 \
//   CLIENT_SECRET=dev_secret_merchant_001 \
//   k6 run 01-oauth-token.js

import http from 'k6/http';
import { check, sleep } from 'k6';
import { Trend, Rate, Counter } from 'k6/metrics';

const tokenLatency = new Trend('oauth_token_ms', true);
const tokenErrors = new Rate('oauth_token_errors');
const tokenIssued = new Counter('oauth_tokens_issued');

const BASE = __ENV.BASE_URL || 'http://localhost:18087';
const CID = __ENV.CLIENT_ID || 'mer_demo_merchant_01';
const SEC = __ENV.CLIENT_SECRET || 'dev_secret_merchant_001';

export const options = {
  stages: [
    { duration: '30s', target: 100 },    // 预热
    { duration: '1m',  target: 500 },    // 爬到 500 VUs
    { duration: '3m',  target: 500 },    // 稳定 500 VUs ~ 500 rps
    { duration: '30s', target: 0 },      // 冷却
  ],
  thresholds: {
    'http_req_duration{endpoint:token}':  ['p(50)<50', 'p(99)<200'],
    'http_req_failed':                    ['rate<0.001'],          // < 0.1%
    'oauth_token_errors':                 ['rate<0.005'],
    'oauth_tokens_issued':                ['count>50000'],         // 约 5min × 200rps 起步
  },
};

export default function () {
  const body = `grant_type=client_credentials&client_id=${CID}&client_secret=${SEC}`;
  const r = http.post(`${BASE}/oauth2/token`, body, {
    headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
    tags: { endpoint: 'token' },
  });

  tokenLatency.add(r.timings.duration);
  const ok = check(r, {
    'status 200':         (resp) => resp.status === 200,
    'has access_token':   (resp) => (resp.json('access_token') || '').length > 50,
    'token_type=Bearer':  (resp) => resp.json('token_type') === 'Bearer',
  });
  tokenErrors.add(!ok);
  if (ok) tokenIssued.add(1);
}

export function handleSummary(data) {
  // 推一份简要 JSON 到 stdout, CI 抓
  return { stdout: JSON.stringify(summary(data), null, 2) };
}

function summary(data) {
  return {
    test: '01-oauth-token',
    vus_max: data.metrics.vus_max?.values?.value,
    iterations: data.metrics.iterations?.values?.count,
    rps: data.metrics.iterations?.values?.rate,
    error_rate: data.metrics.http_req_failed?.values?.rate,
    p50_ms: data.metrics.http_req_duration?.values?.['p(50)'],
    p99_ms: data.metrics.http_req_duration?.values?.['p(99)'],
    slo_passed: (data.metrics.http_req_duration?.values?.['p(99)'] || 999) < 200,
  };
}
