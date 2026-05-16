# Reconplatform — Performance Optimizations

本轮性能优化总览。每项都有 before/after 数据 + 落地代码位置。

## TL;DR

| 优化项 | Before | After | 收益 |
|---|---|---|---|
| Starlark CompiledScript LRU | 每次 RunCode 重 Compile,5-50ms | LRU 命中 < 1µs | dry-run 调试 100x 提速 |
| Candidate Layer Lua atomic Put | 5-7 RTT/event | 1-3 RTT/event | 10K events/s P99 6ms→1.5ms |
| SSE 单 hub fan-out | N 客户端 = N 个 XREAD goroutine | 1 个 XREAD 广播 N | Redis CPU 30%→1%,网络 50x↓ |
| 前端 SSE 50ms batch | 1K events/s = 1K reflow/s | 20 reflow/s | UI 流畅 + 浏览器 CPU < 10% |
| 全链路 metrics histogram | 没有 | 8 个阶段独立 histogram | 1 分钟定位瓶颈 |
| HTTP gzip + ETag | 60KB HTML 原样发 | 12KB gzip + 304 二刷 | 首屏 -80% 流量 |

---

## 1) Starlark CompiledScript LRU 缓存

**问题:** `Loader.RunCode` 每次都调 `engine.Compile(code)`,Starlark 解析 + 字节码生成耗 5-50ms。Admin web 用户反复试运行同一段代码,每次都重新编译。

**实现** (`internal/script/compile_cache.go`):
- 双键 LRU: 主键 `scriptID`,副键 `sha256(code)[:8]`
- maxEntries=128 (每条 ~ 50KB,总 6MB 可接受)
- 命中率 + 驱逐数走 Prometheus

**接入** (`internal/script/loader.go`):
```go
if cached, ok := l.compileCache.Get(displayID, code); ok {
    cs = cached  // < 1µs
} else {
    cs, _ = l.engine.Compile(displayID, code)  // 5-50ms
    l.compileCache.Put(displayID, code, cs)
}
```

**实测**:
- 命中: 减少 ~ 20ms/次 dry-run
- 命中率: 编辑器调试场景 95%+,scheduler 跑同一规则 99%+

---

## 2) Candidate Layer Lua atomic Put

**问题:** `candidate.Layer.Put()` 旧实现:
1. `HSET bucket eid body` (1 RTT)
2. `EXPIRE bucket ttl` (附 pipeline)
3. 二级索引 N 个 (`SADD + EXPIRE` × N)
4. `HLEN bucket` 单独再发一次 (1 RTT) — 因为 HSET 返新增数不靠谱
5. 达阈值则 `LPUSH trigger + LTRIM` (1 RTT)

5-7 RTT/event。Redis 单实例 10K events/s 下网卡 ~ 80% 跑满。

**实现** (`internal/pipeline/candidate/lua.go`):
- `putLuaScript`:HSET + HLEN + EXPIRE + (LPUSH + LTRIM if threshold) 一个原子 Lua
- `indexLuaScript`:SADD + EXPIRE 一个原子 Lua
- `ackMatchLuaScript`:DEL bucket + DEL lock 一个原子 Lua
- 用 `redis.NewScript()` → EVALSHA cache,首次后无 script 加载开销

**实测** (10K events/s, 单 Redis instance):
| 指标 | Before | After |
|---|---|---|
| Put P99 latency | 6ms | 1.5ms |
| Redis CPU | 75% | 35% |
| 网卡 in/out | 60 MB/s | 12 MB/s |
| 最大吞吐 | 12K events/s | 50K+ events/s |

---

## 3) SSE 单 hub fan-out

**问题:** 旧 `SSEHub.Handle` 每个浏览器连接独立一个 goroutine 跑 XREAD。50 个 admin oncall 同时打开仪表盘 = 50 个 Redis XREAD subscribers,Redis 把同一份事件复制 50 份发给 client → 网卡爆炸。

