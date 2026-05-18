// Command recon-pipeline — uber/fx 装配版, 跟 order-core / accounting-system / payment-core 同款风格.
//
// 把 5 段流水线串成一个进程 (或多个独立进程):
//
//	MySQL Binlog -> cdc -> [KafkaSink] -> Kafka -> [Ingester] -> Candidate -> [Matcher] -> [Publisher] -> Kafka result
//
// 角色 (通过 ROLE env 切换):
//
//	RECON_ROLE=cdc-bridge   只跑 CDC + KafkaSink (binlog → Kafka)
//	RECON_ROLE=ingester     只跑 Kafka → Candidate
//	RECON_ROLE=matcher      只跑 Candidate → MatchResult → Kafka result
//	RECON_ROLE=all          单进程 dev 模式 (3 个角色一起跑; 不推荐生产用)
//
// 必要 env:
//
//	RECON_REDIS_ADDR             候选层 Redis
//	RECON_KAFKA_BROKERS          逗号分隔 brokers
//	RECON_KAFKA_TOPICS           ingester 订阅的 topics (逗号分隔)
//	RECON_RESULT_TOPIC           publisher 输出 topic (默认 recon.results)
//	RECON_WORKER_COUNT           matcher worker 数, 默认 4
//	RECON_CDC_SOURCES_YAML       cdc 源配置 (默认 /app/configs/cdc.sources.yaml)
//	RECON_CDC_KAFKA_PREFIX       cdc → kafka topic 前缀 (默认 recon.cdc)
//	RECON_PIPELINE_PROBE_PORT    probe / healthz 端口 (默认 8090)
//
// 优雅停机: SIGTERM → fx.Stop → 各 lifecycle OnStop 依次执行
// (cdc.Manager.Stop / sink.Flush / publisher.Flush / matcher worker ctx.Done) → 退.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
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
	// fx 装配 — 共享 Provider (logger / redis / cfg / probe) + role-gated Invoke.
	opts := []fx.Option{
		fx.Provide(
			newLogger,
			newPipelineConfig,
			newRedis,
			newProbeServer,
			newCandidateLayer, // ingester + matcher 都用; pure matcher 模式没 ingesterOptions 也得有
		),
		fx.Invoke(startProbeServer),
		fx.WithLogger(func(log *zap.Logger) fxevent.Logger {
			return &fxevent.ZapLogger{Logger: log.Named("fx").With(zap.String("role", role))}
		}),
	}

	switch role {
	case "cdc-bridge":
		opts = append(opts, cdcBridgeOptions())
	case "ingester":
		opts = append(opts, ingesterOptions())
	case "matcher":
		opts = append(opts, matcherOptions())
	case "all":
		// "all" 推荐用于本地 dev, 生产分开部署 (cdc-bridge 单 pod, ingester / matcher 各自水平扩展).
		opts = append(opts, cdcBridgeOptions(), ingesterOptions(), matcherOptions())
	default:
		fmt.Fprintf(os.Stderr, "unknown RECON_ROLE: %s (expected cdc-bridge / ingester / matcher / all)\n", role)
		os.Exit(1)
	}

	fx.New(opts...).Run()
}

// ─── 共享 Providers ────────────────────────────────────────────────────────

// pipelineConfig 集中所有 env 配置, 后续接 config-center 时只替换这个 Provider.
type pipelineConfig struct {
	Role             string
	RedisAddr        string
	KafkaBrokers     []string
	KafkaTopics      []string
	ResultTopic      string
	CDCSourcesYAML   string
	CDCKafkaPrefix   string
	ProbePort        string
	MatcherWorkerN   int
}

