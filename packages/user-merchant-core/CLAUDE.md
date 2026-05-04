# user-merchant-core

用户 + 商户身份服务。KYC 状态机 + 商户渠道密钥托管 + user_id / merchant_id 生成 + 登录态。

## 定位

```
外部注册流 / admin-web      ────→  user-merchant-core
                                        │
       ┌────────────┬───────────────────┤
       ↓            ↓                   ↓
 accounting-system  kms-manage   shared-meta (user_merchant_meta DB)
 （CreateAccount）   （密钥加密）
```

- **上游**：注册 / 登录接口、admin-web 的商户/用户管理页
- **下游**：accounting-system（user 主账户创建）、kms-manage（密钥托管）

## 核心领域

| 模型 | 表 | 语义 |
|---|---|---|
| User | `users` | 终端用户，登录凭证 + 基本资料；user_id ∈ [1e8, 9e8) |
| Merchant | `merchants` | 商户，KYC FSM：submitted → reviewing → approved / rejected |
| MerchantChannelSecret | `merchant_channel_secret` | 商户接入渠道的 API key，KMS 加密后存 |
| Session | `sessions` | 登录态（Redis 或 DB，按实现） |

## 生命周期

### 用户注册
1. 调用方发注册请求
2. 生成 user_id（idgen 从 1e8+ 段分配，避免撞 accounting 保留段）
3. 插 `users` 表
4. **调 accounting-system `CreateAccount`** 建 USER_BALANCE 主账户（当前版本容错：失败不阻断注册，首次支付时会兜底重试）

### 商户 KYC
- 状态机在 `internal/service/merchant_service.go`；变更记审计日志到 `merchant_audit`
- admin-web 后台点"批准"触发 `Approve` → 状态流转 + 通知

### 渠道密钥
- 商户接入 GCash / Maya 时密钥先经 kms-manage 加密（`kms:v1:` 前缀）再存 `merchant_channel_secret`
- payment-channel / payment-core 读这张表拿 decrypt 后的 plaintext（通过 kms-manage）

## 接口

gRPC（:9390 或类似端口）：
- `UserService`：`Register / Get / UpdateProfile / Login`
- `MerchantService`：`Apply / Review / Approve / Reject / List / Get`
- `MerchantChannelSecretService`：`Upsert / Get / List`

## 分片

`users` / `merchants` / `merchant_channel_secret` 在 meta DB（不分片；商户 & 用户数量级不大）。若后续增长可按 user_id / merchant_id 前缀分片。

## 依赖约束

- 生成 user_id 必须落在 `[100000000, 899999999]`
- 商户状态流转必须走 service 层，不允许直接 UPDATE 状态字段
- 密钥字段写库前必须经 kms-manage 加密
- 首次 user 创建调 accounting 失败要打 warn 告警，但不阻断（运行时兜底会补建）
