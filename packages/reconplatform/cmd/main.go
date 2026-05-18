// reconplatform 入口 — uber/fx 装配, 跟 order-core / accounting-system / payment-core 同款风格.
//
// 从 config-center 拉规则 + Redis / Kafka endpoint, Kafka 消费 order/payment 事件 → expr 引擎
// 跑规则 → 输出对账结果到 Kafka.
//
// 历史: 本服务原本是 hardcoded `localhost:6379` / `localhost:9092` + inline 规则的 demo 骨架.
// v2 升级所有 endpoint 从环境变量读, 规则从 config-center namespace=reconplatform 拉
// (key="rules", JSON map[string]string).
// v3 升级 (本次): main() 改 fx.New(...).Run(), 各组件走 fx.Provide / fx.Invoke,
// 跟 monorepo 内其它 fx 服务 (12 个) 保持一致.
//
// **降级策略**: config-center 不可达时用环境变量 RECON_RULES_FALLBACK (JSON) 兜底;
// env=prod 强制 config-center 可达 (newConfigCenterClient 内部 Fatal fail-fast).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
	"go.uber.org/zap"

	"github.com/xiongwp/payment-util/configcenter"

	"reconcile-system/internal/engine"
	"reconcile-system/internal/kafka"
	"reconcile-system/internal/model"
	"reconcile-system/internal/rule"
	"reconcile-system/internal/store"
)

func main() {
	fx.New(
		fx.Provide(
			newLogger,
			newReconConfig,
			newStore,
			newRuleEngine,
			newConfigCenterClient,
			newReconEngine,
			newKafkaProducer,
			newKafkaConsumer,
			newMetricsServer,
		),
		fx.Invoke(
			wireRulesHotReload,    // 启动期拉 rules + config-center OnChange 热更新
			startKafkaConsumer,    // 后台 goroutine 消费 + dispatch
			startReconcileLoop,    // 主循环: engine.Output → producer.Send
			startMetricsServer,    // /metrics HTTP server
		),
		fx.WithLogger(func(log *zap.Logger) fxevent.Logger {
			return &fxevent.ZapLogger{Logger: log.Named("fx")}
		}),
	).Run()
}

// ─── 配置 ─────────────────────────────────────────────────────────────────

// reconConfig 集中所有 env-driven 配置, 后续接 config-center 时只替换这个 Provider.
type reconConfig struct {
	RedisAddr         string
	KafkaBrokers      []string
	ConsumeTopics     []string
	ProduceTopic      string
	ConsumerGroup     string
	MetricsPort       string
	ConfigCenterAddr  string
	RulesFallbackJSON string
	Env               string
}

func newReconConfig(log *zap.Logger) *reconConfig {
	cfg := &reconConfig{
		RedisAddr:         envOr("RECON_REDIS_ADDR", "localhost:6379"),
		KafkaBrokers:      envCSV("RECON_KAFKA_BROKERS", []string{"localhost:9092"}),
		ConsumeTopics:     envCSV("RECON_KAFKA_CONSUME_TOPICS", []string{"order", "payment"}),
		ProduceTopic:      envOr("RECON_KAFKA_PRODUCE_TOPIC", "reconcile_result"),
		ConsumerGroup:     envOr("RECON_KAFKA_CONSUMER_GROUP", "reconplatform"),
		MetricsPort:       envOr("RECON_METRICS_PORT", "8080"),
		ConfigCenterAddr:  envOr("RECON_CONFIGCENTER_ENDPOINT", "http://config-center:9691"),
		RulesFallbackJSON: os.Getenv("RECON_RULES_FALLBACK"),
		Env:               strings.ToLower(strings.TrimSpace(os.Getenv("RECON_ENV"))),
	}
	log.Info("reconplatform config loaded",
		zap.String("redis", cfg.RedisAddr),
		zap.Strings("kafka_brokers", cfg.KafkaBrokers),
		zap.Strings("consume_topics", cfg.ConsumeTopics),
		zap.String("produce_topic", cfg.ProduceTopic),
		zap.String("group", cfg.ConsumerGroup),
		zap.String("metrics_port", cfg.MetricsPort),
		zap.String("env", cfg.Env))
	return cfg
}

// ─── infra Providers ───────────────────────────────────────────────────────

// newLogger zap.NewProduction; lifecycle OnStop Sync flushed buffered logs.
func newLogger(lc fx.Lifecycle) (*zap.Logger, error) {
	logger, err := zap.NewProduction()
	if err != nil {
		return nil, fmt.Errorf("zap build: %w", err)
	}
	lc.Append(fx.Hook{
		OnStop: func(_ context.Context) error {
			_ = logger.Sync()
			return nil
		},
	})
	return logger, nil
}

