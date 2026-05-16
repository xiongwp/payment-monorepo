// Command recon-pipeline — 把 5 段流水线串成一个进程 (或多个独立进程).
//
//   MySQL Binlog -> cdc -> [KafkaSink] -> Kafka -> [Ingester] -> Candidate -> [Matcher] -> [Publisher] -> Kafka result
//
// 角色 (通过 ROLE env 切换):
//
//   ROLE=cdc-bridge   只跑 CDC + KafkaSink (binlog → Kafka)
//   ROLE=ingester     只跑 Kafka → Candidate
//   ROLE=matcher      只跑 Candidate → MatchResult → Kafka result
//   ROLE=all          单进程 dev 模式 (3 个角色一起跑;不推荐生产用)
//
// 必要 env:
//
//   RECON_REDIS_ADDR             候选层 Redis
//   RECON_KAFKA_BROKERS          逗号分隔 brokers
//   RECON_KAFKA_TOPICS           ingester 订阅的 topics (逗号分隔)
//   RECON_RESULT_TOPIC           publisher 输出 topic (默认 recon.results)
//   RECON_ROLE                   cdc-bridge / ingester / matcher / all
//   RECON_WORKER_COUNT           matcher worker 数,默认 4
//
// 优雅停机: SIGTERM -> 各 goroutine ctx 取消 -> Flush Kafka + AckMatch 当前桶 -> 退.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"reconcile-system/internal/cdc"
	"reconcile-system/internal/pipeline/candidate"
	"reconcile-system/internal/pipeline/cdcbridge"
	"reconcile-system/internal/pipeline/ingester"
	"reconcile-system/internal/pipeline/matcher"
	"reconcile-system/internal/pipeline/publisher"
)

func main() {
	role := envOr("RECON_ROLE", "all")
	logger, _ := zap.NewProduction()
	defer func() { _ = logger.Sync() }()
	logger = logger.With(zap.String("role", role))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Probe / metrics HTTP server (常驻)
	go runProbe(ctx, logger)

	var wg sync.WaitGroup

	switch role {
	case "cdc-bridge":
		wg.Add(1)
		go func() { defer wg.Done(); runCDCBridge(ctx, logger) }()
	case "ingester":
		wg.Add(1)
		go func() { defer wg.Done(); runIngester(ctx, logger) }()
	case "matcher":
		wg.Add(1)
		go func() { defer wg.Done(); runMatcher(ctx, logger) }()
	case "all":
		// "all" 推荐用于本地 dev,生产分开部署 (cdc-bridge 单 pod, ingester / matcher 各自水平扩展).
		wg.Add(3)
		go func() { defer wg.Done(); runCDCBridge(ctx, logger) }()
		go func() { defer wg.Done(); runIngester(ctx, logger) }()
		go func() { defer wg.Done(); runMatcher(ctx, logger) }()
	default:
		logger.Fatal("unknown ROLE", zap.String("role", role))
	}

	<-ctx.Done()
	logger.Info("shutdown signal received, draining...")
	// 给 30s 优雅退出
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer drainCancel()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		logger.Info("clean shutdown")
	case <-drainCtx.Done():
		logger.Warn("forced shutdown (drain timeout)")
	}
}