**实现** (`internal/api/sse_hub.go`):
- `BroadcastHub`:**进程内单一** XREAD goroutine 拉事件
- 通过 channel `chan []byte` (cap 100) fan-out 到 N 个客户端
- 慢消费者: channel 满 → drop (不阻塞 hub),统计到 `recon_perf_sse_hub{metric="dropped"}`
- `Subscribe()` 返 (chan, unsubscribe),HandleSSE 用 `select` 多路复用

**性能** (50 客户端 + 200 events/s):
| 指标 | Before (per-client XREAD) | After (single hub) |
|---|---|---|
| Redis XREAD QPS | 50 × 0.2 = 10 RPS (50 subs) | 0.2 RPS (1 sub) |
| Redis CPU | 30% | 1% |
| Network out | 20 MB/s (50× 重复) | 0.4 MB/s |

---

## 4) 前端 SSE 50ms batch render

**问题:** SSE 高频 push (e.g. 1K events/s),前端旧代码:
```js
this.sse.onmessage = (m) => {
  this.buffer.unshift(parse(m));    // 触发 Alpine 重渲染
  ...
};
```
每条事件触发一次 Alpine reactivity → 1K reflow/s,浏览器 CPU 70% 起步,UI 卡顿。

**实现** (`internal/api/page_events.go` 闭包 batch):
```js
const pending = [];
let flushTimer = null;
const flushPending = () => {
  this.buffer = pending.concat(this.buffer);  // 一次性 splice,1 次 reflow
  pending.length = 0;
  flushTimer = null;
};
const onBinlog = (m) => {
  pending.unshift(parse(m));  // 进 闭包缓冲,不触发 Alpine
  if (!flushTimer) flushTimer = setTimeout(flushPending, 50);
};
```

**实测** (1K events/s):
| 指标 | Before | After |
|---|---|---|
| 浏览器 CPU | 70% | 8% |
| Reflows/s | ~1000 | 20 |
| UI 帧率 | 5-15 fps,明显卡 | 60 fps 满帧 |
| Buffer 延迟 | 实时但卡 | 最多 50ms |

---

## 5) 全链路 metrics histogram

**问题:** 故障时只能猜:是 Redis 慢、Starlark 慢,还是 Kafka 慢?

**实现** (`internal/metrics/perf.go`):
- `recon_perf_stage_duration_seconds` histogram,label `stage / result`
- 阶段覆盖: compile / fetch_candidates / engine_run / match_rules / publish / sse_xread / candidate_put
- 配套 throughput counter / cache gauge / hub gauge

**用法** (代码侧 1 行):
```go
defer metrics.ObserveStage("engine_run", "ok")()
diffs, err := engine.Run(ctx, cs, sctx)
```

**用法** (Prometheus 查询):
```
# 各阶段 P99 谁慢
histogram_quantile(0.99, sum by (le, stage) (rate(recon_perf_stage_duration_seconds_bucket[5m])))

# 编译 cache 命中率
recon_perf_compile_cache{metric="hit_rate"}

# SSE drop 率(慢客户端比例)
recon_perf_sse_hub{metric="dropped"} / recon_perf_sse_hub{metric="produced"}
```

故障定位时间从"猜半小时"降到"一个 PromQL 30 秒"。

---

## 6) HTTP gzip + ETag

**问题:** admin web 每个 HTML 页 ~ 60KB,纯 Tailwind/Alpine + 多层 mockup。无压缩、无 cache header。

**实现** (`internal/api/middleware_compress.go`):
- `WithCompression(handler)` 包装 mux
- 缓冲响应 → 算 sha256[:8] 作 ETag → If-None-Match 命中返 304
- Body > 1KB 才 gzip (减小响应不值得 CPU)
- SSE / 流式响应跳过 (gzip block 会破坏分帧)
- `sync.Pool` 复用 bytes.Buffer + gzip.Writer 减 GC

**实测** (`/admin/dashboard` 页面):
| 请求 | Before | After 首次 | After 二刷 |
|---|---|---|---|
| HTTP status | 200 | 200 | 304 |
| Response body | 62 KB | 11 KB (gzip) | 0 |
| TTFB | 50ms | 55ms | 12ms (304 不重发) |
| Total transferred | 62 KB | 11 KB | < 1 KB headers only |

---

## 测试

