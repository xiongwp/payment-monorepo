# PH 渠道 Mock Server

`cmd/mockserver` 启动一个进程内 HTTP 服务，模拟 payment-channel 支持的所有
菲律宾渠道：GCash、Maya、GrabPay、CoinsPH、InstaPay、PESONet、BDO、BPI、
Metrobank、LandBank、PayMongo、Xendit、Dragonpay、ShopeePay、BillEase。

用途：

- 本地开发不依赖外部 sandbox（很多菲律宾银行/PSP 的 sandbox 账号难申请）
- CI e2e 测试（`make test-mock`）
- 压测 / 训练集成
- 教学演示

## 场景注入

每个 Charge 请求都会解析出一个场景，决定 mock 的响应：

| Scenario | 说明 | 触发方式 |
|---|---|---|
| `success` | 同步成功 | `amount == 1` 或 `X-Mock-Scenario: success` |
| `requires_action` | 返回跳转链接，需要用户操作 | `amount == 2` 或 `X-Mock-Scenario: ra` |
| `fail_card_declined` | 同步失败（卡拒付） | `amount == 3` |
| `fail_insufficient_funds` | 同步失败（余额不足） | `amount == 4` |
| `fail_risk_blocked` | 同步失败（风控拒绝） | `amount == 5` |
| `fail_channel_unavailable` | 同步失败（渠道不可用） | `amount == 6` |
| `async_success` | 同步返回 processing，稍后推 webhook=success | `amount == 7` |
| `async_fail` | 同步返回 processing，稍后推 webhook=failed | `amount == 8` |
| `timeout` | mock 挂起 30s | `amount == 9` |

配置 webhook 延迟：`-webhook-delay 200ms`（默认 200ms）。

## 使用姿势

### 本地

```bash
# 1. 启动 mock
make run-mock                            # :9400

# 2. 启动 payment-channel（用 sandbox profile）
PAYCHAN_CONFIG=sandbox make run          # 所有 adapter 都指向 mock
```

### 单元/集成测试

`internal/mockserver/mockserver_test.go` 里有完整示例：

```go
srv, _ := mockserver.New(mockserver.Options{WebhookDelay: 20 * time.Millisecond})
ts := httptest.NewServer(srv.Handler())
defer ts.Close()

a, _ := gcash.New(gcash.Config{
    BaseURL:      ts.URL,
    GCashPubKey:  srv.GCashPublicKeyPEM(),   // 用于验签
    MerchantPriv: mockserver.MarshalPKCS8PrivateKeyPEM(myPriv),
})
```

### docker-compose

```yaml
mockserver:
  build:
    context: .
    dockerfile: Dockerfile.mockserver
  ports: ["9400:9400"]
```

起来后把 `channel.*.base_url` 都指向 `http://mockserver:9400`。

## GCash 签名 / 验签

真实 GCash（Alipay+ mPaaS）用 RSA-SHA256 双向签名：

- 商户 → GCash：用商户私钥签，GCash 用商户公钥验
- GCash → 商户（webhook）：用 GCash 私钥签，商户用 GCash 公钥验

Mock 启动时生成一对 RSA-2048 给"GCash 网关"。`srv.GCashPublicKeyPEM()` 返回
公钥，放进 `channel.gcash.gcash_pub_key` 或 adapter 的 Config 即可。

`cmd/mockserver` 默认会把这个公钥打印到 stdout，拷出来直接贴进配置文件。

## 调试接口

- `GET /healthz` → `{"ok": true}`
- `GET /__mock/list` → dump 所有内存中的支付记录（仅用于本地调试）

## 已知局限

- Dragonpay webhook 发的是 JSON 而非真实的 form-encoded（adapter 里 JSON 解析
  够用，但要跟真 Dragonpay 签名格式完全一致还得扩展）
- 其他 9 个银行/钱包渠道（CoinsPH/InstaPay/PESONet/BDO/BPI/Metrobank/LandBank/
  ShopeePay/BillEase）走的是通用 `simpleEnvelope`，schema 是联合字段，能让
  adapter 的 JSON decoder 不报错；某些字段的真实语义（如 BPI 的账户段号）没还原
- 不保证每个字段都跟真实渠道一一对应——用途是让 adapter 的代码路径跑通，不是替
  代 contract testing
