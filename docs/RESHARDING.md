# Resharding SOP

为什么需要：当前 sharding layout 写死 10 库 × 10 表 = **100 个 globalTableIdx**，account_id 按位编码里 `globalTblIdx` 只占 2 位（最大 99）。要扩到 200 / 1000 shard，需要 layout 改造 + 数据迁移。

本文档描述：(1) 何时该 reshard；(2) 升级路径；(3) 风险点；(4) 演练 checklist。

---

## 1. 何时该 reshard

按下列任一条件触发评估：

| 信号 | 阈值 |
|---|---|
| 单 shard MySQL 数据量 | > 100 GB（接近 InnoDB buffer pool 比例失效） |
| 单 shard QPS（按 7 天 P95）| > 5_000（开始打热点 row lock） |
| 全局每秒 booking 笔数 | > 80_000（单 shard 5K × 16 起延迟漂移） |
| 单 shard 备份恢复时间 | > 4 小时（出问题 RTO 不达标） |

**优先做完 P1 容量优化再考虑 reshard**：
- BufferedBalanceWorker leader election（避免多副本互相挤 DB）
- DayCut 改并发 worker pool（减少日切阻塞）
- Trial balance 100-shard 串行 → 并发 + cache
- Leaf segment 步长动态化

这些项做完 + 真实流量验证后，单 shard 容量上限可能再翻一倍，reshard 计划至少推迟 1 年。

---

## 2. 升级路径概览

**禁止做的事：**
- 直接改 `ShardDBCount` 常量从 10 到 20 → account_id layout 的 `globalTblIdx` 字段只有 2 位，新 shard 编不进去
- 跳过 router versioning 直接覆盖路由函数 → 老数据用不同 hash 取模，路由错位

**正确路径：**

```
┌─────────────────────────────────────────────────────────────┐
│ Phase 1: layout 改造（线下，无流量影响）                    │
│   - identity.go 增 globalTblIdx 字段位数（2→3 = 支持 1000） │
│   - 新增 RouterV2 实现新 layout 的路由函数                   │
│   - 单元 / 集成测试覆盖新旧 router 的 dual-read 一致性        │
├─────────────────────────────────────────────────────────────┤
│ Phase 2: 部署 v2 但不切流量                                  │
│   - 全栈服务升级：accounting-system / order-core / 等          │
│   - CurrentRouterVersion 仍 = V1（写还走老 router）           │
│   - 启动 RouterDualRead 模式：读优先 v1，miss 时再尝试 v2     │
├─────────────────────────────────────────────────────────────┤
│ Phase 3: 双写 + 数据搬迁                                      │
│   - 加 shard MySQL 实例（dbCount: 10 → 20）                   │
│   - 起 migrate worker：扫 v1 表 → 重新算 v2 路由 → 双写新表    │
│   - lag 监控：v1 与 v2 数据 diff 必须保持 < 1000 行          │
│   - 持续若干天 / 周直到 v1=v2 完全一致                       │
├─────────────────────────────────────────────────────────────┤
│ Phase 4: cutover                                              │
│   - 设 CurrentRouterVersion = V2（写一律落新 layout）         │
│   - 监控 1 周，确认无路由错位 / 数据不一致                    │
│   - 关闭 dual-read，清理 v1 表                                │
├─────────────────────────────────────────────────────────────┤
│ Phase 5: 收尾                                                 │
│   - 删 RouterV1 实现                                          │
│   - account_id 历史值仍按老 layout 解码（DecodeAccountID 兼容）│
│   - 新建 account 都用 V2 layout，老账户保留原 ID 不动          │
└─────────────────────────────────────────────────────────────┘
```

工期估计：3-6 个月（取决于数据量；100M+ 行的双写迁移最痛苦在 Phase 3）。

---

## 3. 风险点 + 缓解

| 风险 | 描述 | 缓解 |
|---|---|---|
| account_id 跨版本不可比 | v2 编码出的 ID 解码用 v1 函数会算错路由 | DecodeAccountID 看 layout version 字段（高位预留 1 位）→ 自动选解码版本 |
| Fleet 账户重新分配 | 主流量 fleet user_id [1e6, 1e7) 容量 9M。多 channel × 多 shard 会撑爆 | 用 `FleetUserID(ctx, channelCode, gtbl)`，channelStride=1000 已为扩 4× shard 预留 |
| TCC 跨版本事务 | Try 阶段写老表，Confirm 阶段已切 v2，写新表 → 不一致 | Phase 4 cutover 前先 drain 所有 in-flight TCC（waitForDrainTCC），再切 |
| Outbox claim_token 跨版本 | 老 outbox 行的 claim_token 不会被新副本 reclaim → 卡死 | 升级 outbox worker 时双向兼容，cutover 后跑一遍 unclaim-stale-rows |
| Shadow 表数据膨胀 | 双写期间 _shadow 表也跟着翻 2x → 主库 IO 翻倍 | reshard 期间禁止全量压测（只做 sample 流量验证） |
| Reverse 路由（已有 ID 找 shard） | 老 ID 解码版本错 → 调度到新 shard 找不到行 | DecodeAccountID 强制兼容老 layout；router.RouteByAccountNo 不再依赖前 3 字符（已切按位 layout） |

---

## 4. 演练 checklist

reshard 启动前 **必须**先完成下面所有项：

- [ ] 在 staging 跑通 Phase 1-5 全流程，模拟 1M 行真实流量数据
- [ ] dual-read miss 率监控：staging 7 天数据 < 0.01%
- [ ] DBA 评估新 shard MySQL 实例的 binlog / replication / backup 容量
- [ ] 业务方知会：reshard 期间 cross-shard 查询（trial balance / day cut）延迟会临时翻倍
- [ ] rollback 路径：从 Phase 4 倒回 Phase 3 的步骤明确（CurrentRouterVersion 改回 V1，新数据继续双写）
- [ ] 监控 dashboard：新增 dual-read miss / cross-version data-diff / migration lag 三个 SLO
- [ ] runbook 完整：每个阶段 + 每个失败模式都有 1-page 说明

---

## 5. 长期：避免再 reshard 的设计

- account_id layout 的 `globalTblIdx` 直接给 3 位（支持 1000 shard）—— 已在 P1-3 长期改进项规划
- 所有新 ID 设计统一走 `payment-util/shadow.EncodeID` 通用编码，分配 shard 字段时给充足位数
- Fleet user_id 通过 `FleetUserID(ctx, channel, shard)` 多维分配，避免单维拍扁

未来 reshard 频率目标：**至少 5 年一次**（不是 5 个月一次）。