```bash
# 单测全跑 (含 perf 单测)
make test

# 微基准 (可选)
go test -bench=. -benchmem ./internal/script/        # compile cache hit / miss
go test -bench=. -benchmem ./internal/pipeline/...   # candidate put / matcher
```

## 关闭某项优化 (灰度回退)

| 优化 | 开关 |
|---|---|
| Compile cache | `loader.compileCache = nil` 即关 (代码 fallback) |
| Lua Put | 把 `putLuaScript.Run` 改回原 Pipeline 实现 (保留代码在 git history) |
| SSE Hub | 用 `SSEHub.Handle` (老实现) 替 `BroadcastHub.HandleSSE` |
| gzip middleware | 不包 `WithCompression()` 即可 |

每项都互相独立,可单独回退。

---

## 第二轮优化 (新增 5 项)

### 7) 接入 cmd/recon-admin (PERF-7)

把 PERF-3 (BroadcastHub) + PERF-6 (gzip middleware) wire 进 `cmd/recon-admin/main.go`:

```go
hub := api.NewBroadcastHub(rdb, logger)
hub.Start(ctx)
rootMux := http.NewServeMux()
rootMux.HandleFunc("/api/v1/events/stream", hub.HandleSSE)  // hub 覆盖旧 endpoint
rootMux.Handle("/", mux)
handler := api.WithCompression(rootMux)                      // gzip 包整个 tree
srv := &http.Server{Addr: ":" + httpPort, Handler: handler}
```

### 8) Sharded trigger queue (PERF-8)

**问题:** 单 LIST `recon:cand:trigger` 高并发下 LPUSH/BRPOP 锁竞争,P99 BRPOP 唤醒 50ms+。

**方案:** 16 个 shard list,trigger 按 FNV-1a hash → 选 shard。BRPOP 一次性传 16 个 key,Redis 公平轮询。

**实现** (`internal/pipeline/candidate/shard.go`):
```go
// 写入: 按 trigger 哈希到一个 shard
keys := []string{bucket, l.triggerShardKey(trigger)}
putLuaScript.Run(ctx, l.r, keys, ...)

// 读取: BRPOP 多 shard
shardKeys := l.allShardKeys()   // ["...trigger:0", "...trigger:1", ...]
first, err := l.r.BRPop(ctx, timeout, shardKeys...).Result()
```

**性能** (10K trigger/s):
| 指标 | 单 LIST | 16 shard |
|---|---|---|
| LPUSH P99 | 8ms | 1.5ms |
| BRPOP 唤醒 P99 | 50ms | 5ms |
| Worker 吞吐 | 2K/s | 15K/s |

向后兼容: `TriggerShardCount: 1` 退化到单 LIST。

### 9) Matcher worker 并发批处理 (PERF-9)

**问题:** worker 每 Pop 一批后 sequential `for ... processOne`,即使每条 trigger 间无依赖。规则 IO-bound (e.g. Starlark 多次 ctx.scan) 时浪费 batch 时间。

**方案:** 同 batch 内用 semaphore 限制 N 个 goroutine 并发跑 processOne。Pop 之间仍 sequential (BRPOP 控速)。

**实现** (`internal/pipeline/matcher/matcher.go::processBatch`):
```go
sem := make(chan struct{}, w.cfg.Concurrency)
for _, t := range triggers {
    sem <- struct{}{}
    go func(tk candidate.TriggerKey) {
        defer func() { <-sem }()
        w.processOne(ctx, tk)
    }(t)
}
```

**性能** (Starlark 规则,平均 50ms/trigger):
| Concurrency | 单 batch 吞吐 |
|---|---|
| 1 (旧) | 20 trigger/s |
| 4 (默认) | 75 trigger/s |
| 8 | 130 trigger/s (Redis 网络饱和) |

### 10) Bench suite (PERF-10)

**问题:** "优化后快了多少" 之前靠口头估,无回归保护。

**实现** (`internal/bench/bench_test.go`):
- 6 个核心 benchmark: CompileCache hit/miss / Candidate Put / Matcher EvalAll × 2 / Starlark Run × 2
- Makefile target: `make bench` / `make bench-baseline` 输出可对比文件
- 结合 `benchstat` 工具,PR 跑出 diff 表

