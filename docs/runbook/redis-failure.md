# Redis 故障处置 SOP

本 runbook 针对 accounting-system 热路径所依赖的 Redis。MySQL 是资金真理源，
Redis 只是派生的高频缓存；**任何 Redis 故障都可以通过"从 MySQL 重建"恢复**，
本文说明什么时候触发重建、怎么判断是否安全、按什么顺序操作。

> 适用于 **1 master + 2 replica + 3 sentinel + AOF everysec** 的当前部署
> （payment-admin-web/stack/docker-compose.yml）。拓扑：
>
> ```
> accounting-system ──(go-redis FailoverClient)──▶ 3 Sentinel (quorum=2)
>                                                    │
>                                                    ▼ 发现当前 master
>                                                 redis-master (RW)
>                                                    │ replication
>                                              ┌─────┴─────┐
>                                        redis-replica-1   redis-replica-2 (RO)
> ```
>
> master 单点故障时 sentinel 在 ~5s 内选出新 master，客户端自动重连新 master，
> 写路径恢复；期间热路径可能短暂返回 `connection reset` 由调用方重试吃掉。

---

## 1. 触发重建的常见信号

按严重程度从低到高：

| 信号 | 检测方式 | 处置级别 |
|---|---|---|
| Redis 重启后 idem key 丢失 | `HGET balance:<acc> balance` 返回 nil，但 account 表正常 | **日常恢复**：直接走 §4 走完 |
| gRPC 日志出现 `ErrCacheWarming` 激增 | `grep "account cache warming" logs` | **日常恢复** |
| Redis 进程 OOM / 容器 restart | Docker/k8s 事件，或 `redis-cli info memory` | **日常恢复** |
| `redis-cli DEBUG LOADAOF` 报 AOF 损坏 | Redis 启动日志 / `DEBUG LOADAOF` 输出 | **降级恢复**：先看 §5 |
| 热账户 Redis 余额 ≠ MySQL account.balance | `/redis-rebuild` dry-run diff 不全为 0 | **按阈值判断**，见 §3 |
| 对账告警（reconciliation worker） | Prometheus `reconciliation_mismatch_total` 抖动 | **需审计**，见 §6 |

"日常恢复"以下都可以由值班运维一个人按流程执行，不需要升级审批。"降级恢复"及
以上需要找 OWNER 确认。

---

## 2. 预处置：先别急着按按钮

决定做任何写操作之前，**先做这三件事**：

### 2.1 Freeze 热路径（避免边恢复边污染）

在 `/hot-accounts` 页面点 **"热重载白名单"** 前，先：

```
POST /admin/hot-accounts/{id}    body: {"enabled": false, ...}
```

把受影响的账户临时从白名单里摘掉 → 重新加载 → 这些账户的记账瞬时**回落 TCC 路径**。
TCC 读 MySQL，是权威的，不会读脏 Redis。

若受影响范围不明，直接把 hot_path.enabled 关掉更简单（需要改 config 重启）。

### 2.2 备份 Redis 当前状态

即便是脏数据也先备份——万一重建操作错了好回滚：

```bash
docker exec redis redis-cli BGSAVE
docker cp redis:/data/dump.rdb ./redis-dump-$(date +%Y%m%d%H%M%S).rdb
```

### 2.3 用 Dry-run 看 diff

进 admin-web **Redis 重建** 页面，**开着 dry-run**，时间点选 "当前 MySQL 余额"。
执行后看报告（每个账户的 balance_before / balance_after / 差量 / 状态）。

**不看 diff 就直接执行 = 危险**。

---

## 3. 如何读 dry-run 报告

报告里每行一个热点账户。重点看几个字段：

| 字段 | 含义 | 怎么判断 |
|---|---|---|
| `balance_before` | Redis 当前值 | 空/`cache miss` → Redis 没这个账户，正常重建 |
| `balance_after` | 目标值（从 MySQL 推出） | |
| `delta` | `balance_after - balance_before` | |
| `source` | `account` 或 `transaction_journal` | `transaction_journal` 只出现在时间点回放 |
| `状态` | `一致` / `+x` / `-x` / `N/A` | |

### 3.1 阈值：什么 diff 可以直接重建

按差量规模分三档：

**🟢 绿色（直接执行）**

- 全部账户 `delta == 0`：Redis 和 MySQL 一致，重建是 no-op。
- 约 50%+ 账户 `cache miss`，其余 `delta == 0`：典型的 Redis 重启后冷启动，直接执行。