func newPipelineConfig(log *zap.Logger) *pipelineConfig {
	workerN, _ := strconv.Atoi(envOr("RECON_WORKER_COUNT", "4"))
	if workerN < 1 {
		workerN = 1
	}
	cfg := &pipelineConfig{
		Role:           envOr("RECON_ROLE", "all"),
		RedisAddr:      envOr("RECON_REDIS_ADDR", "redis:6379"),
		KafkaBrokers:   csv(envOr("RECON_KAFKA_BROKERS", "kafka:9092")),
		KafkaTopics:    csv(envOr("RECON_KAFKA_TOPICS", "recon.cdc.order-core,recon.cdc.payment-channel,recon.cdc.accounting-system")),
		ResultTopic:    envOr("RECON_RESULT_TOPIC", "recon.results"),
		CDCSourcesYAML: envOr("RECON_CDC_SOURCES_YAML", "/app/configs/cdc.sources.yaml"),
		CDCKafkaPrefix: envOr("RECON_CDC_KAFKA_PREFIX", "recon.cdc"),
		ProbePort:      envOr("RECON_PIPELINE_PROBE_PORT", "8090"),
		MatcherWorkerN: workerN,
	}
	log.Info("recon-pipeline config loaded",
		zap.String("role", cfg.Role),
		zap.String("redis", cfg.RedisAddr),
		zap.Strings("kafka_brokers", cfg.KafkaBrokers),
		zap.Strings("kafka_topics", cfg.KafkaTopics),
		zap.String("result_topic", cfg.ResultTopic),
		zap.Int("matcher_worker_n", cfg.MatcherWorkerN))
	return cfg
}

// newLogger zap.NewProduction + lifecycle Sync; role label 在 fxevent logger 已带, 这里不重复 With.
func newLogger(lc fx.Lifecycle) (*zap.Logger, error) {
	logger, err := zap.NewProduction()
	if err != nil {
		return nil, fmt.Errorf("zap build: %w", err)
	}
	role := envOr("RECON_ROLE", "all")
	logger = logger.With(zap.String("role", role))
	lc.Append(fx.Hook{
		OnStop: func(_ context.Context) error {
			_ = logger.Sync()
			return nil
		},
	})
	return logger, nil
}

// newRedis 单实例 redis client; 共享给 cdc.Publisher / candidate.Layer / SlowLog.PublishPeriodic.
//
// lifecycle OnStop 关连接, 防进程退出时 keepalive socket 半开.
func newRedis(cfg *pipelineConfig, lc fx.Lifecycle, log *zap.Logger) *redis.Client {
	rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr})
	lc.Append(fx.Hook{
		OnStop: func(_ context.Context) error {
			if err := rdb.Close(); err != nil {
				log.Warn("redis close error", zap.Error(err))
				return err
			}
			return nil
		},
	})
	log.Info("redis client ready", zap.String("addr", cfg.RedisAddr))
	return rdb
}

// newProbeServer /healthz + /readyz HTTP server (轻量, 常驻).
func newProbeServer(cfg *pipelineConfig) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	return &http.Server{
		Addr:              ":" + cfg.ProbePort,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

// startProbeServer lifecycle OnStart 后台 ListenAndServe; OnStop graceful shutdown.
func startProbeServer(lc fx.Lifecycle, srv *http.Server, log *zap.Logger) {
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			log.Info("probe listening", zap.String("addr", srv.Addr))
			go func() {
				if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					log.Warn("probe server failed", zap.String("addr", srv.Addr), zap.Error(err))
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			shutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if err := srv.Shutdown(shutCtx); err != nil {
				log.Warn("probe shutdown error", zap.Error(err))
				return err
			}
			return nil
		},
	})
}

// ─── role: cdc-bridge ──────────────────────────────────────────────────────
//
// PIPE-CDC-BRIDGE: 真接 binlog reader + Kafka sink.
//
//	MySQL binlog → cdc.Canal → cdc.Publisher (写 Redis 主存 + 索引 + 位点 + recon:stream:events)
//	                              ↓ AfterPublishHook fanout
//	                          cdcbridge.KafkaSink → Kafka recon.cdc.<svc>
//
// 部署注意:
//   - 同一 MySQL DSN + server-id 只能有一个 binlog 消费者 — 跑 cdc-bridge 时
//     **必须** 关 recon-admin 那侧的 cdc.Manager (admin 默认起 cdc.Manager).
//   - 关法: 在 admin 启动 env 加 RECON_ADMIN_DISABLE_CDC=1.

func cdcBridgeOptions() fx.Option {
	return fx.Options(
		fx.Provide(
			newCDCConfig,
			newCDCTTLProvider,
			newCDCKafkaSink,
			newCDCPublisher,
			newCDCManager,
		),
		fx.Invoke(startCDCBridge),
	)
}

