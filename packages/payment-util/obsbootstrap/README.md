# obsbootstrap

跨服务一致的可观测性 bootstrap. 让任何 Go 服务一行起齐：

* `GET /healthz` — liveness (永远 200)
* `GET /readyz` — readiness (跑全部 ready_checks, JSON 输出)
* `GET /metrics` — Prometheus pull
* `GET /debug/pprof/*` — CPU / heap / goroutine / trace profile
* `GET/POST /admin/log-level` — 动态 zap level

加上：

* 标准 gRPC unary interceptor 链 (panic recover / access log / qps+latency metric)
* 轻量 circuit breaker (close → halfopen → open 三态)
* DB pool + outbox depth gauge 周期采集

设计原则：

* **零侵入**：业务代码不动，启动期 wire 一次。
* **复用 payment-util/healthx**：跟 ProbeFunc 兼容。
* **复用 payment-util/trace**：OTel propagation 已在 trace 包里。
* metric namespace 用 `service_name` 隔离，多服务汇 Prometheus 不冲突。

## 用法

```go
import "github.com/xiongwp/payment-util/obsbootstrap"

func main() {
    logLevel := zap.NewAtomicLevelAt(zap.InfoLevel)
    cfg := zap.NewProductionConfig()
    cfg.Level = logLevel
    log, _ := cfg.Build()

    // 1) Admin HTTP (健康 + metrics + pprof + log-level)
    admin := obsbootstrap.NewAdminServer(obsbootstrap.AdminConfig{
        ServiceName: "payment-core",
        Port:        envOr("ADMIN_HTTP_PORT", "9099"),
        Logger:      log,
        LogLevel:    logLevel,
    })
    admin.AddReadyCheck("mysql", func(c context.Context) error {
        return db.PingContext(c)
    })
    admin.AddReadyCheck("kafka", func(c context.Context) error {
        return kafkaPing(c)
    })
    go admin.Run(ctx)

    // 2) DB + outbox 周期 metric
    obsbootstrap.StartRuntimeCollectors(ctx, obsbootstrap.RuntimeOpts{
        ServiceName: "payment-core",
        DBs:         map[string]*sql.DB{"primary": db, "replica": dbRep},
        OutboxScrapers: map[string]obsbootstrap.OutboxScraper{
            "event_outbox": myEventOutboxScraper,
        },
        Logger: log,
    })

    // 3) gRPC interceptor 链
    srv := grpc.NewServer(grpc.UnaryInterceptor(obsbootstrap.Chain(
        obsbootstrap.PanicRecover(log),
        obsbootstrap.AccessLog(log),
        obsbootstrap.Metrics("payment_core"),
        myAuthInterceptor,
    )))

    // 4) Outbound circuit breaker
    cb := obsbootstrap.NewCircuit("accounting", obsbootstrap.CircuitConfig{
        FailureThreshold: 5,
        OpenDuration:     30 * time.Second,
    })
    if err := cb.Do(ctx, func() error { return accountingClient.Call() }); err != nil {
        // ErrCircuitOpen 或下游真错
    }
}
```

## metric 命名约定

* `<service>_grpc_server_handled_total` — gRPC 请求量
* `<service>_grpc_server_handled_duration_seconds` — gRPC 延迟分布
* `obs_circuit_state{downstream=...}` — 断路器状态
* `obs_db_open_connections{service, pool}` — DB pool
* `obs_outbox_depth{service, name}` — outbox 积压

## env 约定

* `ADMIN_HTTP_PORT` — admin HTTP 端口 (默认 9099)
* `OTEL_EXPORTER_OTLP_ENDPOINT` — 配了走 OTLP exporter (复用 payment-util/trace)
* `LOG_LEVEL` — 启动 zap level (debug/info/warn/error)

## 跟其他 payment-util 包的关系

* `payment-util/healthx` — Probe / Readiness handler (本包暴露的 readyz 用相同语义)
* `payment-util/trace` — OTel + zap trace_id (本包不重新实现 propagator)
* `payment-util/dbboot` — DB pool 初始化 (用了 dbboot 后 RuntimeCollectors 接 *sql.DB 即可)