**🟡 黄色（执行前留证据）**

- 少数账户（< 10%）`delta != 0` 且 `|delta| < 1 PHP`（100 minor unit）：buffer 滞后/精度毛刺。截图报告存档，正式执行。
- 个别账户 `delta != 0`，但绝对值等于**一笔 inflight voucher 的金额**：可能是 Redis 应用了某笔、MySQL outbox 还没落。先等 30s 再 dry-run 一次，等 outbox 追上；追平了再执行。

**🔴 红色（停下来）**

- 多个账户 `delta != 0` 且方向不一致（有的正有的负）。不是单纯的 lag，**可能有真实对账问题**。
- 单账户 `|delta|` 大于你们日均流水的 10%。
- 某个账户的 `balance_after` 是负的，但 account_type 是不可负账户（USER/MERCHANT）—— **不要重建**，MySQL 本身可能已经错了。

🔴 情况下，**不要执行重建**，挂升级给 OWNER + DBA，先查对账。

---

## 4. 执行重建（标准流程）

### 4.1 全量重建（Redis 重启后最常见）

```
/redis-rebuild 页面
  时间点:     当前 MySQL 余额
  账户号过滤:  留空
  Dry-run:    先开 → 看报告 → 按 §3 判断 → 关掉 dry-run 再执行
```

执行完：
- 核对 `updated + skipped + failed == total`
- `failed == 0`
- 去 `/accounts` 抽查 3~5 个刚重建的账户，确认页面显示的余额 = 预期

### 4.2 批量过滤重建（只恢复部分账户）

如果只有几个账户出问题（比如 cache 被误 FLUSHKEY），填入账户号列表：

```
账户号过滤:  010100001-001, 010100002-002
```

好处：不动其他正常账户，爆炸半径小。

### 4.3 回滚重建（怀疑最近 N 分钟写入被污染）

场景：发现热路径跑了一段时间后，某些账户 Redis 和 MySQL 都偏了。典型是上线
了一个买的 worker 误 HSET 了 Redis。

```
/redis-rebuild 页面
  时间点:     回滚到 N 分钟前（选一个污染开始之前）
  Dry-run:    先开
```

内部走 `transaction_journal` 模式，取该时间点最后一笔 transaction 的 `balance_after`
当目标。

**前提**：MySQL 流水没被污染。如果怀疑 MySQL 也有问题 → 不要用这个工具，找 DBA。

---

## 5. 降级恢复：AOF 损坏 / RDB 不完整

Redis 启动时报 `Short read or OOM loading DB` 之类：

1. **停 accounting-system**（不停它会一直连不上 Redis 狂打日志）
2. 把 `/data/appendonly.aof` 和 `/data/dump.rdb` 备份走
3. 尝试 `redis-check-aof --fix /data/appendonly.aof`（会截断到最后一条完好命令）
4. 若 AOF 完全废了 → 直接 `rm appendonly.aof`，让 Redis 从 RDB 启动（最多丢 5 min 数据，RDB snapshot 窗口）
5. Redis 起来 → 走 §4.1 全量重建
6. 重启 accounting-system

数据丢失边界：**最多丢 `appendfsync everysec` 1s 的写 + RDB snapshot 5 min 粒度**。
这部分丢失由重建工具从 MySQL 补齐，不产生资金缺口。

### 5.1 master 单点 down（HA 自动切换）

1. Sentinel 检测 `down-after-milliseconds=5000` 后选出新 master（从 2 个 replica 里
   挑 replication offset 最大的那个）
2. `failover-timeout=10000` 给整个 failover 流程兜底；通常 5–10 s 完成
3. go-redis FailoverClient 通过 sentinel 感知新 master 地址，自动切连接
4. 期间 inflight 的写入可能拿到 `connection reset` → accounting-system 层面
   由业务重试（TCC / outbox recovery）吃掉
5. **原 master 回来后默认变 replica**（sentinel 会 SLAVEOF newmaster），不需要手动干预

**只要 down 的是 master 单点，应用层几乎无感**。但要确认：
- `sentinel ckquorum mymaster` 返回 `OK 2 voters` 以上
- `info replication` 在新 master 上显示 `role:master`、在老 master/replica 上显示 `role:slave`
- accounting-system 日志里没有 `cache warming` 激增（说明客户端平滑切换）