// newStore Redis store (内部走 go-redis), 单实例进程共享.
func newStore(cfg *reconConfig, log *zap.Logger) *store.Store {
	st := store.New(cfg.RedisAddr)
	log.Info("reconplatform store connected", zap.String("redis", cfg.RedisAddr))
	return st
}

// newRuleEngine expr 引擎; rules 后续由 wireRulesHotReload 通过 config-center 灌入.
func newRuleEngine() *rule.Engine {
	return rule.New()
}

// newConfigCenterClient prod fail-fast; 其它 env 不可达返 nil (本地用 fallback rules).
//
// 注: configcenter.Client 注入下游 wireRulesHotReload 后既要做 GetJSON 又要 OnChange,
// 所以返 nil 也合法 — wireRulesHotReload 检测 nil 走 fallback 路径.
func newConfigCenterClient(cfg *reconConfig, log *zap.Logger) *configcenter.Client {
	hostname, _ := os.Hostname()
	rpc := configcenter.NewHTTPClient(cfg.ConfigCenterAddr, nil)
	cli, err := configcenter.NewWithRPC(rpc, configcenter.Config{
		Namespace:  "reconplatform",
		InstanceID: hostname,
		Logger:     log,
	})
	if err != nil {
		if cfg.Env == "prod" || cfg.Env == "production" {
			log.Fatal("config-center unreachable in prod (fail-fast)",
				zap.String("endpoint", cfg.ConfigCenterAddr),
				zap.Error(err))
		}
		log.Warn("config-center unreachable; using RECON_RULES_FALLBACK env",
			zap.String("endpoint", cfg.ConfigCenterAddr),
			zap.Error(err))
		return nil
	}
	log.Info("config-center client ready",
		zap.String("endpoint", cfg.ConfigCenterAddr),
		zap.String("namespace", "reconplatform"))
	return cli
}

// newReconEngine 把 store + ruleEngine 组装成 engine.Engine.
func newReconEngine(st *store.Store, ruleEngine *rule.Engine) *engine.Engine {
	return engine.New(st, ruleEngine)
}

// newKafkaProducer 内部 sarama producer; lifecycle OnStop 关 producer 防消息丢失.
func newKafkaProducer(cfg *reconConfig, lc fx.Lifecycle, log *zap.Logger) *kafka.Producer {
	producer := kafka.NewProducer(cfg.KafkaBrokers)
	lc.Append(fx.Hook{
		OnStop: func(_ context.Context) error {
			// kafka.Producer 当前实现没暴露 Close (内部 stub); 留 hook 占位, 接真实 sarama 时填.
			log.Info("kafka producer hook OnStop (close TBD when real producer wired)")
			return nil
		},
	})
	return producer
}

// newKafkaConsumer 创建消费 group; goroutine 由 startKafkaConsumer 启动.
//
// kafka.NewConsumer 底层是 franz-go kgo.NewClient, 返 *kgo.Client. lifecycle OnStop Close
// 关连接, 防进程退出时 partition session 未释放.
func newKafkaConsumer(cfg *reconConfig, lc fx.Lifecycle, log *zap.Logger) *kgo.Client {
	consumer := kafka.NewConsumer(cfg.KafkaBrokers, cfg.ConsumeTopics...)
	log.Info("kafka consumer ready",
		zap.Strings("brokers", cfg.KafkaBrokers),
		zap.Strings("topics", cfg.ConsumeTopics))
	lc.Append(fx.Hook{
		OnStop: func(_ context.Context) error {
			consumer.Close()
			log.Info("kafka consumer closed")
			return nil
		},
	})
	return consumer
}

