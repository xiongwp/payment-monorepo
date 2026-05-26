# NebulaGraph 部署指南（risk-manage LinkStore 后端）

risk-manage 的 `LinkStore` 默认走 process-local mem，单机 < 1M 边；规模到
device-customer-card-IP 1 亿+ 边后只能上分布式图库。这里给 NebulaGraph 的
生产部署 + schema 初始化 + 接入 risk-manage 的全套步骤。

> 实现代码：`internal/store/nebula_linkstore.go`（`//go:build nebula`）。
> 测试代码：`internal/store/nebula_linkstore_test.go`（同 build tag）。
> wiring：`cmd/server/linkstore_nebula_enabled.go`（同 build tag）。

---

## 集群拓扑

最小生产部署 = 3+3+3 三组件：

| 组件       | 数量 | 角色                                         | 资源建议（1 亿边场景） |
| --------- | ---- | -------------------------------------------- | --------------------- |
| metad     | 3    | 元数据 raft 集群（space / schema / 心跳）    | 2C 4G + 50G SSD       |
| storaged  | 3    | 实际存边 + raft 复制；按 partition 切片      | 8C 32G + 200G NVMe    |
| graphd    | 3    | 查询计算层（无状态）；client 负载均衡接它    | 8C 16G                |

- metad 永远 3 节点（raft；多了浪费、少了不可用）
- storaged 至少 3（`replica_factor=3`）；扩容时按 partition 数对齐加节点
- graphd 按 QPS 横向扩；nebula client SDK 自带负载均衡

---

## SPACE / TAG / EDGE 初始化

```nGQL
-- 1. 建 space（首次 ONLY）
CREATE SPACE IF NOT EXISTS risk_graph (
    vid_type        = FIXED_STRING(32),
    partition_num   = 100,    -- 节点数 × 2；扩容到 50 个 storaged 时不用重建
    replica_factor  = 3       -- raft 三副本，挂一个节点不影响读写
);

-- 等 ~10s leader 选举
USE risk_graph;

-- 2. 节点 / 边 / 标签
CREATE TAG IF NOT EXISTS identity (
    key string NOT NULL
);

CREATE TAG IF NOT EXISTS suspect (
    label string NOT NULL,
    ts    timestamp NOT NULL
);

CREATE TAG IF NOT EXISTS goodwill (
    label string NOT NULL,
    ts    timestamp NOT NULL
);

CREATE EDGE IF NOT EXISTS links (
    at      timestamp NOT NULL,    -- 最近一次共现时间（每次 Link 更新）
    expires timestamp NOT NULL     -- linkTTL = 1h 后过期；cron 清扫用
);

-- 3. 索引（必须，否则 LOOKUP / GO ... WHERE 报错）
CREATE TAG INDEX IF NOT EXISTS identity_key_idx   ON identity(key(64));
CREATE TAG INDEX IF NOT EXISTS suspect_label_idx  ON suspect(label(40));
CREATE TAG INDEX IF NOT EXISTS goodwill_label_idx ON goodwill(label(40));
CREATE EDGE INDEX IF NOT EXISTS links_at_idx      ON links(at);
CREATE EDGE INDEX IF NOT EXISTS links_expires_idx ON links(expires);

-- 4. 建索引后必须 rebuild 一次
REBUILD TAG INDEX identity_key_idx, suspect_label_idx, goodwill_label_idx;
REBUILD EDGE INDEX links_at_idx, links_expires_idx;
```

VID 设计：用 `sha256(key)[:16]` 转 hex 得到 32 char fixed-string VID，正好
匹配 `vid_type = FIXED_STRING(32)`。详见 `nebula_linkstore.go:vidOf()`。

---

## 参数调优

| 参数               | 推荐值        | 备注                                              |
| ------------------ | ------------- | ------------------------------------------------- |
| `partition_num`    | 节点数 × 2    | 扩容时无需重建，最多到 partition_num               |
| `replica_factor`   | 3             | 1 副本只做测试；生产 raft 三副本                   |
| `wal_ttl`          | 14400 (4h)    | WAL 保留时间，影响节点故障恢复窗口                  |
| `rocksdb_block_cache` | 8GB / 节点 | storaged 节点内存 1/4；命中率 > 95% 时调到 1/2     |
| `query_concurrently` | true       | graphd 多 partition 并发拉取                       |

---

## 容量规划

1 亿条边场景：

