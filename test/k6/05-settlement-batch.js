// 05-settlement-batch.js — 清算批处理压测.
//
// 流程: 触发 settlement.trigger → 轮询 status → 完成。
// 重点观察:
//   - 单批触发 RPC < 50ms (派号 + 入队)
//   - 端到端 (1000 商户) < 5min
//
// 跑法:
//   k6 run --vus 10 --iterations 20 test/k6/05-settlement-batch.js

import http from 'k6/http';
import { sleep, check } from 'k6';

export const options = {
  scenarios: {
    settlement: {
      executor: 'per-vu-iterations',
      vus: 10,
      iterations: 2,
      maxDuration: '30m',
    },
  },
  thresholds: {
    'http_req_duration{rpc:trigger}': ['p(95)<100'],
  },
};

const BASE = __ENV.API_BASE || 'https://api.staging.payment.example.com';
const TOKEN = __ENV.ADMIN_TOKEN || 'admin_test';

export default function () {
  const settleDate = '2026-05-13';
  const currency = ['USD', 'EUR', 'GBP', 'JPY'][__VU % 4];

  // 1) Trigger
  const trig = http.post(`${BASE}/internal/clearing/trigger`, JSON.stringify({
    settle_date: settleDate,
    currency: currency,
  }), {
    headers: { 'Authorization': `Bearer ${TOKEN}` },
    tags: { rpc: 'trigger' },
  });
  check(trig, { 'trigger 200': r => r.status === 200 });
  const runId = trig.json('run_id');

  // 2) Poll status
  let attempts = 0;
  let completed = false;
  while (attempts < 60 && !completed) {
    sleep(5);
    attempts++;
    const st = http.get(`${BASE}/internal/clearing/status?settle_date=${settleDate}&run_id=${runId}`, {
      headers: { 'Authorization': `Bearer ${TOKEN}` },
    });
    if (st.status === 200 && st.json('overall_status') === 'COMPLETED') {
      completed = true;
    }
  }
  check(null, { 'settlement completed within 5min': () => completed });
}
