# Storage Layers

按架构图三类存储 → 抽象接口 → 默认 / 生产实现。

```
                     架构图                         代码 接口                      默认实现             生产实现
─────────────────────────────────────  ──────────────────────────  ────────────────  ─────────────────────
Feature DB (Redis/PG)                  store.Counter               MemCounter        Redis (示例 below)
                                       session.Store               MemStore          Redis
                                       store.Blacklist             MemBlacklist      Redis SET / PG
                                       mlscore.Service             NoopService       gRPC（TF Serving / Triton）
                                       ipintel.Service             MemService        MaxMind / IPQS / IPInfo

Graph DB (Neo4j)                       store.LinkStore             MemLinkStore      Neo4j / RedisGraph

Log System (ClickHouse)                audit.Sink                  LogSink+MemSink   Kafka → ClickHouse
                                                                                      append-only audit DB
```

## 接口契约（替换实现时遵守）

### `store.Counter`
- `GetDaily / GetMonthly`: 按 key 取累计金额；不存在返 0
- `GetVelocity`: 滑窗 N 分钟笔数，O(1)（实现可走时间分桶）
- `Incr`: 原子累加 daily + monthly + velocity 三个维度
- 线程安全；无写丢失

### `store.LinkStore`
- `Link(a, b)`: 双向原子记录共现，TTL 1 小时
- `Peers(a, prefix)`: 返回 a 关联的全部 peer，可按 dim prefix 过滤
- 单 key 关联 peer 数硬上限（默认 1024，防关联爆炸）

### `audit.Sink.Write`
- 必须非阻塞（< 1ms）；慢实现内部用 channel buffer + worker
- 失败不能阻塞主路径；fail-open 不抛异常

### `session.Store`
- `Create` 返回 32-hex 不可枚举 ID
- 单 session TTL 30min
- `Finalize` 不覆盖 fingerprint 字段（只覆盖 behavior）

## Redis 适配示例

参见 `internal/store/redis_counter.go`（reference，仅 Counter）。

`go.mod` 加：
```
require github.com/redis/go-redis/v9 v9.x.x
```

构造：
```go
rdb := redis.NewClient(&redis.Options{Addr: "redis:6379"})
counter := store.NewRedisCounter(rdb, "risk:counter:")
```

完整生产部署还需要：
- LinkStore: HSET + EXPIRE 实现 / RedisGraph 用 `MATCH` 查 peer
- Blacklist: SET 包含 `(dim:value)` 简单成员检查
- session: HSET sess:<id> + EXPIRE 1800
- AuditSink: 客户端 producer 写到 Kafka topic，consumer 落 ClickHouse 长留存

## 配置切换

`config.yaml` 通过 `storage.driver` 切实现：

```yaml
storage:
  counter:    redis    # mem | redis
  blacklist:  redis
  links:      redis
  sessions:   redis
  audit:      kafka    # log | kafka

redis:
  addr: redis:6379
  db: 0

kafka:
  brokers: [kafka-1:9092, kafka-2:9092]
  audit_topic: risk.decisions
```

`cmd/server/main.go` 按 driver 选 newCounter / newLinkStore / etc 的具体实现。
当前都默认 mem，生产部署只改 yaml 即可。