### 5.2 replica 单点 down

完全无影响 —— 读写都走 master，replica 只是异步复制目标。

等 replica 恢复后 sentinel 会自动让它重新 `SLAVEOF master` 同步。量大的话首轮
同步走 RDB，可能消耗 master 几十秒 CPU；非重大故障期尽量选择业务低谷做。

### 5.3 Sentinel 单点 down

quorum=2，挂 1 个 sentinel 仍能选主。挂 2 个就没法 failover，但正常读写不受
影响。三个 sentinel **不要部署在同一台物理机 / 同一个 docker host**，否则单点
故障会带掉 quorum。

Stack 里三个 sentinel 都跑在同一台开发机是**可以接受的妥协**（反正 Docker host
挂了 master 也挂了）；生产应该分机柜 / 可用区。

---

## 6. 对账告警怎么办

`reconciliation worker` 报错（Prometheus `reconciliation_mismatch_total > 0`）说明
MySQL 流水加总 ≠ MySQL account.balance。**这不是 Redis 问题**，是 MySQL 自己不
自洽。

- **不要跑重建工具**（重建不会修 MySQL）
- 查 worker 日志找具体偏差账户
- 用 `/platform-accounts/snapshots` 比较今日 / 昨日 snapshot 差
- 升级给 OWNER + DBA

---

## 7. 升级路径与联系人

| 严重度 | 角色 | 联系 |
|---|---|---|
| 日常恢复（§1 表格前 3 项） | 值班运维 | 按 §4 流程自助 |
| 降级恢复（§5） | 平台 SRE | 群内 @oncall |
| 对账不一致（§6） | DBA + 会计系统 OWNER | 电话 |
| 资金差额 > 日均 1% | OWNER + 财务 | 电话 + 建 incident |

---

## 8. Post-incident 必做

任何一次动过 Redis 的操作（重建、修 AOF、清 key），事后 24h 内：

1. 抓一份 `/admin/platform-accounts/balances?business_type=<每个活跃 bt>` 的快照
2. 和事件前最近的 day-cut snapshot 比对
3. 写入 `incidents/` 目录下的 postmortem 文件

保留原因：热路径重建后可能触发的 lingering 问题（idem key TTL 期内的老 voucher
replay）要在 24h 内暴露出来。

---

## 9. 工具索引

| 工具 | 调用方式 | 用途 |
|---|---|---|
| admin-web `/redis-rebuild` | 浏览器 | 有人看着按的标准入口 |
| `cmd/tools/redis-rebuild` CLI | `go run` / 打包二进制 | admin-web 挂了的兜底 |
| `redis-cli -h redis-master` | docker exec redis-master | 当前 master 状态 / HGET / FLUSHKEY |
| `redis-cli -h redis-sentinel-1 -p 26379 SENTINEL masters` | 看 sentinel 视角下当前 master | |
| `redis-cli -h redis-sentinel-1 -p 26379 SENTINEL ckquorum mymaster` | 检查 quorum 是否够 failover | |
| `redis-cli SENTINEL failover mymaster` | 手动触发 failover（演练用） | |
| `redis-check-aof` | docker exec | 修 AOF |

CLI 用法：

```bash
# dry-run
docker exec accounting-system /usr/local/bin/redis-rebuild \
    -addr  http://localhost:8888 \
    -token "$ADMIN_TOKEN" \
    -dry-run

# 正式执行，回滚到 10 分钟前
docker exec accounting-system /usr/local/bin/redis-rebuild \
    -addr  http://localhost:8888 \
    -token "$ADMIN_TOKEN" \
    -as-of 10m
```

---

## 10. 已知残留风险

- **幂等键 TTL 超 24h**：如果一笔 voucher 的 outbox 卡在 PENDING 超 24h（DB 宕
  这么久），Redis 的 idem key 可能过期；recovery 重推时会 double-apply。概率极
  低，但发生了只能人肉查 + 用重建工具回滚到事件之前。
- **Redis 集群化未支持**：目前 Lua 不做多 slot 编排。要上集群需先改 key 加 hash
  tag（见 docs/architecture 中 Lua 章节，如后续新增）。
- **OutboxWorker 卡死超过数小时**：recovery 会多次 IncrementRetry 达 `maxRecoveryRetries`
  上限被置 FAILED，资金事件不会再自动恢复，需人工看 `CRITICAL: outbox recovery
  permanently failed` 日志处理。