// newCDCConfig load cdc.sources.yaml; 找不到 / parse 错 fail-fast (因为生产数据流依赖它).
func newCDCConfig(cfg *pipelineConfig, log *zap.Logger) (*cdc.SourcesAndTTL, error) {
	cdcCfg, err := cdc.LoadFromFile(cfg.CDCSourcesYAML)
	if err != nil {
		log.Error("cdc-bridge load config failed",
			zap.String("path", cfg.CDCSourcesYAML), zap.Error(err))
		return nil, fmt.Errorf("cdc.LoadFromFile %s: %w", cfg.CDCSourcesYAML, err)
	}
	log.Info("cdc-bridge config loaded",
		zap.String("path", cfg.CDCSourcesYAML),
		zap.Int("sources", len(cdcCfg.Sources)),
		zap.Int("ttl_entries", len(cdcCfg.TTL)))
	return cdcCfg, nil
}

func newCDCTTLProvider(cdcCfg *cdc.SourcesAndTTL, log *zap.Logger) *cdc.TTLProvider {
	ttl := cdc.NewTTLProvider()
	if invalid := ttl.Reload(cdcCfg.TTL); len(invalid) > 0 {
		log.Warn("cdc-bridge ttl config has invalid entries",
			zap.Strings("invalid", invalid))
	}
	return ttl
}

func newCDCKafkaSink(cfg *pipelineConfig, lc fx.Lifecycle, log *zap.Logger) (*cdcbridge.KafkaSink, error) {
	kafkaCfg := cdcbridge.DefaultConfig(cfg.KafkaBrokers)
	kafkaCfg.TopicPrefix = cfg.CDCKafkaPrefix
	sink, err := cdcbridge.NewKafkaSink(kafkaCfg, log)
	if err != nil {
		log.Error("cdc-bridge kafka sink init failed",
			zap.Strings("brokers", cfg.KafkaBrokers),
			zap.String("topic_prefix", kafkaCfg.TopicPrefix),
			zap.Error(err))
		return nil, fmt.Errorf("cdcbridge.NewKafkaSink: %w", err)
	}
	lc.Append(fx.Hook{
		OnStop: func(ctx context.Context) error {
			// Flush 先 (把缓冲消息推完), 再 Close.
			if err := sink.Flush(ctx); err != nil {
				log.Warn("cdc-bridge kafka flush error", zap.Error(err))
			}
			sink.Close()
			log.Info("cdc-bridge kafka sink closed")
			return nil
		},
	})
	return sink, nil
}

func newCDCPublisher(rdb *redis.Client, ttl *cdc.TTLProvider, sink *cdcbridge.KafkaSink, log *zap.Logger) *cdc.Publisher {
	pub := cdc.NewPublisher(rdb, ttl, log)
	// AfterPublishHook fanout → Kafka. produce 异步, 失败已在 sink 内部回调 metric+log; 这里 fire-and-forget.
	pub.AfterPublishHook = func(ctx context.Context, e *cdc.Event) {
		_ = sink.Publish(ctx, e)
	}
	return pub
}

func newCDCManager(pub *cdc.Publisher, log *zap.Logger) *cdc.Manager {
	mgr := cdc.NewManager(pub, nil /* canal 自带 schema cache */, log)
	// 跟 admin 一致的内置 enricher / filter — 至少把 trace_id 提到 Indexes.
	mgr.AddGlobalEnricher(cdc.TraceIDEnricher{})
	mgr.AddGlobalFilter(cdc.SkipShadowRowsFilter{})
	return mgr
}

// startCDCBridge lifecycle: OnStart Reload sources → 启 binlog goroutines; OnStop mgr.Stop.
func startCDCBridge(lc fx.Lifecycle, mgr *cdc.Manager, cdcCfg *cdc.SourcesAndTTL, log *zap.Logger) {
	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			mgr.Reload(ctx, cdcCfg.Sources)
			log.Info("cdc-bridge manager started", zap.Int("sources", len(cdcCfg.Sources)))
			return nil
		},
		OnStop: func(_ context.Context) error {
			log.Info("cdc-bridge stopping...")
			mgr.Stop()
			cancel()
			log.Info("cdc-bridge stopped")
			return nil
		},
	})
}