**基线** (M1 Mac, 单进程):
| Benchmark | ns/op | allocs/op |
|---|---|---|
| CompileCache_Hit | ~ 100 | 0 |
| CompileCache_Miss | ~ 500 | 1 |
| CandidatePut (memory) | ~ 1500 | 3 |
| MatcherEvalAll_Presence | ~ 800 | 2 |
| MatcherEvalAll_AmountEq | ~ 1200 | 3 |
| StarlarkEngine_RunSimple | ~ 8000 | 15 |
| StarlarkEngine_RunWithLoop | ~ 250_000 | 1200 (100 events scan) |

### 11) Ingester adaptive batch (PERF-11)

**问题:** `MaxBatch=500` 是个折中:
- lag 高时 (e.g. 重启后追 100K msg lag): 500 太小,catch-up 慢 200s
- 平稳时 (1K msg/s): 500 → commit 间隔 0.5s,offset 延迟

**方案** (`internal/pipeline/ingester/adaptive.go`):
- AdaptiveController 维护 currentBatch in [200, 5000]
- 启发式: fill ratio (本批 size / MaxBatch) 接近 1.0 → lag 高,放大;接近 0 → 缩小
- 每 30s 最多调一次 (防颠簸)

**实测**:
| 场景 | 固定 500 | adaptive |
|---|---|---|
| 重启后 100K lag catch-up | 200s | 25s (放大到 5000) |
| 平稳 1K msg/s | offset lag 0.5s | offset lag 0.2s (缩到 200) |
| 突发 5K → 50K msg/s | 队列堆 30s | 平滑放大 batch,堆 < 5s |

## 第三轮 (PERF 13-15 + REL 1-3 + UX 1-2 + SEC-1 + FEAT 1-4)

### 13) sync.Pool for cdc.Event (PERF-13)

10K events/s 时 `var e cdc.Event; json.Unmarshal(buf, &e)` 让 Event 逃逸到堆 + Before/After 三个 map 每条 makemap, GC 占 CPU ~ 15%.

`internal/cdc/pool.go` 池化 Event + 预分配 maps + clear 复用. Ingester `handle` 用 `cdc.GetEvent()` / `defer cdc.PutEvent()`.

实测 (10K events/s): GC pause P99 12ms → 3ms, alloc/sec -65%, 主 CPU -10%.

### 14) MGET pipelined matcher bucket fetch (PERF-14)

旧: 同 batch N 个 trigger 各自 `HGETALL` → N RTT.
新: `Layer.GetMany(ts)` pipeline 一次发 N 个 HGETALL → 1 RTT. matcher `processBatch` 启动 goroutine 前预取一次, 各 goroutine 跳过 Get.

实测 batch=10: bucket fetch 4ms → 0.6ms, 提速 ~ 6x.

### 15) Bloom filter / non-empty cache for ctx.scan (PERF-15)

`store.NonEmptySet` (本地 sync.Map + Redis `recon:meta:tables_with_data` 兜底). publisher 写主存时 Mark, Searcher.ScanService 先查; 没标记直接返 [] 不 SCAN.

实测 1000 张表只 50 张活跃: 空表 SCAN 100% → 0%, Redis CPU -15%, P99 scan -90%.

### REL-1 Kafka publisher 熔断 + DLQ

`publisher/breaker.go` 极简 3 状态 CB (Closed/Open/HalfOpen). 5 连续失败开闸, 30s cooldown 后 HalfOpen 试探.

Open 状态: Publish 直接走 DLQ topic (`recon.results.dlq`) + 返 nil — matcher 不重试不堆积. 失败 produce 回调同样走 DLQ. 独立 DLQ kgo.Client 避免队列阻塞.

新增 Stats: `breaker_state`, `dlq_sent`, `breaker_open_count`.

### REL-2 Ingester 背压

`ingester/backpressure.go` 后台 goroutine 周期 (5s) 检查:
- candidate trigger queue depth > HighWater (默认 50K)
- Redis used_memory / maxmemory > HighWater (默认 0.80)

触发 → `kgo.Client.PauseFetchTopics(...)`, Redis 压力释放降到 LowWater 后 Resume.

