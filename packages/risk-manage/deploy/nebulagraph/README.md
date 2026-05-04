# risk-manage NebulaGraph 接入

实时图计算后端。risk-manage 的 LinkStore（device-customer-card-IP 关联图）
默认 mem 实现，单进程 1 小时 TTL；生产换 NebulaGraph 跑实时多跳查询：
- 同 device 7 天内 N 个 user → 卡共享
- 同 customer 多张卡多张 IP → 拖库 / 撞库
- IP 与多个 device 关联 + 任一带 fraud tag → 通过图传播 raise risk

## 启用步骤

1. `go.mod`：
   ```
   require github.com/vesoft-inc/nebula-go/v3 v3.7.0
   ```
2. 删 `internal/store/nebula_linkstore.go` 顶部 `//go:build nebula`。
3. 修改 `cmd/server/main.go` 的 `newLinkStore`：
   ```go
   func newLinkStore(v *viper.Viper, logger *zap.Logger) store.LinkStore {
       if v.GetString("linkstore.nebula.addr") == "" {
           return store.NewMemLinkStore()
       }
       hostList := []nebula.HostAddress{
           {Host: v.GetString("linkstore.nebula.addr"), Port: 9669},
       }
       pool, err := nebula.NewConnectionPool(hostList, nebula.GetDefaultConf(), nebula.DefaultLogger{})
       if err != nil {
           logger.Warn("nebula connect failed; fallback mem", zap.Error(err))
           return store.NewMemLinkStore()
       }
       space := v.GetString("linkstore.nebula.space")
       if space == "" {
           space = "risk_graph"
       }
       return store.NewNebulaLinkStore(pool, space, logger)
   }
   ```

## Schema 初始化

`schema.ngql` 在本目录。首次部署：

```bash
nebula-console -addr nebula -port 9669 -u root -p nebula \
    -e "CREATE SPACE risk_graph(partition_num=10, replica_factor=3, vid_type=FIXED_STRING(32));"
sleep 10  # 等 space 创建
nebula-console -addr nebula -port 9669 -u root -p nebula \
    -f deploy/nebulagraph/schema.ngql
```

## TTL 清理任务

NebulaGraph OSS 没有原生 TTL；`links.expires` 字段需要外部 cron 清。
本目录 `cleanup.ngql` 是定期跑的脚本（推荐 5min 一次）：

```ngql
-- 找所有过期 edge → 删
GO FROM "all_known_hub_vids" OVER links REVERSELY
WHERE links.expires < now()
| DELETE EDGE links;
```

或更精细：起一个 Go 定时任务遍历 `last_active` 节点，按 hop 清理。

## 性能基线

| 操作              | 单机 NebulaGraph (3 节点) | mem |
|------------------|----------------------|-----|
| Link (单笔写)     | ~3-5ms (含 round-trip) | < 1µs |
| Peers (1-hop)    | ~1-2ms                | ~10µs |
| PeersWithin (2-hop) | ~5-10ms             | ~100µs |
| PeersWithin (3-hop) | ~50-200ms (cap=200 node) | ~10ms |

> hot path Screen 调 1-hop / 2-hop 是安全的；3-hop 应该走异步 enrich 不在
> Screen 主路径上跑。

## Tag 设计

- `identity(key)` — 所有节点的 tag，唯一索引在 `key` 上
- `suspect(label, ts)` — fraud 信号 tag（device 被标 known-bad 等）
- `goodwill(label, ts)` — 已知好信号（VIP customer 等）

Tag 名约定 `tag_xxx`，运营 admin 可以加 tag 实时影响图查询：

```
POST /admin/graph/tag
Body: {"node":"device:abc","tag":"suspect_card_test","ttl_hours":24}
```

> 此 admin endpoint 当前未实现；要加见 `internal/store/linkstore.go` 接口
> 已有 `Tag(ctx, node, tag)`，套一个 HTTP wrapper 即可。

## 监控

- `nebula_query_latency_seconds{p99}` > 50ms 持续 → 索引没建好 / VID 冲突
- `nebula_storage_disk_usage_pct` > 80% → 加 retention task / 扩盘
- 缓存命中率：风控热点 device/customer 应该在客户端缓存 + Nebula 二级
- `risk_graph_query_failed_total{reason}` 应用层熔断器（可加在 NebulaLinkStore）
