# 端到端例子 — 用户充值 ¥100 真落账

整套流程: **业务系统 → split-payment gRPC → accounting-system → MySQL ledger**

## 前置(一次性)

1. 起栈:`./deploy.sh up split-payment accounting-system payment-admin-web`
2. 浏览器 → `/moneyflow/v2` Designer → **Import** → 选 `seed/scenarios/user_topup_multileg.graph.json` → **Save**
   - 这一步把 graph 写进 split-payment 的 MySQL,key = `user-topup-multileg`
3. 在 accounting-system 那边把对应的 TransactionRule 灌进 meta DB:

```bash
docker exec -i shared-meta mysql -uroot -ppassword accounting \
  < packages/split-payment/seed/scenarios/user_topup_multileg.sql
```

## 真触发(模拟支付宝渠道回调)

```bash
curl -s -X POST http://localhost:19190/api/moneyflow/trigger \
  -H 'Content-Type: application/json' \
  -d '{
    "graph_key": "user-topup-multileg",
    "event": {
      "event":        "channel.settled",
      "charge_id":    "topup_20260515_001",
      "amount_minor": 10000,
      "currency":     "CNY",
      "trace_id":     "trace-demo-001",
      "attributes": {
        "channel_receivable_account":          "PLATFORM_RECEIVABLE_CHANNEL/sub_42",
        "channel_receivable_account_amount":   "10000",
        "channel_receivable_account_currency": "CNY",

        "channel_suspense_account":          "PLATFORM_CHANNEL_INBOUND_SUSPENSE/main",
        "channel_suspense_account_amount":   "10000",
        "channel_suspense_account_currency": "CNY",

        "user_id_account":          "USER_WALLET/42",
        "user_id_account_amount":   "9900",
        "user_id_account_currency": "CNY",

        "fee_clearing_account":          "PLATFORM_FEE_CLEARING/main",
        "fee_clearing_account_amount":   "100",
        "fee_clearing_account_currency": "CNY",

        "channel_fee_account":          "alipay_ch_fee/main",
        "channel_fee_account_amount":   "60",
        "channel_fee_account_currency": "CNY",

        "fee_account":          "PLATFORM_FEE_REVENUE/main",
        "fee_account_amount":   "40",
        "fee_account_currency": "CNY"
      }
    }
  }' | jq
```

## 期望响应

```json
{
  "vouchers": [
    {
      "event_code": "channel_settled",
      "order_no":   "topup_20260515_001_channel_settled",
      "voucher_no": "V_2026051508_abc123",
      "status":     2
    },
    {
      "event_code": "fee_cleared",
      "order_no":   "topup_20260515_001_fee_cleared",
      "voucher_no": "V_2026051508_def456",
      "status":     2
    }
  ],
  "plan": { "...": "完整 RunPlan" }
}
```

每个 `event_code` 一个 voucher_no = 一次 accounting 原子落账 (多 leg 一起成功)。

## 业务系统侧怎么发起这个

生产场景里替换 curl 的部分:

### 用 gRPC (推荐)

```go
import (
    "google.golang.org/grpc"
    "google.golang.org/grpc/credentials/insecure"
    pb "your-monorepo/split-payment/internal/grpcsvc"
)

conn, _ := grpc.NewClient("split-payment:9098", grpc.WithTransportCredentials(insecure.NewCredentials()))
client := pb.NewAdminServiceClient(conn)

eventJSON, _ := json.Marshal(map[string]any{
    "event":        "channel.settled",
    "charge_id":    "topup_" + chargeID,
    "amount_minor": 10000,
    "currency":     "CNY",
    "attributes":   attrs,  // 按 graph 里每个 node.account_id_attr 配三件套
})
resp, err := client.TriggerEvent(ctx, &pb.TriggerEventRequest{
    GraphKey:  "user-topup-multileg",
    EventJson: eventJSON,
})
// resp.Vouchers[i].VoucherNo → 落业务表做审计串联
```

### 用 Kafka (异步, 高吞吐)

业务系统发到 `channel.settled` topic,split-payment 起一个 subscriber goroutine 自动消费(当前 main.go 只接了 `refund.completed`,仿照添加即可):

```go
go runKafkaTrigger(ctx, log, "channel.settled", graphKey="user-topup-multileg", adminClient)
```

## 验证落账

```bash
# 看 voucher 详情 + accounting 那边的 transaction 流水
docker exec shared-meta mysql -uroot -ppassword accounting -e \
  "SELECT order_no, status, voucher_no, amount FROM transaction_order \
    WHERE order_no LIKE 'topup_20260515_001%' ORDER BY id"

# 验账户余额
docker exec shared-shard-4 mysql -uroot -ppassword accounting_db_4 -e \
  "SELECT account_no, balance FROM account WHERE account_no IN \
    ('USER_WALLET/42','PLATFORM_FEE_REVENUE/main','PLATFORM_FEE_CLEARING/main')"
```

## 幂等

同 `(charge_id, event_code)` 再发一次,accounting 内部用 `order_no` 去重,不会重复落账:

```bash
# 重发同一 curl, 期望:
{
  "vouchers": [
    {
      "event_code": "channel_settled",
      "voucher_no": "V_2026051508_abc123",   ← 跟上次一样
      "status":     2
    },
    ...
  ]
}
```

## Designer 里直接试

我会在 v1/v2 designer 加 "Trigger Now" 按钮,点一下:
1. 从当前 spec 的 nodes 提取 account_id_attr 三件套
2. 弹 prompt 填 charge_id / amount
3. POST `/api/moneyflow/trigger` → 真落账
4. alert 显示 voucher_no
