# billing-system

商户级计费 + 出账系统。

## 能力

- **Fee rule engine** — 按 merchant/product/channel/region/卡组/币种/金额范围多维匹配
- **Fee event** — 每笔 transaction 实时算 fee 落库，幂等 by (merchant, ref_id, event_type)
- **Statement** — 日/月聚合账单，按币种拆 multi-statement
- **Refund/Chargeback** — 三种策略 (refund/keep/prorate) + 罚款
- **FX markup** — 跨币种独立 line item，进 billing 不影响主 fee
- **商户后台 REST API** — 账单查询 + PDF 下载（MVP 是 text/plain）
- **Admin ops API** — fee dry-run / 强制聚合 / 增 rule

## 启动

```bash
docker build -t billing-system:local .
docker run -p 18090:8080 \
  -e BILLING_BASE_CURRENCY=USD \
  -v $(pwd)/configs/fee_rules.yaml:/app/configs/fee_rules.yaml \
  billing-system:local
```

## API

```bash
# dry-run 算 fee（不落库 — 给商户结账前预览）
curl -s -X POST localhost:18090/api/v1/fee/calc -d '{
  "merchant_id": "mer_test",
  "ref_id": "pi_test",
  "event_type": "charge",
  "amount_minor": 10000,
  "currency": "PHP",
  "product": "card_charge",
  "channel_adapter": "visa",
  "region": "PH"
}' | jq

# 喂事件（真实算+落库）
curl -s -X POST localhost:18090/api/v1/fee/events -d '...'

# 列商户账单
curl -s 'localhost:18090/api/v1/statements?merchant_id=mer_test&limit=12'

# 单期账单
curl -s localhost:18090/api/v1/statements/1
curl -s localhost:18090/api/v1/statements/1/events
curl -s localhost:18090/api/v1/statements/1/pdf

# 强制触发某 period 聚合
curl -s -X POST localhost:18090/api/v1/aggregate -d '{
  "from": "2026-05-01T00:00:00Z",
  "to":   "2026-06-01T00:00:00Z",
  "final": true
}'
```

## 接 order-core 自动算 fee

生产：order-core 发 Kafka topic `payments.charge.succeeded` →
billing-system 监听 → Process 算 fee_event。

MVP：order-core 直接 POST 到 `/api/v1/fee/events` 也行（同步阻塞但简单）。

## 后续路线

详见顶层 `PAYMENT_SYSTEM_GAPS.md` 的 P1 清单。短期最重要:

1. MySQL repo 替代 memory（数据持久化）
2. Kafka consumer 接 order-core 事件
3. 真 PDF 渲染（gofpdf 或 wkhtmltopdf）
4. 商户后台 UI（accounting-admin-web 加 billing 模块）
5. 接 reconplatform — fee_event_mismatch 自动检测