// newMetricsServer /metrics HTTP server (Prometheus scrape).
//
// 跟 payment-gateway / order-core 同款 — Server 仅作为对象 Provide, ListenAndServe 由
// startMetricsServer (fx.Invoke) 通过 lifecycle hook 管理.
func newMetricsServer(cfg *reconConfig) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	return &http.Server{
		Addr:              fmt.Sprintf(":%s", cfg.MetricsPort),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

// ─── fx.Invoke: 副作用 (rules 热更新 / kafka 消费 / 主循环 / metrics) ─────

// wireRulesHotReload 启动期拉一次 rules + 注册 OnChange 监听, 让 admin 改 rules 秒级生效.
//
// 跟历史行为对齐:
//   - config-center 可用: GetJSON("rules") + OnChange("rules") 注册回调
//   - 不可用: fallback env RECON_RULES_FALLBACK; 都没有走兜底 demo 规则
func wireRulesHotReload(cfg *reconConfig, cli *configcenter.Client, ruleEngine *rule.Engine, log *zap.Logger) {
	applyRules(cfg, ruleEngine, cli, log)
	if cli != nil {
		cli.OnChange("rules", func(_ *configcenter.ConfigValue) {
			applyRules(cfg, ruleEngine, cli, log)
		})
		log.Info("config-center OnChange listener registered for rules key")
	}
}

// startKafkaConsumer 后台 goroutine 跑 kafka.Consume, 把消息解码后投给 eng.Handle.
//
// 跟历史行为一致 — 解码失败的消息 silently drop (跟原 main.go 同款; 后续接 DLQ 时改).
// lifecycle ctx 来自 fx, fx 退出时 ctx 取消会让 kafka.Consume 内部退出.
func startKafkaConsumer(lc fx.Lifecycle, consumer *kgo.Client, eng *engine.Engine, log *zap.Logger) {
	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			go kafka.Consume(consumer, func(msg []byte) {
				var ev model.Event
				if err := json.Unmarshal(msg, &ev); err != nil {
					// 老行为: silently drop bad msg. 后续可加 DLQ + metric.
					return
				}
				eng.Handle(ctx, ev)
			})
			log.Info("kafka consumer goroutine started")
			return nil
		},
		OnStop: func(_ context.Context) error {
			cancel()
			log.Info("kafka consumer context cancelled")
			return nil
		},
	})
}

// startReconcileLoop 主循环 — engine 产出 reconcile result, 推 Kafka produce topic.
//
// 单 goroutine, lifecycle 退出时 cancel 让 select 退 ctx.Done() 分支.
func startReconcileLoop(lc fx.Lifecycle, cfg *reconConfig, eng *engine.Engine, producer *kafka.Producer, log *zap.Logger) {
	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			go func() {
				log.Info("reconcile output loop started",
					zap.String("produce_topic", cfg.ProduceTopic))
				for {
					select {
					case <-ctx.Done():
						log.Info("reconcile output loop stopped")
						return
					case r, ok := <-eng.Output():
						if !ok {
							log.Info("reconcile engine output channel closed")
							return
						}
						log.Debug("reconcile result", zap.Any("result", r))
						producer.Send(cfg.ProduceTopic, r)
					}
				}
			}()
			return nil
		},
		OnStop: func(_ context.Context) error {
			cancel()
			return nil
		},
	})
}

// startMetricsServer lifecycle: OnStart 后台 ListenAndServe; OnStop 5s graceful shutdown.
//
// 跟 payment-gateway / order-core 同款 (newHTTPServer + startHTTPServer 模式).
func startMetricsServer(lc fx.Lifecycle, srv *http.Server, log *zap.Logger) {
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			log.Info("reconplatform metrics listening", zap.String("addr", srv.Addr))
			go func() {
				if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					log.Error("metrics server failed",
						zap.String("addr", srv.Addr), zap.Error(err))
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			shutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if err := srv.Shutdown(shutCtx); err != nil {
				log.Warn("metrics server shutdown error", zap.Error(err))
				return err
			}
			return nil
		},
	})
}

// ─── helpers ───────────────────────────────────────────────────────────────

// applyRules 从 cli 拉 "rules" key (JSON map[string]string) → rule.Engine.Update 全量覆盖.
// 跟 v2 行为对齐 — 不可达 fallback env RECON_RULES_FALLBACK; 都没有就内置一组 demo 规则.
func applyRules(cfg *reconConfig, eng *rule.Engine, cli *configcenter.Client, log *zap.Logger) {
	rules := map[string]string{}
	loaded := false
	if cli != nil {
		var rs map[string]string
		if err := configcenter.GetJSON(cli, context.Background(), "rules", &rs); err != nil {
			log.Warn("config-center GetJSON(rules) failed; trying fallback",
				zap.Error(err))
		} else {
			rules = rs
			loaded = true
		}
	}
	if !loaded && cfg.RulesFallbackJSON != "" {
		if err := json.Unmarshal([]byte(cfg.RulesFallbackJSON), &rules); err != nil {
			log.Warn("RECON_RULES_FALLBACK parse failed", zap.Error(err))
		} else if len(rules) > 0 {
			loaded = true
			log.Info("rules loaded from RECON_RULES_FALLBACK env", zap.Int("count", len(rules)))
		}
	}
	if !loaded {
		// 兜底 demo 规则; prod 不应走到这里 (newConfigCenterClient 已 Fatal fail-fast).
		rules = map[string]string{
			"r1": "order.amount == payment.amount",
			"r2": "order.amount > 0",
		}
		log.Warn("rules: using built-in demo (config-center + fallback env both empty)",
			zap.Int("count", len(rules)))
	}
	for id, expr := range rules {
		eng.Update(id, expr)
	}
	log.Info("reconplatform rules applied", zap.Int("count", len(rules)))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envCSV(key string, def []string) []string {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}
