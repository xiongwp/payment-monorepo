# accounting-system

复式记账核心。双分录 + TCC + 日切 + 缓冲记账。平台唯一的资金真理源。

## 定位

```
order-core / user-merchant-core / risk-manage  ─gRPC──→  accounting-system
                                                             │
                                                             ├─ account_meta  （1 个 meta DB）
                                                             └─ accounting_db_N （10 个分片 DB × 100 张表）
```

所有"钱"的事件必须由调用方通过 `HybridDoubleEntryBooking` 落账；accounting-system 不自己发起业务逻辑。

## 核心概念

### AccountType（账户类型，1-9）
| ID | 名 | 备注 |
|---|---|---|
| 1 | USER | 用户主账户 |
| 2 | MERCHANT | 商户账户 |
| 3 | MERCHANT_PENDING_SETTLE | 商户待结算 |
| 4 | PLATFORM | 平台损益 |
| 5 | TRANSIT_CHANNEL_RECEIVABLE | 中间渠道应收（GCash/Maya 等 fleet）|
| 6 | TRANSIT_CHANNEL_PAYABLE | 中间渠道应付 |
| 7 | TRANSACTION_FEE | 手续费 |
| 8 | CHARGE_FEE | 服务费 |
| 9 | TRANSIT | 平台中间账户 |

### AccountBusinessType（1-999）
- **`[1, 100]`**：系统保留枚举（USER_BALANCE=1, MERCHANT_BALANCE=2, MERCHANT_PENDING_SETTLE=3, ...）
- **`[101, 999]`**：动态注册（渠道应收 GCASH_RECEIVABLE 等），通过 `RegisterBusinessType` 自动分配

### user_id 段
- `[0, 99]`：平台 fleet 账户保留（每 channel 100 个平台账户，按 user_id%100 路由）
- `[100, 100000000)` 保留未用
- `[100000000, 899999999]`：**业务 user_id 合法段**
- 业务侧传 < 1e8 的 user_id 会被 `CreateAccount` 拒

### 金额精度
- 对外 API（Money 字段）：**ISO minor units**（PHP cents = 1e-2 PHP）
- 内部 storage 值：minor × 100（ISO minor 再放大 100 = 4 位小数 decimal），目的是兼容未来更高精度币种
- **容易踩的坑**：string `debit_amount/credit_amount` 字段当"major units"解读，order-core 传 `10000` 会被当 10000.0 PHP 而不是 100 PHP。新代码用 `Money{MinorUnits, Currency}` 显式避免

### TCC
`DoubleEntryBooking / HybridDoubleEntryBooking` 内部分 Try / Confirm / Cancel 三阶段。TCC 全局协调者（`tcc_coordinator`）+ 分支（`tcc_branch`）实现崩溃恢复，卡在 CONFIRMING 超 5 min 的会被 worker 自动推回（`/admin/tcc/retry-confirm` 可立即触发）。

### 缓冲记账（BufferedBalance）
高频热账户（渠道 fleet）写 balance 不是直接 UPDATE，而是先写 `account_balance_buffer`，后台 worker 每 30s + jitter flush 到 `account` 表。好处：高并发下避免锁竞争。**代价**：fleet balance 查询不即时。

## 接口

- gRPC（:50051）
  - `AccountingService`：`CreateAccount / GetAccount / Freeze / DoubleEntryBooking / HybridDoubleEntryBooking / BatchBooking / AtomicBatchBooking / MoneyFlow / ...`
  - `AccountingAdminService`：实例管理 / hot-account / buffer-account / business-type 注册
  - `FreezeService`：冻结 / 解冻 / 扣减冻结 / 返还冻结

- 内部 HTTP（:8888）管理端点（详见 `internal/adminhttp/server.go`）
  - `/admin/business-types` CRUD
  - `/admin/platform-accounts/fleet` 批量建 100 个渠道账户
  - `/admin/platform-accounts/balances?business_type=X` 返回 fleet 100 账户余额
  - `/admin/buffered-balance/flush` 立即触发 buffer flush（e2e 测试友好）
  - `/admin/config` 系统通用 kv 配置
  - `/admin/tcc/retry-confirm` 立即恢复 CONFIRMING
  - `/admin/day-cut/resume` 恢复卡死的日切分片

- Metrics HTTP（:9090）：`/metrics`

## Onboarding（启动期）

服务本身不做 onboarding。调用方（order-core / user-merchant-core）启动时：
1. `POST /admin/business-types` 注册渠道 `business_type_code`（幂等：已注册返回原 ID）
2. `POST /admin/platform-accounts/fleet` 建 100 个 fleet 平台账户（幂等）

运行时记账前，用户 main 账户（USER_BALANCE / MERCHANT_PENDING_SETTLE）必须已 `CreateAccount`。首次付款无此账户的兜底自动建由调用方（如 order-core accounting client）负责。

## 分片

1 个 meta DB（account_meta）+ 10 shard × 10 table = 100 分片。
- 账户 / 交易 / 快照 / buffer / TCC 全部按 `user_id`（业务账户）或 `reserved_id%100`（平台账户）路由
- 双分录的两条 entry 的 user_id 会被业务侧保证路由到同一物理分片（同 user_id 或同 user_id%100）→ 单库事务

## 日切

- 凌晨 cron：扫所有 account 余额做快照（`account_balance_snapshot_NN`）
- 试算平衡（`RunTrialBalance`）验证借贷恒等
- 卡死分片可通过 `/admin/day-cut/resume` 重新派发

## 依赖约束

- proto 里 `account_no` 字符串格式 `{dbIdx}{tblIdx:02d}{user_id}-{business_type}`（fleet）或 `{dbIdx}{tblIdx:02d}{user_id_last8:0>8}-{business_type}`（用户）
- AccountingEntry 的 DebitMoney / CreditMoney 二选一带值，两个都 0 会被 `validate entries` 拒
- TCC 幂等键 `request_id` 必传；重复传返回首次结果（不会重复记账）
- `currency` 字段必填，不在 `precisionMap` 的会直接拒（内置 15+ 币种）
