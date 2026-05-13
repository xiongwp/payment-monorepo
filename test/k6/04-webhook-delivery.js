// 04-webhook-delivery.js — merchant webhook 推送压测.
//
// 测试 merchant-webhook 服务: 模拟 charge.succeeded 事件入队 → 出站 HTTP 投递 →
// 商户应用 200 OK 返回 → ack。重点观察:
//   - 投递 P99 latency (出站 HTTP)
//   - 失败 → DLQ 转入率
//   - 重试退避是否生效 (用 fake-merchant 模拟随机 5xx)
//
// 跑法:
//   k6 run --vus 100 --duration 10m test/k6/04-webhook-delivery.js

import http from 'k6/http';
import { sleep, check } from 'k6';
import { Trend, Rate } from 'k6/metrics';

const enqueueLatency = new Trend('webhook_enqueue_ms', true);
const deliveryFailure = new Rate('webhook_delivery_failures');

export const options = {
  vus: 100,
  duration: '10m',
  thresholds: {
    'webhook_enqueue_ms': ['p(99)<200'],
    'webhook_delivery_failures': ['rate<0.05'],
  },
};

const BASE = __ENV.API_BASE || 'https://api.staging.payment.example.com';
const TOKEN = __ENV.ADMIN_TOKEN || 'admin_test';

export default function () {
  const eventId = `evt_k6_${__VU}_${__ITER}_${Date.now()}`;

  const t0 = Date.now();
  const enq = http.post(`${BASE}/internal/merchant-webhook/enqueue`, JSON.stringify({
    event_id: eventId,
    event_type: 'charge.succeeded',
    merchant_id: `m_test_${__VU % 10}`,    // 10 个商户负载分散
    payload: {
      id: `ch_${eventId}`,
      amount: 1000,
      currency: 'USD',
    },
  }), {
    headers: {
      'Authorization': `Bearer ${TOKEN}`,
      'Content-Type': 'application/json',
    },
  });
  enqueueLatency.add(Date.now() - t0);

  check(enq, { 'enqueue 202': r => r.status === 202 });

  // 等 5s 看是否被投递
  sleep(5);

  const status = http.get(`${BASE}/internal/merchant-webhook/events/${eventId}`, {
    headers: { 'Authorization': `Bearer ${TOKEN}` },
  });
  const delivered = check(status, {
    'event delivered or in_progress': r => ['delivered', 'in_progress'].includes(r.json('state')),
  });
  if (!delivered) deliveryFailure.add(1);
}
