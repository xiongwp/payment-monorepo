// reconplatform 入口：从 config-center 拉规则 + Redis / Kafka endpoint，
// Kafka 消费 order/payment 事件 → expr 引擎跑规则 → 输出对账结果。
//
// 历史：本服务原本是 hardcoded `localhost:6379` / `localhost:9092` + inline
// 规则的 demo 骨架。v2 升级：所有 endpoint 从环境变量读，规则从 config-center
// namespace=reconplatform 拉（key="rules"，JSON map[string]string）。
//
// **降级策略**：config-center 不可达时用环境变量 RECON_RULES_FALLBACK（JSON）
// 兜底；env=prod 强制 config-center 可达。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"go.uber.org/zap"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/xiongwp/payment-util/configcenter"

	"reconcile-system/internal/engine"
	"reconcile-system/internal/kafka"
	"reconcile-system/internal/model"
	"reconcile-system/internal/rule"
	"reconcile-system/internal/store"
)

func main() {
	logger, _ := zap.NewProduction()
	defer logger.Sync()

	redisAddr := envOr("RECON_REDIS_ADDR", "localhost:6379")
	kafkaBrokers := envCSV("RECON_KAFKA_BROKERS", []string{"localhost:9092"})
	consumeTopics := envCSV("RECON_KAFKA_CONSUME_TOPICS", []string{"order", "payment"})
	produceTopic := envOr("RECON_KAFKA_PRODUCE_TOPIC", "reconcile_result")
	consumerGroup := envOr("RECON_KAFKA_CONSUMER_GROUP", "reconplatform")

	st := store.New(redisAddr)
	ruleEngine := rule.New()

	// config-center 接入：拉 rules JSON 并应用。
	ccCli := newConfigCenterClient(logger)
	applyRules(ruleEngine, ccCli, logger)
	if ccCli != nil {
		// admin 改 rules 后秒级热更新
		ccCli.OnChange("rules", func(_ *configcenter.ConfigValue) {
			applyRules(ruleEngine, ccCli, logger)
		})
	}

	eng := engine.New(st, ruleEngine)

	producer := kafka.NewProducer(kafkaBrokers)
	consumer := kafka.NewConsumer(kafkaBrokers, consumeTopics...)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	go kafka.Consume(consumer, func(msg []byte) {
		var ev model.Event
		if err := json.Unmarshal(msg, &ev); err != nil {
			return
		}
		eng.Handle(ctx, ev)
	})

	// 暴露 Prometheus metrics endpoint 在 :8080/metrics
	metricsPort := envOr("RECON_METRICS_PORT", "8080")
	http.Handle("/metrics", promhttp.Handler())
	go func() {
		addr := fmt.Sprintf(":%s", metricsPort)
		log.Printf("recon: metrics listening on %s/metrics", addr)
		if err := http.ListenAndServe(addr, nil); err != nil {
			logger.Error("metrics server failed", zap.Error(err))
		}
	}()

	logger.Info("reconplatform started",
		zap.String("redis", redisAddr),
		zap.Strings("kafka_brokers", kafkaBrokers),
		zap.Strings("consume_topics", consumeTopics),
		zap.String("produce_topic", produceTopic),
		zap.String("group", consumerGroup),
		zap.String("metrics_port", metricsPort))

	for {
		select {
		case <-ctx.Done():
			logger.Info("shutting down")
			return
		case r, ok := <-eng.Output():
			if !ok {
				return
			}
			log.Println(r)
			producer.Send(produceTopic, r)
		}
	}
}

// newConfigCenterClient prod fail-fast；其它 env 不可达返 nil（本地用 fallback rules）。
func newConfigCenterClient(logger *zap.Logger) *configcenter.Client {
	endpoint := envOr("RECON_CONFIGCENTER_ENDPOINT", "http://config-center:9691")
	hostname, _ := os.Hostname()
	rpc := configcenter.NewHTTPClient(endpoint, nil)
	cli, err := configcenter.NewWithRPC(rpc, configcenter.Config{
		Namespace:  "reconplatform",
		InstanceID: hostname,
		Logger:     logger,
	})
	if err != nil {
		env := strings.ToLower(strings.TrimSpace(os.Getenv("RECON_ENV")))
		if env == "prod" || env == "production" {
			logger.Fatal("config-center unreachable in prod (fail-fast)", zap.Error(err))
		}
		logger.Warn("config-center unreachable; using RECON_RULES_FALLBACK env",
			zap.Error(err))
		return nil
	}
	return cli
}

// applyRules 从 cli 拉 "rules" key（JSON map[string]string）→ rule.Engine.Update
// 全量覆盖；不可达 fallback env RECON_RULES_FALLBACK；都没有就内置一组 demo 规则。
func applyRules(eng *rule.Engine, cli *configcenter.Client, logger *zap.Logger) {
	rules := map[string]string{}
	loaded := false
	if cli != nil {
		var rs map[string]string
		if err := configcenter.GetJSON(cli, context.Background(), "rules", &rs); err == nil {
			rules = rs
			loaded = true
		}
	}
	if !loaded {
		if raw := os.Getenv("RECON_RULES_FALLBACK"); raw != "" {
			_ = json.Unmarshal([]byte(raw), &rules)
			if len(rules) > 0 {
				loaded = true
			}
		}
	}
	if !loaded {
		// 兜底 demo 规则；prod 不应走到这里
		rules = map[string]string{
			"r1": "order.amount == payment.amount",
			"r2": "order.amount > 0",
		}
	}
	for id, expr := range rules {
		eng.Update(id, expr)
	}
	logger.Info("reconplatform rules applied", zap.Int("count", len(rules)))
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