- **边平均大小**：`(8 src VID) + (8 dst VID) + (8 at) + (8 expires) + (overhead) ≈ 64 byte`
- **3 副本占用**：`1e8 × 64 × 3 ≈ 19 GB` 仅边数据
- **加索引 + WAL + RocksDB amplification ~3x ≈ 60 GB`
- **3 storaged 节点**：每节点 200 GB NVMe SSD 留 70% buffer → 撑到 3 亿边

10 亿边：上 6+ storaged，partition_num=200，节点盘扩到 1TB。

---

## TTL 清理（OSS 没原生 TTL）

`links.expires` 是 1h 后的 unix 时间戳；过期边由 cron 清。推荐 5min 一次：

```nGQL
-- 找出所有 expires < now() 的边并删除（按 known-hub 节点遍历，避免全表扫）
GO FROM <hub_vids> OVER links REVERSELY
WHERE links.expires < now()
| YIELD src(EDGE) AS s, dst(EDGE) AS d, type(EDGE) AS t
| DELETE EDGE links s -> d;
```

更简单的做法：维护一个 `active_keys` Redis set，定时把活跃 key 喂给上面的
GO 语句作为 `<hub_vids>`。冷 key 让自然时间淡出，过期边不删不影响查询正确性
（risk-manage 自带 `links.expires > now()` 过滤）。

Enterprise 版可以直接：

```nGQL
ALTER EDGE links TTL_DURATION = 3600, TTL_COL = "at";
```

---

## risk-manage 接入

1. **编译**：

   ```bash
   go build -tags nebula -o bin/risk-manage-server ./cmd/server
   ```

2. **配置**（`config.yaml`）：

   ```yaml
   linkstore:
     nebula:
       addrs:
         - "nebula-graphd-0:9669"
         - "nebula-graphd-1:9669"
         - "nebula-graphd-2:9669"
       user: "root"
       password: "${NEBULA_PWD}"  # 推荐 env 注入
       space: "risk_graph"
   ```

3. **验证**：起服务后查 log 有 `nebula linkstore initialized`；连不上自动 fallback
   mem 并打 warn —— 服务起得来，但风控 LinkStore 路径降级。

---

## 监控

应用层 metric（risk-manage 自带）：
- `linkstore_op_duration_seconds{op, store="nebula"}` —— Link / Peers / WeightedFanout
- `linkstore_op_failed_total{op, reason}` —— nebula 错误码分布
- `nebula_session_pool_active{}` —— 在用连接数；接近 MaxConnPoolSize 要扩

NebulaGraph 自带 metric（`http://graphd:19669/stats`）：
- `num_queries_succeed / num_queries_failed`
- `slow_query_count`（> 200ms）
- `storage_disk_usage_pct` —— > 80% 告警

应用层熔断：连续 N 次 nebula 失败 → 临时降级 mem（每条 Link 双写 mem 兜底）。

---

## 性能基线

| 操作              | NebulaGraph (3 节点) | mem      |
| ----------------- | -------------------- | -------- |
| Link (单笔双向写) | 3-5 ms               | < 1 µs   |
| Peers (1-hop)     | 1-2 ms               | ~10 µs   |
| PeersWithin 2-hop | 5-10 ms              | ~100 µs  |
| PeersWithin 3-hop | 50-200 ms (cap=200)  | ~10 ms   |
| WeightedFanout    | 3-5 ms（含本地 math）| ~50 µs   |

**Hot path 安全**：Screen 走 1-hop / 2-hop。3-hop 应该走异步 enrich，不在
同步 Screen 路径上跑。

---

## 故障演练

| 场景              | 期望行为                                                |
| ----------------- | ------------------------------------------------------- |
| 杀 1 个 storaged  | 读写不影响（raft 三副本）；leader 切换 ~1s             |
| 杀 1 个 graphd    | 客户端切别的 graphd；约 50ms 抖动                       |
| 杀 1 个 metad     | 读写不影响（DDL 暂不能跑）；leader 选举 ~3s            |
| 杀 2 个 storaged  | 该 partition 不可写（quorum 丢）；读仍能从最后副本走  |
| 全集群挂           | risk-manage Healthy()=false；newLinkStore 不会自动切    |
|                   | mem（已 init 走的是 nebula 引用）；要 manual restart    |

建议 chaos test 用 `cmd/chaos/chaos.sh nebula-storaged-down` 跑回归。
