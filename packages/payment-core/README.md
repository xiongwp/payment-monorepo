# payment-core

`payment-core` 是 `order-core` 与 `payment-channel` 之间的无状态路由层。

```
order-core ──gRPC──▶ payment-core ──gRPC──▶ payment-channel ──HTTP──▶ 第三方渠道
```

## 职责

1. **按 `payment_method` / `country` / `amount` / `merchant` 路由** 到具体的
   payment-channel adapter（`internal/routing`）。
2. 把 order-core 的 `PaymentChannel` 6 方法翻译成 payment-channel 的
   `AcquirerService` RPC 调用（`internal/service`）。
3. 规范化 webhook、失败码与 `RequiredAction` 形状，保证 order-core 拿到的
   协议稳定（`internal/channel/failure_code.go` + `service.WebhookService`）。
4. **不持有任何业务状态、无 DB**。重启后立即可用，所有状态都在 order-core 与
   payment-channel 两侧。

## 目录

```
├── api/proto/
│   └── paymentcore/v1/paymentcore.proto   order-core 看到的 PaymentCoreService
│   # channel.v1 的 proto 直接引用 payment-channel 仓的 stub（见 go.mod 的 replace），
│   # 避免双份 pb.go 冲突。
├── cmd/server/                              fx 装配入口
├── config/config.yaml                       路由规则 + channel endpoint
├── internal/
│   ├── channel/                             PaymentRequest / Response + 失败码归一
│   ├── routing/                             路由引擎（+ 单测）
│   ├── channelclient/                       payment-channel gRPC client 门面
│   ├── service/                             PaymentService / WebhookService
│   ├── server/                              gRPC adapter + interceptors
│   └── metrics/                             Prometheus paycore_*
├── Dockerfile / Makefile / go.mod
```

## 快速开始

前置条件：把 `payment-channel` 仓库克隆为 `../payment-channel`（同级）。
`go.mod` 的 `replace` 指向这个相对路径。

```bash
make install-tools && make proto  # 生成 paymentcore.v1 的 Go stub
make build                        # 编译
make test                         # 跑单测（路由 + service e2e + 6 条 bufconn e2e）
./bin/payment-core                # 启动（读 ./config/config.yaml）
```

### 路由规则

`config/config.yaml` 里 `routing.rules` 逐条匹配：

```yaml
- priority: 100
  country: PH
  payment_method: GCASH
  adapter: gcash
- priority: 999            # 兜底
  country: PH
  adapter: gcash
```

匹配顺序：
1. `merchant` 非空优先（可用来给 VIP 商户单独路由到更优通道）
2. `priority` 数值小优先
3. 条件更具体者优先（country + payment_method + amount 区间命中越多越具体）

支持的字段：`merchant / country / payment_method / amount_min / amount_max`，
全字段可选。

### 失败码归一

payment-channel 自身会把渠道原始码归到 7 个规范码，payment-core 再兜底一次
（防止新 adapter 漏写）：`card_declined` / `insufficient_funds` /
`risk_blocked` / `auth_failed` / `expired` / `channel_unavailable` / `unknown`。

### Webhook

order-core 收到 `payment-channel` 转过来的回调（或自己直接收渠道回调）后，
把原始 `headers + body + adapter` POST 给 payment-core 的 `ParseWebhook`
RPC，payment-core 做结构归一返回规范化的 `WebhookEvent`（无 DB）。签名校验
在 payment-channel 侧已完成，payment-core 不再重复校验。

## 设计决策

- **为什么无 DB**：业务状态机、对账、差错处理都在 order-core；渠道流水和
  幂等兜底都在 payment-channel。中间层保留状态会让三仓一致性难题再起一层，
  没有收益。
- **为什么共用 payment-channel 的 proto**：`channel.v1` 在 protoregistry 里是
  全局唯一的 proto package；如果 payment-core 再生成一份，两个 init() 挂进同一
  进程（e2e 测试场景）会 panic `proto: file ... already registered`。统一由
  payment-channel 维护，payment-core 通过 go.mod replace 指向 sibling 目录。
- **为什么每个 RPC 都先路由再透传**：Capture / Void / Refund / Query 本来只要
  凭 external_ref_no 就能定位 adapter，但 order-core 现阶段不透传 adapter 名，
  故在 payment-core 先做一次路由兜底（见 `service.adapterForOp` TODO）。
  后续可以让 order-core 在 PayIntent 上冗余一列 `adapter_name`。

## 联调

```bash
# 假设 payment-channel 已在 127.0.0.1:9092
go run ./cmd/server

# 用 grpcurl 模拟 order-core 发一笔 charge
grpcurl -plaintext -d '{
  "payment_intent_id": "pi_4371234560001",
  "charge_id":         "ch_4371234560001",
  "amount":            10000,
  "currency":          "PHP",
  "country":           "PH",
  "payment_method":    "GCASH",
  "return_url":        "https://cashier/ret"
}' 127.0.0.1:9090 paymentcore.v1.PaymentCoreService/Charge
```