// ─── role: ingester ────────────────────────────────────────────────────────

func ingesterOptions() fx.Option {
	return fx.Options(
		fx.Provide(newIngester),
		fx.Invoke(startIngester),
	)
}

// newCandidateLayer 候选层 — ingester / matcher 都共享同一实例 (无状态 wrapper, 复用 redis client).
// 注: candidate.NewRedisLayer 返 candidate.Layer 接口, 不是具体 *RedisLayer 类型.
func newCandidateLayer(rdb *redis.Client) candidate.Layer {
	return candidate.NewRedisLayer(rdb, candidate.DefaultConfig())
}

func newIngester(cfg *pipelineConfig, rdb *redis.Client, layer candidate.Layer, log *zap.Logger) (*ingester.Ingester, error) {
	ingCfg := ingester.DefaultConfig(cfg.KafkaBrokers, cfg.KafkaTopics)
	ing, err := ingester.New(ingCfg, layer, log)
	if err != nil {
		log.Error("ingester init failed",
			zap.Strings("brokers", cfg.KafkaBrokers),
			zap.Strings("topics", cfg.KafkaTopics),
			zap.Error(err))
		return nil, fmt.Errorf("ingester.New: %w", err)
	}
	// PERF-12: 双写到 Redis stream 供 admin web SSE 实时监控.
	ing.WithSSEStream(rdb, "recon:stream:events", 10000)
	return ing, nil
}

// startIngester lifecycle OnStart 后台跑 ing.Run, OnStop 通过 ctx cancel 让 Run 退.
func startIngester(lc fx.Lifecycle, ing *ingester.Ingester, log *zap.Logger) {
	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			go func() {
				log.Info("ingester started")
				if err := ing.Run(ctx); err != nil {
					log.Error("ingester run failed", zap.Error(err))
				}
				log.Info("ingester goroutine exited")
			}()
			return nil
		},
		OnStop: func(_ context.Context) error {
			cancel()
			return nil
		},
	})
}

// ─── role: matcher ─────────────────────────────────────────────────────────

func matcherOptions() fx.Option {
	return fx.Options(
		// candidate.Layer 由顶层 fx.Provide(newCandidateLayer) 提供 (跟 ingester 共享).
		fx.Provide(
			newMatcherPublisher,
			newDynamicRegistry,
			newSlowLog,
		),
		fx.Invoke(
			registerBuiltinRules,
			startSlowLogPublisher,
			startMatcherWorkers,
			startMatcherSweep,
		),
	)
}

func newMatcherPublisher(cfg *pipelineConfig, lc fx.Lifecycle, log *zap.Logger) (*publisher.KafkaPublisher, error) {
	pubCfg := publisher.DefaultConfig(cfg.KafkaBrokers)
	pubCfg.Topic = cfg.ResultTopic
	pub, err := publisher.NewKafkaPublisher(pubCfg, log)
	if err != nil {
		log.Error("matcher publisher init failed",
			zap.Strings("brokers", cfg.KafkaBrokers),
			zap.String("topic", pubCfg.Topic),
			zap.Error(err))
		return nil, fmt.Errorf("publisher.NewKafkaPublisher: %w", err)
	}
	lc.Append(fx.Hook{
		OnStop: func(ctx context.Context) error {
			if err := pub.Flush(ctx); err != nil {
				log.Warn("matcher publisher flush error", zap.Error(err))
			}
			pub.Close()
			log.Info("matcher publisher closed")
			return nil
		},
	})
	return pub, nil
}

// newDynamicRegistry DynamicRegistry 支持 Starlark 规则热编译 / 热替换 / 移除.
// admin web 拿 dynReg 引用即可在 PUT /scripts/:id 时调 Replace, 改规则不重启进程.
func newDynamicRegistry() *matcher.DynamicRegistry {
	return matcher.NewDynamicRegistry(nil)
}

// newSlowLog REL-3 慢日志记录器, 阈值 100ms / ring 100 — > 100ms 才记到环形缓冲.
func newSlowLog() *matcher.SlowLog {
	return matcher.NewSlowLog(100, 100)
}