// ─── cdc-bridge ───────────────────────────────────────────────
//
// PIPE-CDC-BRIDGE: 真接 binlog reader + Kafka sink.
//
// 数据流:
//
//	MySQL binlog → cdc.Canal → cdc.Publisher (写 Redis 主存 + 索引 + 位点 + recon:stream:events)
//	                              ↓ AfterPublishHook fanout
//	                          cdcbridge.KafkaSink → Kafka recon.cdc.<svc>
//
// 配置:
//   RECON_CDC_SOURCES_YAML  YAML 文件路径 (默认 /app/configs/cdc.sources.yaml,
//                           与 recon-admin 同 env, 复用同一份 sources 配置)
//   RECON_REDIS_ADDR        Redis 地址 (主存 + 位点)
//   RECON_KAFKA_BROKERS     Kafka brokers
//   RECON_CDC_KAFKA_PREFIX  topic 前缀 (默认 recon.cdc)
//
// 部署注意:
//   - 同一 MySQL DSN + server-id 只能有一个 binlog 消费者 — 跑 cdc-bridge 时
//     **必须** 关 recon-admin 那侧的 cdc.Manager (admin 默认起 cdc.Manager).
//   - 关法: 在 admin 启动 env 加 RECON_ADMIN_DISABLE_CDC=1 (admin main.go 检查).
//   - 或者直接两个 cmd 用不同 server-id (Source.ServerID).
func runCDCBridge(ctx context.Context, logger *zap.Logger) {
	cfgPath := envOr("RECON_CDC_SOURCES_YAML", "/app/configs/cdc.sources.yaml")
	cdcCfg, err := cdc.LoadFromFile(cfgPath)
	if err != nil {
		logger.Fatal("cdc-bridge: load config",
			zap.String("path", cfgPath), zap.Error(err))
	}
	logger.Info("cdc-bridge: config loaded",
		zap.String("path", cfgPath),
		zap.Int("sources", len(cdcCfg.Sources)),
		zap.Int("ttl_entries", len(cdcCfg.TTL)))

	rdb := dialRedis()
	defer rdb.Close()

	// TTL provider — 默认 30d 兜底.
	ttl := cdc.NewTTLProvider()
	if invalid := ttl.Reload(cdcCfg.TTL); len(invalid) > 0 {
		logger.Warn("cdc-bridge: ttl config has invalid entries",
			zap.Strings("invalid", invalid))
	}

	pub := cdc.NewPublisher(rdb, ttl, logger)

	// 装 Kafka sink (PIPE-CDC-BRIDGE 核心).
	kafkaCfg := cdcbridge.DefaultConfig(brokers())
	kafkaCfg.TopicPrefix = envOr("RECON_CDC_KAFKA_PREFIX", kafkaCfg.TopicPrefix)
	sink, err := cdcbridge.NewKafkaSink(kafkaCfg, logger)
	if err != nil {
		logger.Fatal("cdc-bridge: kafka sink init", zap.Error(err))
	}
	defer sink.Close()

	pub.AfterPublishHook = func(ctx context.Context, e *cdc.Event) {
		// Kafka produce 异步, 失败已在 sink 内部回调 metric+log; 这里仅 fire-and-forget.
		_ = sink.Publish(ctx, e)
	}

	mgr := cdc.NewManager(pub, nil /* canal 自带 schema cache */, logger)
	// 跟 admin 一致的内置 enricher / filter — 至少把 trace_id 提到 Indexes.
	mgr.AddGlobalEnricher(cdc.TraceIDEnricher{})
	mgr.AddGlobalFilter(cdc.SkipShadowRowsFilter{})

	mgr.Reload(ctx, cdcCfg.Sources)
	logger.Info("cdc-bridge: manager started", zap.Int("sources", len(cdcCfg.Sources)))

	<-ctx.Done()
	logger.Info("cdc-bridge: stopping...")
	mgr.Stop()
	if err := sink.Flush(context.Background()); err != nil {
		logger.Warn("cdc-bridge: kafka flush", zap.Error(err))
	}
	logger.Info("cdc-bridge: stopped")
}

// ─── ingester ─────────────────────────────────────────────────

func runIngester(ctx context.Context, logger *zap.Logger) {
	rdb := dialRedis()
	defer rdb.Close()

	cfg := ingester.DefaultConfig(
		brokers(),
		topics(),
	)
	layer := candidate.NewRedisLayer(rdb, candidate.DefaultConfig())
	ing, err := ingester.New(cfg, layer, logger)
	if err != nil {
		logger.Fatal("ingester init", zap.Error(err))
	}
	// PERF-12: 双写到 Redis stream 供 admin web SSE 实时监控
	ing.WithSSEStream(rdb, "recon:stream:events", 10000)
	if err := ing.Run(ctx); err != nil {
		logger.Error("ingester run", zap.Error(err))
	}
}

// ─── matcher (workers + publisher) ────────────────────────────

