# clearing-settlement

T+1 批量清算 / 结算服务。在 accounting-system 完成日切后，把商户的"待结算"余额（MERCHANT_PENDING_SETTLE）按规则移到"可用"余额（MERCHANT_BALANCE）并产生结算记录。

## 定位

```
accounting-system.day-cut.completed  ─event/cron→  clearing-settlement
                                                         │
                                                         └─gRPC─→  accounting-system
                                                              （DoubleEntryBooking
                                                               转账：PENDING_SETTLE → BALANCE）
```

本服务**自己不算钱**，所有动账走 accounting-system。本服务只决定"谁该结算多少"+ 记审计 + 重试 + 幂等。

## 分片

10 dbs × 10 tables = **100 分片**，按 `merchant_id` hash 路由（与 accounting-system 同模型）。
- `settlement_run_NN`（00-99）：每分片每次 run 一行，记 (settle_date, run_id) 在该分片的进度
- `settlement_record_NN`（00-99）：单 merchant 单次结算明细，按 merchant_id 路由

同 merchant 永远落同一物理表 → 单库事务 + 顺序扫描友好。
Admin `/admin/settle/status` 并发查所有 100 分片再聚合。

## 核心模型

| 模型 | 表 | 语义 |
|---|---|---|
| SettlementRun | `settlement_run_NN` | 单分片在某次运行的状态机：PENDING → PROCESSING → COMPLETED / FAILED；联合唯一 (db_idx, tbl_idx, settle_date, run_id) |
| SettlementRecord | `settlement_record_NN` | 单 merchant 单次结算明细：(settle_date, merchant_id, run_id) → amount + status + voucher_no |

run_id 自增允许重跑：同一 (settle_date, merchant_id) 有 COMPLETED record 时跳过（幂等）。

## 触发方式

1. **Cron**：每日 N 点（settle_time，默认 02:00 Local）扫所有商户。要求当天日切已全部 COMPLETED 才开跑（否则数据未冻结）。
2. **Admin**：`POST /admin/settle/trigger {settle_date}` 立即触发；用于补跑 / 测试。
3. **Resume**：`POST /admin/settle/resume {settle_date, run_id}` 重新派发卡死的 PROCESSING merchant。

## 设计原则

- **幂等**：同 merchant 同日结算只执行一次。重复触发返回首次结果。
- **崩溃安全**：每个 merchant 是独立 record；服务挂了重启从游标继续。
- **不阻塞日切**：本服务故障不影响 accounting-system；账实仍可信。
- **金额精度**：透传 accounting-system 的 minor units × 100 模型，不做本地浮点。

## 接口

- gRPC（:9890）：`SettlementService` `TriggerSettlement / GetSettlementStatus / ListSettlementRecords`
- Admin HTTP（:9891）：`/admin/health`、`/admin/settle/trigger`、`/admin/settle/resume`、`/admin/settle/status`
- Metrics HTTP（:9892）：`/metrics`

## 依赖约束

- 必须等当天日切 100 分片全部 COMPLETED 才开跑
- 商户结算规则（频率：日 / 周 / T+N；最低门槛）走 yaml + 热重载
- 调 accounting-system 必须传 request_id（同 (settle_date, merchant_id, run_id) 推导，重跑生成同样 ID）