// registerBuiltinRules 内置 Go 规则 (编译期注册, 运行时只读) + 接 SlowLog.
//
// 跟历史行为对齐:
//   - pi_three_way_presence: payment-core 三方齐
//   - pi_amount_equality: 金额三方相等
// 用户可以通过 admin API 动态注册更多规则 (script 包负责 Starlark 加载).
func registerBuiltinRules(dynReg *matcher.DynamicRegistry, slowLog *matcher.SlowLog, log *zap.Logger) {
	reg := dynReg.Registry
	reg.WithSlowLog(slowLog)

	reg.MustRegister(&matcher.CrossServicePresenceRule{
		RuleName: "pi_three_way_presence",
		BizKey:   "pi_id",
		ExpectedServices: []string{
			"order-core", "payment-channel", "accounting-system",
		},
	})
	reg.MustRegister(&matcher.AmountEqualityRule{
		RuleName: "pi_amount_equality",
		BizKey:   "pi_id",
		Pairs: []matcher.AmountPair{
			{Service: "order-core", Table: "payment_intent"},
			{Service: "payment-channel", Table: "acquirer_tx"},
			{Service: "accounting-system", Table: "ledger_entry"},
		},
	})
	log.Info("matcher builtin rules registered", zap.Int("count", 2))
}

// startSlowLogPublisher 周期把 SlowLog 推到 Redis (admin /admin/perf 读 recon:perf:slowlog).
func startSlowLogPublisher(lc fx.Lifecycle, slowLog *matcher.SlowLog, rdb *redis.Client, log *zap.Logger) {
	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			go slowLog.PublishPeriodic(ctx, rdb, 10*time.Second)
			log.Info("matcher slowlog publisher started", zap.Duration("interval", 10*time.Second))
			return nil
		},
		OnStop: func(_ context.Context) error {
			cancel()
			return nil
		},
	})
}

// startMatcherWorkers 起 cfg.MatcherWorkerN 个 worker, 每个用唯一 ID.
func startMatcherWorkers(lc fx.Lifecycle, cfg *pipelineConfig, layer candidate.Layer,
	dynReg *matcher.DynamicRegistry, pub *publisher.KafkaPublisher, log *zap.Logger) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	reg := dynReg.Registry
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			for i := 0; i < cfg.MatcherWorkerN; i++ {
				wg.Add(1)
				go func(id int) {
					defer wg.Done()
					w := matcher.NewWorker(
						matcher.DefaultWorkerConfig(fmt.Sprintf("matcher-%d", id)),
						layer, reg, pub, log)
					if err := w.Run(ctx); err != nil {
						log.Error("matcher worker run failed",
							zap.Int("id", id), zap.Error(err))
					}
				}(i)
			}
			log.Info("matcher workers started", zap.Int("count", cfg.MatcherWorkerN))
			return nil
		},
		OnStop: func(stopCtx context.Context) error {
			cancel()
			// 给 worker 一点时间 drain inflight job, 上限 30s (跟原 main 一致).
			drainCtx, drainCancel := context.WithTimeout(stopCtx, 30*time.Second)
			defer drainCancel()
			done := make(chan struct{})
			go func() { wg.Wait(); close(done) }()
			select {
			case <-done:
				log.Info("matcher workers drained")
			case <-drainCtx.Done():
				log.Warn("matcher workers drain timeout")
			}
			return nil
		},
	})
}

// startMatcherSweep 兜底超时触发 — 1min 一轮, 扫 12h 前未完成的桶.
func startMatcherSweep(lc fx.Lifecycle, layer candidate.Layer, log *zap.Logger) {
	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			go func() {
				ticker := time.NewTicker(1 * time.Minute)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						n, err := layer.Sweep(ctx, 12*time.Hour)
						if err != nil {
							log.Warn("sweep failed", zap.Error(err))
						} else if n > 0 {
							log.Info("sweep triggered", zap.Int("count", n))
						}
					}
				}
			}()
			log.Info("matcher sweep goroutine started")
			return nil
		},
		OnStop: func(_ context.Context) error {
			cancel()
			return nil
		},
	})
}

// ─── helpers ───────────────────────────────────────────────────────────────

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func csv(v string) []string {
	out := strings.Split(v, ",")
	for i := range out {
		out[i] = strings.TrimSpace(out[i])
	}
	return out
}