func runMatcher(ctx context.Context, logger *zap.Logger) {
	rdb := dialRedis()
	defer rdb.Close()

	layer := candidate.NewRedisLayer(rdb, candidate.DefaultConfig())

	pubCfg := publisher.DefaultConfig(brokers())
	pubCfg.Topic = envOr("RECON_RESULT_TOPIC", pubCfg.Topic)
	pub, err := publisher.NewKafkaPublisher(pubCfg, logger)
	if err != nil {
		logger.Fatal("publisher init", zap.Error(err))
	}
	defer pub.Close()

	// DynamicRegistry: 支持 Starlark 规则热编译 / 热替换 / 移除.
	// admin web 拿 dynReg 引用即可在 PUT /scripts/:id 时调 Replace,
	// 改规则不重启进程,Worker.EvalAll 拿到读锁后用旧 rule 跑完就不受影响.
	dynReg := matcher.NewDynamicRegistry(nil)
	reg := dynReg.Registry

	// REL-3: 慢日志记录器 + 周期 publish 到 Redis,
	// admin /admin/perf 读 recon:perf:slowlog 拿快照渲染 top-N 慢规则.
	// 阈值 100ms / ring 100 — > 100ms 才记到环形缓冲.
	slowLog := matcher.NewSlowLog(100, 100)
	reg.WithSlowLog(slowLog)
	go slowLog.PublishPeriodic(ctx, rdb, 10*time.Second)

	// 内置 Go 规则 (编译期注册,运行时只读): payment-core 三方齐
	reg.MustRegister(&matcher.CrossServicePresenceRule{
		RuleName: "pi_three_way_presence",
		BizKey:   "pi_id",
		ExpectedServices: []string{
			"order-core", "payment-channel", "accounting-system",
		},
	})
	// 内置 Go 规则: 金额三方相等
	reg.MustRegister(&matcher.AmountEqualityRule{
		RuleName: "pi_amount_equality",
		BizKey:   "pi_id",
		Pairs: []matcher.AmountPair{
			{Service: "order-core", Table: "payment_intent"},
			{Service: "payment-channel", Table: "acquirer_tx"},
			{Service: "accounting-system", Table: "ledger_entry"},
		},
	})
	// 用户可以通过 admin API 动态注册更多规则 (script 包负责 Starlark 加载,
	// 编排进 matcher.Registry 由 cmd/recon-admin 完成).

	workerN, _ := strconv.Atoi(envOr("RECON_WORKER_COUNT", "4"))
	if workerN < 1 {
		workerN = 1
	}
	var wg sync.WaitGroup
	for i := 0; i < workerN; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			w := matcher.NewWorker(
				matcher.DefaultWorkerConfig(fmt.Sprintf("matcher-%d", id)),
				layer, reg, pub, logger)
			if err := w.Run(ctx); err != nil {
				logger.Error("matcher worker run", zap.Int("id", id), zap.Error(err))
			}
		}(i)
	}

	// Sweep goroutine: 兜底超时触发
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				n, err := layer.Sweep(ctx, 12*time.Hour)
				if err != nil {
					logger.Warn("sweep failed", zap.Error(err))
				} else if n > 0 {
					logger.Info("sweep triggered", zap.Int("count", n))
				}
			}
		}
	}()

	wg.Wait()
	_ = pub.Flush(context.Background())
}

// ─── helpers ──────────────────────────────────────────────────

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func brokers() []string {
	v := envOr("RECON_KAFKA_BROKERS", "kafka:9092")
	out := strings.Split(v, ",")
	for i := range out {
		out[i] = strings.TrimSpace(out[i])
	}
	return out
}

func topics() []string {
	v := envOr("RECON_KAFKA_TOPICS", "recon.cdc.order-core,recon.cdc.payment-channel,recon.cdc.accounting-system")
	out := strings.Split(v, ",")
	for i := range out {
		out[i] = strings.TrimSpace(out[i])
	}
	return out
}

func dialRedis() *redis.Client {
	addr := envOr("RECON_REDIS_ADDR", "redis:6379")
	return redis.NewClient(&redis.Options{Addr: addr})
}

// runProbe /healthz + /readyz + /metrics 入口 (轻量).
func runProbe(ctx context.Context, logger *zap.Logger) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })

	port := envOr("RECON_PIPELINE_PROBE_PORT", "8090")
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	logger.Info("probe listening", zap.String("port", port))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Warn("probe server", zap.Error(err))
	}
}
