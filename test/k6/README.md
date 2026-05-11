# k6 Performance Benchmark Suite

针对支付平台关键路径的负载测试 + 持续性能验证。每条用例对齐一个 SLO。

## 安装

```bash
brew install k6                        # macOS
# 或 docker: docker run grafana/k6 ...
```

## 用例清单 (每个一份 .js)

| 文件 | 目标 | SLO 阈值 |
|---|---|---|
| `01-oauth-token.js` | OAuth2 token issue 吞吐 | p99 < 200ms, 0 err, 500 rps |
| `02-charge-create.js` | payment-gateway PI 创建 | p99 < 500ms, ≤ 0.1% err, 200 rps |
| `03-refund-flow.js` | 退款全流程 (create + submit + complete) | p99 < 1s, 100 rps |
| `04-merchant-list.js` | user-merchant-core 读 (含 cache) | p99 < 50ms, 1000 rps |
| `05-fx-quote-confirm.js` | FX quote + confirm (2 步) | p99 < 300ms, 100 rps |
| `06-moneyflow-trigger.js` | charge.succeeded → graph 执行 | p99 < 800ms, 200 rps |
| `07-soak-24h.js` | 24h 稳定性 (内存泄漏 / 慢腐蚀) | 0 err drift, mem +5% max |
| `08-spike-burst.js` | 突发流量 (1000 → 5000 rps) | 自动 HPA 扩容 + 0 5xx 持续 |

## 跑

```bash
# 单 case
k6 run test/k6/01-oauth-token.js

# 带变量
BASE_URL=http://localhost:18087 \
CLIENT_ID=mer_demo_merchant_01 \
CLIENT_SECRET=dev_secret_merchant_001 \
k6 run test/k6/01-oauth-token.js

# 全跑 + 推 metric 到 Prometheus
k6 run --out experimental-prometheus-rw=http://prometheus:9090/api/v1/write \
  test/k6/01-oauth-token.js

# CI: GitHub Actions matrix 跑全部, fail 不到 SLO 时 PR 红
```

## 输出

每个 case 含 thresholds 块, 失败立即 exit non-zero:

```js
thresholds: {
  http_req_duration: ['p(99)<200'],   // p99 < 200ms
  http_req_failed:   ['rate<0.001'],  // 错误率 < 0.1%
  iterations:        ['rate>500'],    // 500 rps
}
```

## CI 集成

`.github/workflows/perf.yml`:
- nightly 跑全部
- PR 跑 01-04 (快速)
- 结果上传 Grafana annotation, regression > 10% 阻 merge

## 报告

`test/k6/dashboards/k6-grafana.json` — 全部 case 时间序列对比, 标 SLO 线。