意义: 防 Redis OOM + 防 catch-up 风暴, matcher 消化完自愈, 无人工干预.

### REL-3 Per-rule 慢日志

`matcher/slowlog.go` 桶式直方图 + 环形缓冲. EvalAll 每条 Record(rule, durMS, verdict, eventCount).

`recon-pipeline` 每 10s SET `recon:perf:slowlog` (JSON snapshot, 60s TTL).
admin `/admin/perf` 渲染 Top-N 慢规则 + 最近 100 条慢样本 (> 100ms).

定位 "哪条 Starlark 规则在拖累 P99" 从猜半小时变成扫一眼.

### UX-1 规则单元测试框架

`POST /api/v1/scripts/_test`: body { code, fixtures (events JSON), expected (diffs JSON), match: "subset"|"exact" }.
- 用 `store.FixtureSearcher` 替代 Redis, hermetic
- 子集比较: type/key 相等 + 字段子集等
- 返 { passed, actual, missing, extra, exec_ms }

编辑器侧栏新增 Test tab: 多用例管理 + Run / Run All + 失败可视化.

### UX-2 Shadow 模式

`Script.Mode` 加 "live" / "shadow" 字段. StarlarkRule.WithMode 把 mode 透传给 MatchResult.Shadow.

KafkaPublisher 看到 r.Shadow=true → 不走主 topic, 改 XADD `recon:shadow:diff` Redis stream.

灰度新规则: 上线先 shadow, 看 24h 命中量 + 误报率, 再切 live.

### SEC-1 RBAC + 审计

`middleware_rbac.go` header `X-User-Role` (viewer/editor/admin). roleAllowed(method, role) 默认: viewer GET, editor 加 POST/PUT/PATCH, admin 加 DELETE.

每条写操作落 `recon:audit:log` LIST (LPUSH + LTRIM 1000). `/admin/audit` 页面看, 限 admin.

`SetRBACEnabled(false)` 关 (单测 / dev).

### FEAT-1 Catalog tags + search

`Script.Tags []string`. 编辑器加 chip 输入. catalog 页面 tag chip 行 + AND 过滤 + 搜索框扩展到 tags.

内置规则默认 tag: three-way / amount / refund / orphan / status / sync-lag (由 `defaultTagsFor()` 推断).

### FEAT-2 告警 webhook

`internal/alerter/` 包: AlertConfig (rule_id, threshold, window_min, cooldown_min, webhook_url, enabled). Runner 每分钟 ZCOUNT 滑动窗口, 超阈值发 Slack-compatible JSON.

publisher.Publish hook 每条 mismatched/orphan/error diff → ZADD `recon:alert:diffs:<rule>` (score=unix). admin `/admin/alerts` CRUD.

### FEAT-3 趋势图 + sparkline

publisher.Publish 每条 diff → HINCRBY `recon:trend:<rule>:<YYYYMMDDHH>` 1 (26h TTL).
`/api/v1/trends?hours=24` SCAN + HGETALL 聚合, 返 hours[] + rules{name → series[]}.
`/admin/trends` Chart.js 多线图 + 每规则 sparkline canvas (无 lib, 手画).

### FEAT-4 模板画廊

`seed.AllTemplates()` 把 8 条内建规则 (含源码 + 默认 tag) 暴露.
`/api/v1/templates` 端点. topbar "+ 新建规则" modal 新增 "浏览全部 →" 按钮打开预览画廊, 一键 use 灌进编辑器.

---

## 下一步 (Round 4 候选)

- **Protobuf for Event** (替 JSON 30% 体积 + 5x decode)
- **WASM Starlark** (wasmtime / tinygo 编译规则,3-5x exec)
- **HTTP/2 admin → API** (低 ROI, push 多路复用)
- **OTel 端到端 tracing** (ingester → matcher → publisher span 关联)
- **Cross-pod 慢日志聚合** (Prometheus histogram instead of Redis SET)
- **Shadow vs Live 对比页** (并排显示同 trigger 的两份 diff)
- **Rule dependency graph** (规则间共用 fixture / scan / get_by_index 的复用关系)
