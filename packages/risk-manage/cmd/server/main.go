// Command server 启动 risk-manage gRPC 服务。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/redis/go-redis/v9"
	"github.com/segmentio/kafka-go"
	"github.com/spf13/viper"
	"github.com/xiongwp/payment-util/configcenter"
	"github.com/xiongwp/payment-util/trace"
	"go.uber.org/fx"
	"go.uber.org/zap"

	"time"

	"github.com/xiongwp/risk-manage/internal/audit"
	"github.com/xiongwp/risk-manage/internal/auth"
	"github.com/xiongwp/risk-manage/internal/cohort"
	"github.com/xiongwp/risk-manage/internal/dashboard"
	"github.com/xiongwp/risk-manage/internal/engine"
	"github.com/xiongwp/risk-manage/internal/eventbus"
	"github.com/xiongwp/risk-manage/internal/featurestore"
	"github.com/xiongwp/risk-manage/internal/features"
	"github.com/xiongwp/risk-manage/internal/feedback"
	"github.com/xiongwp/risk-manage/internal/ipintel"
	"github.com/xiongwp/risk-manage/internal/extsignal"
	"github.com/xiongwp/risk-manage/internal/merchantlist"
	"github.com/xiongwp/risk-manage/internal/metrics"
	"github.com/xiongwp/risk-manage/internal/mloverride"
	"github.com/xiongwp/risk-manage/internal/mlscore"
	"github.com/xiongwp/risk-manage/internal/reliability"
	"github.com/xiongwp/risk-manage/internal/review"
	"github.com/xiongwp/risk-manage/internal/ruleinsights"
	"github.com/xiongwp/risk-manage/internal/ruleio"
	"github.com/xiongwp/risk-manage/internal/ruleoverlap"
	"github.com/xiongwp/risk-manage/internal/rulesim"
	"github.com/xiongwp/risk-manage/internal/rules"
	"github.com/xiongwp/risk-manage/internal/sanction"
	"github.com/xiongwp/risk-manage/internal/server"
	"github.com/xiongwp/risk-manage/internal/service"
	risksession "github.com/xiongwp/risk-manage/internal/session"
	"github.com/xiongwp/risk-manage/internal/store"
	"github.com/xiongwp/risk-manage/internal/synthetic"
	"github.com/xiongwp/risk-manage/internal/webhook"
)

func main() {
	metrics.Register()

	// OTel 初始化：OTEL_EXPORTER_OTLP_ENDPOINT 空 → no-op；非空 → 走 OTLP gRPC
	// 推 span 到 collector。
	otelShutdown, otelErr := trace.InitOTel(context.Background(), "risk-manage", os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	if otelErr != nil {
		fmt.Fprintln(os.Stderr, "otel init:", otelErr)
	}
	defer func() {
		if otelShutdown != nil {
			_ = otelShutdown(context.Background())
		}
	}()

	app := fx.New(
		fx.Provide(
			loadConfig,
			newLogger,
			// config-center 客户端：rule thresholds / fail-policy / circuit
			// breaker 阈值等动态配置走这里。namespace="risk-manage"。
			// prod 不可达 fail-fast；dev 返 nil 兜底。
			configcenter.FxProvider("risk-manage"),
			newCounter,
			newBlacklist,
			newEventBus,
			newFeatureStore,
			newLinkStore,
			newIPIntel,
			newMLScore,
			newABTracker,
			newMLOverride,
			newExtSignalCache,
			newMLDrift,
			newExtractors,
			newSessionStore,
			newReviewStore,
			newFeedbackRecorder,
			newAPIKeyStore,
			newMerchantList,
			newIntervalTracker,
			newWebhookStore,
			newWebhookPublisher,
			newBreakers,
			newRateLimiter,
			newSanctionService,
			newEngine,
			newAuditSink,
			newRuleAuditStore,
			newRiskSvc,
			newServer,
		),
		fx.Invoke(startGRPC, startMetricsHTTP, startSynthetic, startWarmup, startCoverageWorker, startRuleInsightsWorker, startABTrackerWiring, startStorageMetricsWorker, startServiceRegistrar),
	)
	app.Run()
}

func loadConfig() (*viper.Viper, error) {
	v := viper.New()
	v.SetEnvPrefix("RISK")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	// AutomaticEnv 仅自动绑定已在 yaml 出现的 key；BindEnv 兜底保证
	// RISK_REGISTRY_ENDPOINTS 能被 GetStringSlice("registry.endpoints") 读到。
	_ = v.BindEnv("registry.endpoints")
	_ = v.BindEnv("env")
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath("./config")
	v.AddConfigPath(".")
	v.AddConfigPath("/etc/risk-manage")
	if err := v.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if !errors.As(err, &notFound) {
			return nil, err
		}
	}
	if err := assertProdSafety(v); err != nil {
		return nil, err
	}
	// rules-recipes.yaml 是预置规则配方包（注册反爆破 / 拆单 / 设备指纹 / DSL 等
	// 30+ 条）。MergeInConfig 会把它的 `rules:` 列表合并到 v 里；冲突时 viper 用
	// new 覆盖 base — 所以 config.yaml 里同 id 的规则会胜出（运维想关某条只
	// 需在 config.yaml 加同 id 的 enabled: false）。
	mv := viper.New()
	mv.SetConfigType("yaml")
	mv.SetConfigName("rules-recipes")
	mv.AddConfigPath("./config")
	mv.AddConfigPath(".")
	mv.AddConfigPath("/etc/risk-manage")
	if err := mv.ReadInConfig(); err == nil {
		// 拼接：v.rules + mv.rules → v.rules（保留 base，append 新条目；新文件
		// 中跟 base 同 id 的优先用 base，避免运维手改的 enabled 被 recipes 反盖）。
		baseRules := v.Get("rules")
		recipeRules := mv.Get("rules")
		merged := mergeRulesByID(baseRules, recipeRules)
		v.Set("rules", merged)
		// 把使用的 file path 暴露到 viper 让启动期日志可读
		v.Set("rules_recipes_file", mv.ConfigFileUsed())
	} else {
		var notFound viper.ConfigFileNotFoundError
		if !errors.As(err, &notFound) {
			return nil, err
		}
		// 不致命但通常不应发生：log 一下让运维一眼看到是 recipes 没找到导致
		// rule_count 偏低（默认值跟单独 config.yaml 难区分）。
		fmt.Fprintln(os.Stderr,
			"[WARN] rules-recipes.yaml not found in ./config / . / /etc/risk-manage; "+
				"only base rules from config.yaml will be loaded")
	}
	return v, nil
}

// assertProdSafety risk-manage prod 校验：
//   - auth.allow_unauthenticated 必须 false（决策服务无鉴权 = 任何人能拉黑）
//   - 不允许全部规则 enabled=false（等于风控空跑）
func assertProdSafety(v *viper.Viper) error {
	env := strings.ToLower(strings.TrimSpace(v.GetString("env")))
	if env != "prod" && env != "production" {
		return nil
	}
	if v.GetBool("auth.allow_unauthenticated") {
		return fmt.Errorf("PROD-SAFETY: risk-manage auth.allow_unauthenticated=true is forbidden in env=prod")
	}
	rules, _ := v.Get("rules").([]any)
	enabledCount := 0
	for _, r := range rules {
		if rm, ok := r.(map[string]any); ok {
			if e, ok := rm["enabled"].(bool); !ok || e {
				enabledCount++
			}
		}
	}
	if enabledCount == 0 {
		return fmt.Errorf("PROD-SAFETY: risk-manage 0 rules enabled in env=prod (risk control would be a no-op)")
	}
	return configcenter.AssertProdMandatory(v)
}

// mergeRulesByID 按 id 合并两组规则，base 同 id 优先；recipe 里没在 base 出现的
// 新规则 append 在尾部。两边都是 []map[string]any 形式（viper Get 解出的）。
func mergeRulesByID(base, recipe any) []any {
	baseSlice, _ := base.([]any)
	recipeSlice, _ := recipe.([]any)
	if base == nil {
		baseSlice = nil
	}
	if recipe == nil {
		recipeSlice = nil
	}
	seen := make(map[string]struct{}, len(baseSlice))
	for _, r := range baseSlice {
		if m, ok := r.(map[string]any); ok {
			if id, _ := m["id"].(string); id != "" {
				seen[id] = struct{}{}
			}
		}
	}
	out := append([]any{}, baseSlice...)
	for _, r := range recipeSlice {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		id, _ := m["id"].(string)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, r)
	}
	return out
}

func newLogger() (*zap.Logger, error) {
	if os.Getenv("RISK_LOG_DEV") == "true" {
		return zap.NewDevelopment()
	}
	return zap.NewProduction()
}

// newCounter Counter 配置：
//
//	counter.redis.addr: ""           # 配 host:port → 用 Redis 后端 (生产)
//	counter.redis.db:   0
//	counter.redis.password: ""
//	counter.redis.prefix: "risk:cnt:"
//	counter.cache_ttl: "1s"           # in-process LRU TTL；0 = 关缓存
//
// addr 为空 → 退到 MemCounter (dev / 单进程)。生产必须配 Redis 让多实例
// risk-manage 共享 counter (velocity / daily 等阈值跨 pod 一致)。
func newCounter(v *viper.Viper) store.Counter {
	ttl := v.GetDuration("counter.cache_ttl")
	if ttl == 0 {
		ttl = 1 * time.Second
	}
	var inner store.Counter
	if addr := v.GetString("counter.redis.addr"); addr != "" {
		rdb := redis.NewClient(&redis.Options{
			Addr:     addr,
			Password: v.GetString("counter.redis.password"),
			DB:       v.GetInt("counter.redis.db"),
		})
		prefix := v.GetString("counter.redis.prefix")
		if prefix == "" {
			prefix = "risk:cnt:"
		}
		inner = store.NewRedisCounter(rdb, prefix)
	} else {
		inner = store.NewMemCounter()
	}
	return store.NewCachedCounter(inner, ttl)
}

// newEventBus 默认 MemBus（同进程订阅；够 dev / 单实例部署）。
// 生产改 RedisBus（build tag redis 编进二进制后这里换成 eventbus.NewRedisBus）。
// eventbus.disabled=true 走 NoopBus（不发事件）。
func newEventBus(v *viper.Viper) eventbus.Bus {
	if v.GetBool("eventbus.disabled") {
		return eventbus.NoopBus{}
	}
	buf := v.GetInt("eventbus.buf_size")
	if buf <= 0 {
		buf = 1024
	}
	return eventbus.NewMemBus(buf)
}
func newBlacklist() store.Blacklist  { return store.NewMemBlacklist() }
func newLinkStore() store.LinkStore  { return store.NewMemLinkStore() }
// newIPIntel 默认 stub（空 mem service）。生产应注入 MaxMind / IPQS 客户端。
func newIPIntel() ipintel.Service { return ipintel.NewMemService() }

// newMLScore 默认走内置 LogisticService（先验权重）→ 客户开箱有 ML 分而非全 0。
// 生产建议：
//   1. 训练自己的 LogisticRegression / XGBoost 后用 NewLogisticServiceWith 替换权重
//   2. 或换成 RemoteService（gRPC client 调 TF Serving / Triton / 自建 XGBoost svc）
//
// 始终用 ChampionChallengerService 包一层（即使没 challenger）：
//   - 让 dashboard 能显示 champion_name
//   - 让 ABTracker 通过 SideEffect 收数据（升级 challenger 时直接用）
//
// 配置：
//
//	mlscore:
//	  enabled: true       # 默认 true；false 退回 NoopService
//	  model_ver: ""       # 空 = "logistic-v1.0-prior" 默认；自训后填新版本号
//	  champion_name: ""   # 缺省 "logistic-v1"
func newMLScore(v *viper.Viper) mlscore.Service {
	if v.IsSet("mlscore.enabled") && !v.GetBool("mlscore.enabled") {
		return mlscore.NoopService{}
	}
	champion := mlscore.NewLogisticService()
	champName := v.GetString("mlscore.champion_name")
	if champName == "" {
		champName = "logistic-v1"
	}
	return mlscore.NewChampionChallenger(champName, champion)
}

// newMLOverride provider：运营手动 ML 降级开关。零值 = 正常运行；
// admin 端点 /admin/mlscore/override 写入 disabled / force_score 后立即
// 影响下一次 Screen，无需重启。
func newMLOverride() *mloverride.Store { return mloverride.New() }

// newExtSignalCache provider：第三方反欺诈信号 (Sift/MaxMind/IPQS) 短期
// 缓存。TTL 走 viper key extsignal.cache_ttl_minutes，缺省 60 分钟。
func newExtSignalCache(v *viper.Viper) *extsignal.ScoreCache {
	mins := v.GetInt("extsignal.cache_ttl_minutes")
	if mins <= 0 {
		mins = 60
	}
	return extsignal.NewScoreCache(time.Duration(mins) * time.Minute)
}

// newABTracker provider：A/B test 用的三元组 ring buffer。cap 走 viper key
// mlscore.abtest.capacity，缺省 4096。
//
// 跟 ChampionChallengerService SideEffect 的接线在 startABTrackerWiring fx
// invoke 里做（依赖 mlscore.Service 已经构造好）。
func newABTracker(v *viper.Viper) *mlscore.ABTracker {
	cap := v.GetInt("mlscore.abtest.capacity")
	return mlscore.NewABTracker(cap)
}

// newExtractors 特征提取链。按顺序执行，每个 extractor fail-open。
// 顺序敏感：time → currency → card → customer_history（依赖前面字段的不能先跑）。
func newExtractors(links store.LinkStore, counter store.Counter) features.Chain {
	return features.Chain{
		features.NewTimeExtractor(),
		features.NewCurrencyExtractor(nil), // 用内置 default rates
		features.NewCardExtractor(nil),     // 用内置 default BIN map
		features.NewCustomerHistoryExtractor(links, counter),
	}
}

// newMLDrift 在线分数分布监控；admin 端点 /admin/mlscore/drift 看 snapshot + IsDrifted。
//
//	mlscore:
//	  drift:
//	    reservoir_size: 1000      # 滑窗采样数
//	    threshold_pct:  30        # 任一关键统计量 |Δ%| > 此值 → drifted
//
// 调用方式：周期任务 / Prometheus alerting 拉 /admin/mlscore/drift；
// 模型新版上线后 admin POST /admin/mlscore/drift/baseline 把当前快照设为新基线。
func newMLDrift(v *viper.Viper) *mlscore.DriftMonitor {
	size := v.GetInt("mlscore.drift.reservoir_size")
	thr := v.GetFloat64("mlscore.drift.threshold_pct")
	return mlscore.NewDriftMonitor(size, thr)
}

// newSessionStore 端 SDK fingerprint + behavior 的临时存储。生产 Redis 替换。
func newSessionStore() risksession.Store { return risksession.NewMemStore() }

// newReviewStore 人工 review 队列。生产换 PG / MySQL append-only 表。
func newReviewStore() review.Store { return review.NewMemStore() }

// newFeedbackRecorder Outcome 反馈记录器。生产换 PG append-only `risk_outcome` 表。
// maxAll=0 → 默认 100k 条上限。
func newFeedbackRecorder() feedback.Recorder { return feedback.NewMemRecorder(0) }

// newMerchantList 商户级 allow / block 名单。生产换 PG-backed Mem-cache + admin write。
func newMerchantList() merchantlist.Service { return merchantlist.NewMemService() }

// newIntervalTracker 给 register_interval / cross_city_login 等"距上次操作 < N 秒"
// 类规则用。生产换 Redis SET key value EX 24h。
func newIntervalTracker() store.IntervalTracker { return store.NewMemIntervalTracker() }

// newWebhookStore 商户 webhook 订阅。从 viper 读：
//
//	webhook:
//	  subscriptions:
//	    - merchant_id: m1
//	      url: https://merchant1.example/risk-webhook
//	      secret: <hmac-secret>
//	      events: [risk.review.created, risk.review.decided]   # 留空 = 全部
func newWebhookStore(v *viper.Viper) webhook.SubscriptionStore {
	store := webhook.NewMemSubscriptionStore()
	var defs []struct {
		MerchantID string   `mapstructure:"merchant_id"`
		URL        string   `mapstructure:"url"`
		Secret     string   `mapstructure:"secret"`
		Events     []string `mapstructure:"events"`
		Disabled   bool     `mapstructure:"disabled"`
	}
	if err := v.UnmarshalKey("webhook.subscriptions", &defs); err != nil {
		return store
	}
	for _, d := range defs {
		if d.MerchantID == "" || d.URL == "" {
			continue
		}
		evs := make([]webhook.EventType, 0, len(d.Events))
		for _, e := range d.Events {
			evs = append(evs, webhook.EventType(e))
		}
		store.Set(&webhook.Subscription{
			MerchantID: d.MerchantID, URL: d.URL, Secret: d.Secret,
			Events: evs, Disabled: d.Disabled,
		})
	}
	return store
}

// breakerPair 给 risk service 注入两个独立熔断器（ipintel / mlscore）。
type breakerPair struct {
	ip *reliability.Breaker
	ml *reliability.Breaker
}

// newBreakers 熔断参数；启动期 yaml bootstrap → SDK 覆盖 → 注册 OnChange 秒级热更。
//
// admin 改 namespace=risk-manage 下 key：
//   reliability.ipintel.fail_threshold / open_duration
//   reliability.mlscore.fail_threshold / open_duration
// 即时生效（Breaker.SetConfig 锁内整体替换）。
// newBreakers 100% 走 config-center；不可达 → hardcoded safe default。
//
// admin /admin/ns/risk-manage 改 reliability.{ipintel,mlscore}.{fail_threshold,
// open_duration} 即时生效（Breaker.SetConfig 锁内整体替换）。
func newBreakers(cli *configcenter.Client, logger *zap.Logger) breakerPair {
	ctx := context.Background()
	build := func(ccKey, name string, defThr int, defOpen time.Duration) *reliability.Breaker {
		thr := defThr
		open := defOpen
		if cli != nil {
			thr = cli.GetInt(ctx, ccKey+".fail_threshold", defThr)
			open = cli.GetDuration(ctx, ccKey+".open_duration", defOpen)
		}
		br := reliability.NewBreaker(reliability.Config{Name: name, FailThreshold: thr, OpenDuration: open})
		if cli != nil {
			apply := func(_ *configcenter.ConfigValue) {
				thr := cli.GetInt(ctx, ccKey+".fail_threshold", defThr)
				open := cli.GetDuration(ctx, ccKey+".open_duration", defOpen)
				br.SetConfig(reliability.Config{Name: name, FailThreshold: thr, OpenDuration: open})
				logger.Info("breaker hot-reloaded",
					zap.String("name", name),
					zap.Int("fail_threshold", thr),
					zap.Duration("open_duration", open))
			}
			cli.OnChange(ccKey+".fail_threshold", apply)
			cli.OnChange(ccKey+".open_duration", apply)
		}
		return br
	}
	return breakerPair{
		ip: build("reliability.ipintel", "ipintel", 5, 15*time.Second),
		ml: build("reliability.mlscore", "mlscore", 3, 30*time.Second),
	}
}

// newSanctionService AML / 制裁名单服务。
//
//	sanction:
//	  csv_path: /etc/risk-manage/sanction.csv      # 启动加载
//	  reload_interval: 1h                           # cron 频率
//
// 缺 csv_path → 返回空列表的 Mem 服务（rule 仍可注册，命中永远 false）。
// 生产对接：S3 watcher + 政府每日 OFAC SDN 文件 → 落本地 → Reload。
func newSanctionService(v *viper.Viper, lc fx.Lifecycle, logger *zap.Logger) sanction.Service {
	svc := sanction.NewMemService()
	_ = svc.Reload(context.Background(), nil)
	path := v.GetString("sanction.csv_path")
	if path == "" {
		logger.Info("sanction list not configured (sanction.csv_path empty); rule will never hit")
		return svc
	}
	load := func() {
		entries, err := sanction.LoadCSVFile(path)
		if err != nil {
			logger.Warn("sanction list load failed", zap.String("path", path), zap.Error(err))
			return
		}
		_ = svc.Reload(context.Background(), entries)
		n, _ := svc.Stats()
		logger.Info("sanction list loaded", zap.Int("entries", n), zap.String("path", path))
	}
	load()
	interval := v.GetDuration("sanction.reload_interval")
	if interval <= 0 {
		interval = time.Hour
	}
	stop := make(chan struct{})
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			go func() {
				t := time.NewTicker(interval)
				defer t.Stop()
				for {
					select {
					case <-t.C:
						load()
					case <-stop:
						return
					}
				}
			}()
			return nil
		},
		OnStop: func(_ context.Context) error { close(stop); return nil },
	})
	return svc
}

// newRateLimiter per-merchant Screen QPS 限流。配置：
//
//	rate_limit:
//	  default:
//	    rps:   500       # 默认套餐 sustained QPS
//	    burst: 1000      # 突发上限
//	  custom:
//	    big_merchant_id:  { rps: 5000, burst: 10000 }   # 大客户单独配
//
// rps<=0 → 限流 disabled，所有 Allow 直接 true。
func newRateLimiter(v *viper.Viper) *reliability.MerchantLimiter {
	def := reliability.LimitConfig{
		RPS:   v.GetFloat64("rate_limit.default.rps"),
		Burst: v.GetFloat64("rate_limit.default.burst"),
	}
	customRaw := v.GetStringMap("rate_limit.custom")
	custom := make(map[string]reliability.LimitConfig, len(customRaw))
	for mid, cfg := range customRaw {
		m, ok := cfg.(map[string]any)
		if !ok {
			continue
		}
		rps, _ := m["rps"].(float64)
		burst, _ := m["burst"].(float64)
		custom[mid] = reliability.LimitConfig{RPS: rps, Burst: burst}
	}
	return reliability.NewMerchantLimiter(def, custom)
}

// newWebhookPublisher 异步推送 worker pool。配置：
//
//	webhook:
//	  buffer: 1024     # 队列上限
//	  workers: 4       # 并发 worker
//	  max_retries: 5   # 失败指数退避重试上限
func newWebhookPublisher(store webhook.SubscriptionStore, v *viper.Viper, lc fx.Lifecycle, logger *zap.Logger) *webhook.Publisher {
	buf := v.GetInt("webhook.buffer")
	workers := v.GetInt("webhook.workers")
	retries := v.GetInt("webhook.max_retries")
	p := webhook.NewPublisher(store, logger, buf, workers, retries)
	// 默认开 DLQ：MemDLQStore（4096 cap）。生产应该换 PG-backed
	// store + 长留存 (schema 见 internal/webhook/dlq.go 注释)。
	dlqCap := v.GetInt("webhook.dlq_capacity")
	dlq := webhook.NewMemDLQStore(dlqCap)
	p.SetDLQ(dlq)
	logger.Info("webhook DLQ enabled", zap.Int("capacity", dlqCap))
	lc.Append(fx.Hook{
		OnStop: func(_ context.Context) error { p.Stop(); return nil },
	})
	return p
}

// newAPIKeyStore 商户级 API key store。配置：
//
//	auth:
//	  api_keys:
//	    - token: rsk_live_m1_<secret>
//	      scope: merchant            # merchant | admin | internal
//	      merchant_id: m1            # 仅 scope=merchant 必填
//	    - token: rsk_admin_<secret>
//	      scope: admin
//	    - token: rsk_internal_<secret>
//	      scope: internal
//
// 列表为空 → 返回 nil（gRPC 老 AuthTokens 模式继续兜底，行为不变）。
// 生产换 PG-backed loader（schema 类似 risk_api_key 表）。
func newAPIKeyStore(v *viper.Viper, logger *zap.Logger) auth.APIKeyStore {
	var defs []struct {
		Token      string `mapstructure:"token"`
		Scope      string `mapstructure:"scope"`
		MerchantID string `mapstructure:"merchant_id"`
	}
	if err := v.UnmarshalKey("auth.api_keys", &defs); err != nil || len(defs) == 0 {
		return nil
	}
	store := auth.NewMemAPIKeyStore()
	for _, d := range defs {
		if d.Token == "" {
			continue
		}
		var sc auth.Scope
		switch strings.ToLower(d.Scope) {
		case "merchant":
			sc = auth.ScopeMerchant
		case "admin":
			sc = auth.ScopeAdmin
		case "internal":
			sc = auth.ScopeInternal
		default:
			logger.Warn("api_key skipped: unknown scope", zap.String("scope", d.Scope))
			continue
		}
		if sc == auth.ScopeMerchant && d.MerchantID == "" {
			logger.Warn("api_key skipped: merchant scope requires merchant_id")
			continue
		}
		// 解析 token 前缀判定 test / live：rsk_test_... → IsTest=true，
		// 让 sandbox.Detect 接受 risk_test_* 触发器；rsk_live_... → 生产 key 不响应
		env, _ := auth.ParseKeyPrefix(d.Token)
		isTest := env == "test"
		store.Add(d.Token, auth.Principal{Scope: sc, MerchantID: d.MerchantID, IsTest: isTest})
	}
	logger.Info("api keys loaded", zap.Int("count", len(defs)))
	return store
}

func newEngine(v *viper.Viper, counter store.Counter, bl store.Blacklist, links store.LinkStore, ml merchantlist.Service, san sanction.Service, ivt store.IntervalTracker, logger *zap.Logger) (*engine.Engine, error) {
	eng := engine.New(logger)
	eng.RegisterFactory("amount_limit", rules.AmountLimitFactory(counter))
	eng.RegisterFactory("velocity", rules.VelocityFactory(counter))
	eng.RegisterFactory("blacklist", rules.BlacklistFactory(bl))
	eng.RegisterFactory("country_block", rules.CountryBlockFactory())
	eng.RegisterFactory("link_fanout", rules.LinkFanoutFactory(links))
	eng.RegisterFactory("link_fanout_multihop", rules.LinkFanoutMultihopFactory(links))
	eng.RegisterFactory("graph_reputation", rules.GraphReputationFactory(links))
	eng.RegisterFactory("cross_merchant_link", rules.CrossMerchantLinkFactory(links))
	eng.RegisterFactory("card_testing", rules.CardTestingFactory(links))
	eng.RegisterFactory("sanction_screening", rules.SanctionScreeningFactory(san))
	eng.RegisterFactory("dsl", rules.DSLFactory())
	eng.RegisterFactory("impossible_travel", rules.ImpossibleTravelFactory())
	eng.RegisterFactory("returning_customer", rules.ReturningCustomerFactory())
	eng.RegisterFactory("avs_check", rules.AVSCheckFactory())
	eng.RegisterFactory("bin_country", rules.BINCountryFactory())
	eng.RegisterFactory("email_validation", rules.EmailValidationFactory())
	eng.RegisterFactory("velocity_amount", rules.VelocityAmountFactory(counter))
	eng.RegisterFactory("rolling_amount", rules.RollingAmountFactory(counter))
	eng.RegisterFactory("account_age", rules.AccountAgeFactory())
	eng.RegisterFactory("disposable_email", rules.DisposableEmailFactory())
	eng.RegisterFactory("email_reputation", rules.EmailReputationFactory())
	eng.RegisterFactory("ip_risk", rules.IPRiskFactory())
	eng.RegisterFactory("behavior_anomaly", rules.BehaviorAnomalyFactory())
	eng.RegisterFactory("bot_detection", rules.BotDetectionFactory())
	eng.RegisterFactory("ml_threshold", rules.MLThresholdFactory())
	eng.RegisterFactory("merchant_allowlist", rules.MerchantAllowlistFactory(ml))
	eng.RegisterFactory("merchant_blocklist", rules.MerchantBlocklistFactory(ml))
	eng.RegisterFactory("new_account_high_value", rules.NewAccountHighValueFactory())
	eng.RegisterFactory("login_anomaly", rules.LoginAnomalyFactory())
	eng.RegisterFactory("email_pattern", rules.EmailPatternFactory())
	eng.RegisterFactory("register_velocity", rules.RegisterVelocityFactory(links))
	eng.RegisterFactory("client_tampering", rules.ClientTamperingFactory())
	eng.RegisterFactory("register_interval", rules.RegisterIntervalFactory(ivt))
	eng.RegisterFactory("username_pattern", rules.UsernamePatternFactory())
	eng.RegisterFactory("fingerprint_multi_account", rules.FingerprintMultiAccountFactory(links))
	eng.RegisterFactory("ua_batch_register", rules.UABatchRegisterFactory(links))

	var defs []struct {
		ID       string                `mapstructure:"id"`
		Name     string                `mapstructure:"name"`
		Type     string                `mapstructure:"type"`
		Decision string                `mapstructure:"decision"`
		Enabled  bool                  `mapstructure:"enabled"`
		Mode     string                `mapstructure:"mode"`
		Weight   int                   `mapstructure:"weight"`
		Config   string                `mapstructure:"config"`
		Rollout  engine.RolloutConfig  `mapstructure:"rollout"`
	}
	if err := v.UnmarshalKey("rules", &defs); err != nil {
		return nil, err
	}
	engineDefs := make([]engine.RuleDef, len(defs))
	for i, d := range defs {
		engineDefs[i] = engine.RuleDef{
			ID: d.ID, Name: d.Name, Type: d.Type, Decision: d.Decision,
			Enabled: d.Enabled, Mode: d.Mode, Weight: d.Weight,
			ConfigJSON: json.RawMessage(d.Config),
			Rollout:    d.Rollout,
		}
	}
	if err := eng.LoadRules(engineDefs); err != nil {
		return nil, err
	}
	return eng, nil
}

// newAuditSink 构造决策审计 sink。
//
//	LogSink            打 zap audit_event=risk_decision，让 log shipping 路由到独立审计 sink
//	MemSink(cap=4096)  in-process ring buffer，给 admin /admin/audit 端点查最近 N 条
//
// 生产应再加一个 KafkaSink 把每条决策落到长留存的 audit topic；当前 stub
// 只用 Log + Mem 两个内置 sink 起步，接入 Kafka 时直接 append 到 MultiSink。
// newRuleAuditStore 规则变更审计日志（谁改了哪条规则）。Mem 实现进程内，
// 重启清；生产应该换 PG-backed 实现（schema 见 audit/rule_audit.go 注释）。
// rule_audit.disabled=true → nil，所有写都 no-op。
func newRuleAuditStore(v *viper.Viper) audit.RuleAuditStore {
	if v.GetBool("rule_audit.disabled") {
		return nil
	}
	cap := v.GetInt("rule_audit.mem_capacity")
	return audit.NewMemRuleAuditStore(cap)
}

func newAuditSink(lc fx.Lifecycle, v *viper.Viper, logger *zap.Logger) audit.Sink {
	cap := v.GetInt("audit.mem_capacity")
	if cap <= 0 {
		cap = 4096
	}
	base := audit.MultiSink{
		&audit.LogSink{Logger: logger},
		audit.NewMemSink(cap),
	}

	// audit.file.path 配了 → 加一个文件 sink（重启不丢，比 LogSink 更结构化）
	// 用 AsyncBatchSink 包：fsync + 滚动开销不阻塞 Screen 主路径。
	if path := v.GetString("audit.file.path"); path != "" {
		maxSize := int64(v.GetInt("audit.file.max_size_mb")) << 20
		if maxSize == 0 {
			maxSize = 128 << 20
		}
		maxBackups := v.GetInt("audit.file.max_backups")
		if maxBackups == 0 {
			maxBackups = 10
		}
		fsyncEvery := v.GetInt("audit.file.fsync_every")
		fileSink, err := audit.NewFileSink(path, maxSize, maxBackups, fsyncEvery, logger)
		if err != nil {
			logger.Warn("audit file sink init failed; skipping", zap.Error(err))
		} else {
			async := audit.NewAsyncBatchSink(fileSink, 8192, 1000, time.Second, logger)
			base = append(base, async)
			logger.Info("audit file sink enabled",
				zap.String("path", path), zap.Int64("max_size_bytes", maxSize))
			lc.Append(fx.Hook{
				OnStop: func(_ context.Context) error {
					async.Stop()
					return fileSink.Close()
				},
			})
		}
	}

	// audit.kafka.brokers 配了 → 加 Kafka audit sink。Flink / ClickHouse Kafka
	// engine 都消费这条 topic。包 AsyncBatchSink 让 Kafka produce 不阻塞主路径。
	if brokers := v.GetString("audit.kafka.brokers"); brokers != "" {
		topic := v.GetString("audit.kafka.topic")
		if topic == "" {
			topic = "risk.decision.v1"
		}
		kw := &kafka.Writer{
			Addr:         kafka.TCP(splitCommaList(brokers)...),
			Topic:        topic,
			Balancer:     &kafka.Hash{},
			BatchSize:    1000,
			BatchTimeout: 100 * time.Millisecond,
			RequiredAcks: kafka.RequireOne,
		}
		ks := audit.NewKafkaSink(kw, logger)
		async := audit.NewAsyncBatchSink(ks, 8192, 1000, time.Second, logger)
		base = append(base, async)
		logger.Info("audit kafka sink enabled",
			zap.String("brokers", brokers), zap.String("topic", topic))
		lc.Append(fx.Hook{
			OnStop: func(_ context.Context) error {
				async.Stop()
				return ks.Close()
			},
		})
	}

	// audit.clickhouse.dsn 配了 → 加 ClickHouse audit sink (本地长留存查询)。
	// 跟 Kafka 双写：Kafka 给实时 Flink + ClickHouse Kafka engine table；CH
	// 直连 sink 给"立刻可查"场景。生产建议只走 Kafka → CH Kafka engine 单
	// 路径以减少双写不一致风险；但 dev / POC 直连 CH 简单。
	if dsn := v.GetString("audit.clickhouse.dsn"); dsn != "" {
		conn, err := clickhouse.Open(&clickhouse.Options{
			Addr: splitCommaList(dsn),
			Auth: clickhouse.Auth{
				Database: v.GetString("audit.clickhouse.database"),
				Username: v.GetString("audit.clickhouse.username"),
				Password: v.GetString("audit.clickhouse.password"),
			},
		})
		if err != nil {
			logger.Warn("clickhouse audit sink init failed; skipping", zap.Error(err))
		} else {
			chs := audit.NewClickHouseSink(conn, logger)
			base = append(base, chs)
			logger.Info("audit clickhouse sink enabled", zap.String("dsn", dsn))
		}
	}

	// audit.chain_signing: 启用 → 在所有写入前添加链式签名（chain_prev_hash /
	// chain_row_hash 写到 metadata）。合规取证场景必须开。
	//
	// 重启续链：如果配了 audit.file.path 且文件已存在，读最后一条 audit
	// 的 chain_row_hash 作为新 chain 的 prev_hash，保证 audit 链不断。
	// 没文件 / 读失败 → fallback genesis（首次启动）。
	if v.GetBool("audit.chain_signing") {
		logger.Info("audit chain signing ENABLED (tamper-evident)")
		if path := v.GetString("audit.file.path"); path != "" {
			if last, err := audit.ReadLastChainRowHash(path); err == nil && last != "" {
				logger.Info("audit chain resumed from FileSink",
					zap.String("path", path),
					zap.String("last_row_hash", last[:16]+"..."))
				return audit.NewChainSinkResume(base, last)
			}
		}
		return audit.NewChainSink(base)
	}
	return base
}

func newRiskSvc(
	eng *engine.Engine,
	counter store.Counter,
	links store.LinkStore,
	ipIntel ipintel.Service,
	mlSvc mlscore.Service,
	mlDrift *mlscore.DriftMonitor,
	mlOverride *mloverride.Store,
	extractors features.Chain,
	sessions risksession.Store,
	reviewQ review.Store,
	fbRec feedback.Recorder,
	wh *webhook.Publisher,
	bp breakerPair,
	sink audit.Sink,
	bus eventbus.Bus,
	fs featurestore.Store,
	logger *zap.Logger,
) *service.RiskService {
	svc := service.NewWithAudit(eng, counter, links, ipIntel, mlSvc, sessions, reviewQ, fbRec, sink, logger)
	svc.SetMLOverride(mlOverride)
	svc.SetWebhookPublisher(wh)
	svc.SetBreakers(bp.ip, bp.ml)
	svc.SetMLDrift(mlDrift)
	svc.SetExtractors(extractors)
	svc.SetEventBus(bus)
	svc.SetFeatureStore(fs)
	// slow log 阈值：viper 没配 → 100ms 缺省；0 / 负值 → 关闭。
	if v, has := readSlowThresholdSec(svc); has {
		svc.SetSlowThreshold(v)
	}
	return svc
}

// readSlowThresholdSec 从 viper key risk.slow_threshold_ms 取阈值，转秒返。
// 缺省 100ms（够覆盖 IPIntel 网络往返 + ML 推理）。
//
// 调用 viper 需要这层间接：newRiskSvc 已经签名固定，不方便加 viper 参数；
// 改为给 service 加一个独立 setter 在 invoke 阶段调更省事。
func readSlowThresholdSec(_ *service.RiskService) (float64, bool) {
	// 用 env 兜底（viper 没 fx wire 进来；保持向后兼容）
	if v := os.Getenv("RISK_SLOW_THRESHOLD_MS"); v != "" {
		if n, err := strconvAtoi(v); err == nil && n > 0 {
			return float64(n) / 1000.0, true
		}
	}
	return 0.1, true // 100ms 缺省
}

// newFeatureStore 默认 EventbusStore（落到 ml.feature_snapshot topic，离线 worker
// 消费）。featurestore.disabled=true 走 NoopStore（关功能）；
// featurestore.mem=true 走 Mem（dev 友好，可在 Get 反查单条）。
func newFeatureStore(v *viper.Viper, bus eventbus.Bus) featurestore.Store {
	if v.GetBool("featurestore.disabled") {
		return featurestore.NoopStore{}
	}
	if v.GetBool("featurestore.mem") {
		return featurestore.NewMemStore(v.GetInt("featurestore.mem_max"))
	}
	return featurestore.NewEventbusStore(bus)
}

func newServer(svc *service.RiskService, bl store.Blacklist, apiKeys auth.APIKeyStore, lim *reliability.MerchantLimiter, v *viper.Viper, logger *zap.Logger) *server.Server {
	tokens := map[string]string{}
	for _, t := range v.GetStringSlice("auth.tokens") {
		tokens[t] = "ok"
	}
	srv := server.NewServer(server.Deps{
		RiskSvc:    svc,
		Blacklist:  bl,
		AuthTokens: tokens,
		APIKeys:    apiKeys,
		Limiter:    lim,
		Logger:     logger,
	})
	// graceful shutdown 等 in-flight RPC 跑完；超时强制 Stop 防部署窗口被
	// 卡死。viper key server.shutdown_timeout_sec 缺省 15s。
	if sec := v.GetInt("server.shutdown_timeout_sec"); sec > 0 {
		srv.SetShutdownTimeout(time.Duration(sec) * time.Second)
	}
	return srv
}

func startGRPC(lc fx.Lifecycle, s *server.Server, v *viper.Viper, logger *zap.Logger) {
	port := v.GetInt("server.grpc_port")
	if port == 0 {
		port = 9490
	}
	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			go func() {
				if err := s.ListenAndServe(ctx, port); err != nil {
					logger.Error("grpc exited", zap.Error(err))
				}
			}()
			return nil
		},
		OnStop: func(_ context.Context) error { cancel(); return nil },
	})
}

// startWarmup Redis / LinkStore 重启后的 graph warm-start。
//
// 触发时机：服务启动 5s 后（让 audit MemSink 准备好），从 audit ring buffer
// 拉最近 N 小时的决策行，逐条 Link() 进 LinkStore。让重启后那 5min 黄金窗口
// 不再无防御。
//
// 配置：
//   warmup.disabled=true → 关
//   warmup.lookback_hours → 默认 1h（LinkStore TTL = 1h，再多没意义）
//
// 失败 fail-open：如果 sink 不是 MemSink（例如 chain wrapped）拿不到行就静默
// 跳过；生产 ClickHouse sink 应该实现 Recent() 拼到这个流程里。
func startWarmup(lc fx.Lifecycle, v *viper.Viper, links store.LinkStore, sink audit.Sink, logger *zap.Logger) {
	if v.GetBool("warmup.disabled") {
		return
	}
	hours := v.GetInt("warmup.lookback_hours")
	if hours <= 0 {
		hours = 1
	}
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			go func() {
				time.Sleep(5 * time.Second)
				mem := findMemSink(sink)
				if mem == nil {
					logger.Info("warmup skipped: audit sink doesn't expose Recent()")
					return
				}
				cutoff := time.Now().Add(-time.Duration(hours) * time.Hour)
				audits := mem.Recent(50000)
				rows := make([]store.AuditRow, 0, len(audits))
				for _, a := range audits {
					if a.OccurredAt.Before(cutoff) {
						continue
					}
					rows = append(rows, store.AuditRow{
						OccurredAt: a.OccurredAt,
						MerchantID: a.Input.MerchantID,
						CustomerID: a.Input.CustomerID,
						IPAddress:  a.Input.IPAddress,
						DeviceID:   a.Input.DeviceID,
						Metadata:   a.Input.Metadata,
					})
				}
				edges := store.Warmup(context.Background(), links, rows)
				logger.Info("linkstore warmup complete",
					zap.Int("audit_rows", len(rows)),
					zap.Int("edges_written", edges),
					zap.Int("lookback_hours", hours))
			}()
			return nil
		},
	})
}

// startSynthetic 起 1 分钟周期的 synthetic probe worker，给 silent regression
// 类故障兜底（例如 yaml 漏拷贝 / dsl metadata 字段拼错 / 阈值配 0）。
// synthetic.disabled=true 关；synthetic.interval 自定义间隔（默认 60s）。
func startSynthetic(lc fx.Lifecycle, v *viper.Viper, svc *service.RiskService, logger *zap.Logger) {
	if v.GetBool("synthetic.disabled") {
		return
	}
	interval := v.GetDuration("synthetic.interval")
	w := synthetic.NewWorker(svc, synthetic.DefaultProbes(), interval, logger)
	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			go w.Start(ctx)
			logger.Info("synthetic monitoring started", zap.Duration("interval", interval))
			return nil
		},
		OnStop: func(_ context.Context) error {
			cancel()
			return nil
		},
	})
}

func startMetricsHTTP(
	lc fx.Lifecycle,
	v *viper.Viper,
	eng *engine.Engine,
	sink audit.Sink,
	sessions risksession.Store,
	reviewQ review.Store,
	fbRec feedback.Recorder,
	mlList merchantlist.Service,
	mlSvc mlscore.Service,
	wh *webhook.Publisher,
	mlDrift *mlscore.DriftMonitor,
	abTracker *mlscore.ABTracker,
	mlOverride *mloverride.Store,
	extSignal *extsignal.ScoreCache,
	fs featurestore.Store,
	ruleAudit audit.RuleAuditStore,
	logger *zap.Logger,
) {
	addr := v.GetString("metrics.addr")
	if addr == "" {
		addr = ":9590"
	}
	probe := func() int { return eng.RuleCount() }
	// auditQ 永远非 nil：admin-web 的 /risk + /risk/decisions 页面会无条件请求
	// /admin/audit/decisions；以前只在 sink 是 MemSink 时才注册此 endpoint，
	// 生产用 EventbusStore / dual-sink 部署里 findMemSink 返 nil → 端点 404 →
	// 这两个页面 Promise.all 第一个失败就整页报红。改成始终注册：MemSink 在
	// 时返 Recent(limit)，否则返空切片让 UI 优雅降级（提示运营接长期审计平台
	// 比如 ClickHouse）。注：production 想看 ring 数据应在 sink 链里加 MemSink
	// 作为 secondary，参见 audit/dual_sink.go。
	mem := findMemSink(sink)
	auditQ := func(limit int) []*metrics.AuditRow {
		if mem == nil {
			return []*metrics.AuditRow{}
		}
		rows := mem.Recent(limit)
		out := make([]*metrics.AuditRow, 0, len(rows))
		for _, a := range rows {
			ruleIDs := make([]string, 0, len(a.Hits))
			for _, h := range a.Hits {
				ruleIDs = append(ruleIDs, h.RuleID)
			}
			out = append(out, &metrics.AuditRow{
				DecisionID:     a.DecisionID,
				OccurredAt:     a.OccurredAt.UTC().Format(time.RFC3339Nano),
				Verdict:        a.Verdict,
				RiskScore:      a.RiskScore,
				RiskLevel:      a.RiskLevel,
				HitRules:       ruleIDs,
				EvalDurationMs: a.EvalDurationMs,
			})
		}
		return out
	}
	// sessionReg 把 web-sdk 端点 + admin review/feedback 端点都注册到 metrics
	// mux（同 :9590 端口）。生产推荐 SDK 公网端口 + admin 内网端口分开；当前
	// 简化共用 mux，靠 api-gateway 上层路径鉴权 + IP rate limit 区分。
	//
	// 反馈闭环：注册 review handler 时挂一个 onDecided 钩子 → admin approve/
	// reject 自动落一条 SourceReviewHuman 的 Outcome 到 feedback.Recorder，
	// 让 ML pipeline 能直接 pull 到"风控判 review，运营人工最终判定"的 label。
	var sessionReg metrics.SessionRegisterer
	if sessions != nil || reviewQ != nil || fbRec != nil || mlList != nil || mlDrift != nil {
		sessionReg = func(mux *http.ServeMux) {
			if sessions != nil {
				risksession.RegisterHandlers(mux, sessions, logger)
			}
			if mlList != nil {
				// merchant_list filter：只信赖入站 query.merchant_id（pathScopedAuth 已经
				// 在 admin 路径上做了 token 校验）；后续若引入 grpc principal-into-ctx，
				// 这里改成解析 ctx.Principal 强制覆盖即可。
				merchantlist.RegisterHandlers(mux, mlList, logger, nil)
			}
			if mlDrift != nil {
				registerDriftHandlers(mux, mlDrift, logger)
			}
			// 规则 + 阈值热重载（POST /admin/rules/reload）
			registerRulesReload(mux, eng, v, ruleAudit, logger)
			registerExplainHandler(mux, eng, sink, logger)
			registerAuditSearchHandler(mux, sink, logger)
			registerAuditChainVerifyHandler(mux, sink, logger)
			if reviewQ != nil {
				onDecided := func(it *review.Item) {
					if fbRec != nil {
						if err := fbRec.Record(feedback.Outcome{
							DecisionID: it.ID,
							Source:     feedback.SourceReviewHuman,
							IsFraud:    it.Status == review.StatusRejected,
							Actor:      it.DecidedBy,
							Notes:      it.DecideReason,
						}); err != nil {
							logger.Warn("review→feedback bridge failed", zap.Error(err))
						}
					}
					// 推 webhook 给商户："你的待审单已被运营决议"
					if wh != nil {
						wh.Publish(context.Background(), it.MerchantID, &webhook.Event{
							Type:       webhook.EventReviewDecided,
							MerchantID: it.MerchantID,
							Data: map[string]any{
								"decision_id":       it.ID,
								"status":            string(it.Status),
								"decided_by":        it.DecidedBy,
								"decide_reason":     it.DecideReason,
								"payment_intent_id": it.PaymentIntentID,
								"amount":            it.Amount,
								"currency":          it.Currency,
							},
						})
					}
				}
				review.RegisterHandlersWithHook(mux, reviewQ, logger, onDecided)
			}
			if fbRec != nil {
				feedback.RegisterHandlers(mux, fbRec, logger)
				registerDisputeHandler(mux, fbRec, sink, logger)
			}
			registerRuleInsightsHandler(mux, eng, sink, fbRec, v, logger)
			registerCohortHandler(mux, sink, fbRec, logger)
			registerRuleSimulateHandler(mux, eng, sink, fbRec, logger)
			registerABTestHandler(mux, abTracker, fbRec, logger)
			registerMLOverrideHandler(mux, mlOverride, logger)
			extsignal.RegisterHandlers(mux, extSignal, logger)
			registerWebhookDLQHandler(mux, wh, logger)
			registerWhoamiHandler(mux, logger)
			registerRuleIOHandler(mux, eng, ruleAudit, logger)
			registerChallengerAdminHandler(mux, mlSvc, logger)
			registerRuleOverlapHandler(mux, sink, logger)
			// 聚合 dashboard：把 rule_count / queue depth / model 名 / 决策分布
			// 一次性返给 admin-web Dashboard 页面（避免页面里跑 5 个并行 RPC）。
			dashboard.RegisterHandler(mux, dashboard.Sources{
				RuleCount: func() int { return eng.RuleCount() },
				GetQueueCounts: func() dashboard.QueueCounts {
					if reviewQ == nil {
						return dashboard.QueueCounts{}
					}
					return dashboard.QueueCounts{
						Pending:   reviewQ.CountByStatus(review.StatusPending),
						InReview:  reviewQ.CountByStatus(review.StatusInReview),
						Escalated: reviewQ.CountByStatus(review.StatusEscalated),
						Approved:  reviewQ.CountByStatus(review.StatusApproved),
						Rejected:  reviewQ.CountByStatus(review.StatusRejected),
						Overdue:   len(reviewQ.OverdueSLA(time.Now(), 0)),
					}
				},
				ChampionModel: func() string {
					if cc, ok := mlSvc.(*mlscore.ChampionChallengerService); ok {
						return cc.ChampionName()
					}
					return ""
				},
				ChallengerModels: func() []string {
					if cc, ok := mlSvc.(*mlscore.ChampionChallengerService); ok {
						return cc.ChallengerNames()
					}
					return nil
				},
				RecentAudits: func(limit int) []*audit.DecisionAudit {
					if memSink, ok := sink.(*audit.MemSink); ok {
						return memSink.Recent(limit)
					}
					return nil
				},
				GetRecallStats: func() dashboard.RecallStats {
					if mem, ok := fs.(*featurestore.MemStore); ok {
						days := v.GetInt("recall.window_days")
						if days <= 0 {
							days = 7
						}
						r := mem.ComputeRecall(days)
						return dashboard.RecallStats{
							WindowDays:        r.WindowDays,
							LabeledSamples:    r.LabeledSamples,
							ActualFraud:       r.ActualFraud,
							ActualLegit:       r.ActualLegit,
							TruePositives:     r.TruePositives,
							FalsePositives:    r.FalsePositives,
							TrueNegatives:     r.TrueNegatives,
							FalseNegatives:    r.FalseNegatives,
							PrecisionAtBlock:  r.PrecisionAtBlock,
							RecallAtBlock:     r.RecallAtBlock,
							FalsePositiveRate: r.FalsePositiveRate,
							F1Score:           r.F1Score,
						}
					}
					// EventbusStore 没法本地算（流式），返空让 frontend 看到 "—"
					return dashboard.RecallStats{}
				},
			}, logger)
		}
	}
	// Admin auth：根据 config 选 token 源
	//   admin.tokens_file 非空 → 文件 hot-reload（生产推荐：mount KMS-decrypted secret）
	//   否则 admin.tokens 列表 → 静态启动期固定（dev / 单测）
	//   两者都空 → middleware nil，admin 端点完全放行（dev 默认）
	var adminAuth func(http.Handler) http.Handler
	if path := v.GetString("admin.tokens_file"); path != "" {
		reload := v.GetDuration("admin.tokens_reload")
		src := metrics.NewFileTokenSource(path, reload, logger)
		adminAuth = metrics.AdminAuthFromSource(src)
	} else if toks := v.GetStringSlice("admin.tokens"); len(toks) > 0 {
		adminAuth = metrics.AdminAuthFromSource(metrics.NewStaticTokenSource(toks))
	}
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			metrics.StartServerWithAuth(addr, logger, probe, auditQ, sessionReg, adminAuth)
			// Review queue depth gauge：5s poll
			if reviewQ != nil {
				go func() {
					t := time.NewTicker(5 * time.Second)
					defer t.Stop()
					for range t.C {
						metrics.ReviewQueueDepth.Set(float64(reviewQ.CountByStatus(review.StatusPending)))
					}
				}()
			}
			// ML drift gauges：60s poll，把 DriftMonitor 数据导出到 Prometheus
			if mlDrift != nil {
				go func() {
					t := time.NewTicker(60 * time.Second)
					defer t.Stop()
					for range t.C {
						snap := mlDrift.Snapshot()
						metrics.MLScoreMean.Set(snap.Mean)
						metrics.MLScoreP95.Set(snap.P95)
						drifted, reports := mlDrift.IsDrifted()
						if drifted {
							metrics.MLScoreDrifted.Set(1)
						} else {
							metrics.MLScoreDrifted.Set(0)
						}
						for _, r := range reports {
							if r.Metric == "mean" {
								metrics.MLScoreDriftMeanPct.Set(r.DeltaPct)
							}
						}
					}
				}()
			}
			return nil
		},
		OnStop: func(ctx context.Context) error {
			// Graceful shutdown admin HTTP server：等 in-flight 请求跑完
			// (10s timeout)。避免 fx Stop 直接 SIGKILL 关 listener
			// 导致 admin 调用方(payment-admin-web)收 connection reset。
			return metrics.ShutdownAdmin(ctx, 10*time.Second)
		},
	})
}

// registerRulesReload admin 端点：POST /admin/rules/reload
//   - 重读 viper 配置（rules + score thresholds）
//   - 调 eng.LoadRules + eng.SetScoreThresholds（原子替换）
//   - 返回 { loaded: N, review_min, deny_min }
//
// 配合 ConfigMap 热挂载 + curl POST 即可加 / 改 / 关规则不重启服务。
func registerRulesReload(mux *http.ServeMux, eng *engine.Engine, v *viper.Viper, ruleAudit audit.RuleAuditStore, logger *zap.Logger) {
	mux.HandleFunc("/admin/rules/reload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		if err := v.ReadInConfig(); err != nil {
			logger.Warn("rules reload: ReadInConfig failed", zap.Error(err))
			// 不返错——配置文件不存在也允许走 env / runtime values
		}
		var defs []struct {
			ID       string               `mapstructure:"id"`
			Name     string               `mapstructure:"name"`
			Type     string               `mapstructure:"type"`
			Decision string               `mapstructure:"decision"`
			Enabled  bool                 `mapstructure:"enabled"`
			Mode     string               `mapstructure:"mode"`
			Weight   int                  `mapstructure:"weight"`
			Config   string               `mapstructure:"config"`
			Rollout  engine.RolloutConfig `mapstructure:"rollout"`
		}
		if err := v.UnmarshalKey("rules", &defs); err != nil {
			http.Error(w, `{"error":"unmarshal rules failed"}`, http.StatusInternalServerError)
			return
		}
		engineDefs := make([]engine.RuleDef, len(defs))
		for i, d := range defs {
			engineDefs[i] = engine.RuleDef{
				ID: d.ID, Name: d.Name, Type: d.Type, Decision: d.Decision,
				Enabled: d.Enabled, Mode: d.Mode, Weight: d.Weight,
				ConfigJSON: json.RawMessage(d.Config),
				Rollout:    d.Rollout,
			}
		}
		if err := eng.LoadRules(engineDefs); err != nil {
			logger.Warn("rules reload: LoadRules failed", zap.Error(err))
			http.Error(w, `{"error":"load failed"}`, http.StatusInternalServerError)
			return
		}
		// thresholds 也热重载
		rmin := v.GetInt("score_thresholds.review_min")
		dmin := v.GetInt("score_thresholds.deny_min")
		if rmin > 0 || dmin > 0 {
			eng.SetScoreThresholds(rmin, dmin)
		}
		t := eng.ScoreThresholds()
		logger.Info("rules hot-reloaded",
			zap.Int("count", eng.RuleCount()),
			zap.Int("review_min", t.ReviewMin),
			zap.Int("deny_min", t.DenyMin))
		actor := "unknown"
		if p, ok := auth.PrincipalFrom(r.Context()); ok && p != nil {
			actor = p.KeyID
		}
		afterJSON, _ := json.Marshal(map[string]any{
			"count":      eng.RuleCount(),
			"review_min": t.ReviewMin,
			"deny_min":   t.DenyMin,
		})
		_ = ruleAuditWrite(ruleAudit, audit.RuleAuditEntry{
			Action: "reload",
			Actor:  actor,
			After:  afterJSON,
		})
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"loaded":     eng.RuleCount(),
			"review_min": t.ReviewMin,
			"deny_min":   t.DenyMin,
		})
	})
	// POST /admin/rules/mode  Body: {"id":"<rule>","shadow":true}
	// 不重启切换规则 mode（enforce ↔ shadow）。给规则预测试上线流程用：
	//   1) 新规则 yaml 配 mode: shadow（或上线后 POST 这里切 shadow）
	//   2) 跑 N 天观察 shadow_hits，对照真实 chargeback 判效果
	//   3) POST 这里切回 enforce，规则进入决策路径
	mux.HandleFunc("/admin/rules/mode", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			ID     string `json:"id"`
			Shadow bool   `json:"shadow"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		if body.ID == "" {
			http.Error(w, `{"error":"id required"}`, http.StatusBadRequest)
			return
		}
		if !eng.SetRuleMode(body.ID, body.Shadow) {
			http.Error(w, `{"error":"rule not found"}`, http.StatusNotFound)
			return
		}
		mode := "enforce"
		if body.Shadow {
			mode = "shadow"
		}
		logger.Info("rule mode toggled",
			zap.String("rule_id", body.ID), zap.String("mode", mode))
		// 写规则审计：actor 从 ctx Principal 取（admin auth middleware 已经
		// inject）；fallback "unknown"
		actor := "unknown"
		if p, ok := auth.PrincipalFrom(r.Context()); ok && p != nil {
			actor = p.KeyID
		}
		_ = ruleAuditWrite(ruleAudit, audit.RuleAuditEntry{
			Action: "mode_change",
			Actor:  actor,
			RuleID: body.ID,
			After:  json.RawMessage(`{"mode":"` + mode + `"}`),
		})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":   body.ID,
			"mode": mode,
		})
	})
	// GET /admin/rules/audit?rule_id=X&limit=100  → 规则变更历史
	mux.HandleFunc("/admin/rules/audit", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		ruleID := r.URL.Query().Get("rule_id")
		limit := 100
		if l := r.URL.Query().Get("limit"); l != "" {
			if n, err := strconvAtoi(l); err == nil && n > 0 {
				limit = n
			}
		}
		var rows []*audit.RuleAuditEntry
		if ruleAudit != nil {
			if ruleID == "" {
				rows = ruleAudit.Recent(limit)
			} else {
				rows = ruleAudit.ByRuleID(ruleID, limit)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rows)
	})
	// GET /admin/rules/list  → 当前在跑的规则集（id+name+type+config_json+
	// mode+weight+rollout+enabled），给 admin-web 规则编辑 UI 渲染表格用。
	mux.HandleFunc("/admin/rules/list", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(eng.RuleDefs())
	})
	// POST /admin/rules/update  Body: RuleDef{...}
	// 单条规则原子热更新：先 BuildRule 跑 schema 校验（factory + json.Unmarshal），
	// 通过才落 engine。失败返 400 + 错误明细，UI 直接显示给运营。
	// 已存在 id → update；新 id → create。
	mux.HandleFunc("/admin/rules/update", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		var d engine.RuleDef
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&d); err != nil {
			http.Error(w, `{"error":"invalid json: `+err.Error()+`"}`, http.StatusBadRequest)
			return
		}
		if d.ID == "" || d.Type == "" {
			http.Error(w, `{"error":"id and type required"}`, http.StatusBadRequest)
			return
		}
		// 找 before 状态（给审计 diff 用）。NB：必须在 UpdateRule 之前抓。
		var beforeJSON json.RawMessage
		for _, existing := range eng.RuleDefs() {
			if existing.ID == d.ID {
				beforeJSON, _ = json.Marshal(existing)
				break
			}
		}
		built, err := eng.BuildRule(d)
		if err != nil {
			logger.Warn("rules update: build failed",
				zap.String("rule_id", d.ID), zap.Error(err))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		updated := eng.UpdateRule(d, built)
		action := "create"
		if updated {
			action = "update"
		}
		actor := "unknown"
		if p, ok := auth.PrincipalFrom(r.Context()); ok && p != nil {
			actor = p.KeyID
		}
		afterJSON, _ := json.Marshal(d)
		_ = ruleAuditWrite(ruleAudit, audit.RuleAuditEntry{
			Action: action,
			Actor:  actor,
			RuleID: d.ID,
			Before: beforeJSON,
			After:  afterJSON,
		})
		logger.Info("rule "+action,
			zap.String("rule_id", d.ID),
			zap.String("type", d.Type),
			zap.String("actor", actor))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     d.ID,
			"action": action,
		})
	})
	// POST /admin/rules/delete  Body: {"id":"<rule>","reason":"..."}
	// 软停：从 engine 移除规则（不再参与决策），下一次 reload 从 yaml 重新长出来
	// 除非 yaml 也改了。给运营紧急关规则用（误伤大量交易时按掉）。
	mux.HandleFunc("/admin/rules/delete", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			ID     string `json:"id"`
			Reason string `json:"reason"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		if body.ID == "" {
			http.Error(w, `{"error":"id required"}`, http.StatusBadRequest)
			return
		}
		var beforeJSON json.RawMessage
		for _, existing := range eng.RuleDefs() {
			if existing.ID == body.ID {
				beforeJSON, _ = json.Marshal(existing)
				break
			}
		}
		if !eng.RemoveRule(body.ID) {
			http.Error(w, `{"error":"rule not found"}`, http.StatusNotFound)
			return
		}
		actor := "unknown"
		if p, ok := auth.PrincipalFrom(r.Context()); ok && p != nil {
			actor = p.KeyID
		}
		_ = ruleAuditWrite(ruleAudit, audit.RuleAuditEntry{
			Action: "delete",
			Actor:  actor,
			RuleID: body.ID,
			Before: beforeJSON,
			Reason: body.Reason,
		})
		logger.Info("rule deleted",
			zap.String("rule_id", body.ID),
			zap.String("actor", actor),
			zap.String("reason", body.Reason))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      body.ID,
			"deleted": true,
		})
	})
}

// ruleAuditWrite nil-safe 包装。
func ruleAuditWrite(s audit.RuleAuditStore, e audit.RuleAuditEntry) error {
	if s == nil {
		return nil
	}
	return s.Write(context.Background(), e)
}

// strconvAtoi 给小整数手动 parse 避免 strconv import 在 build tag 下不必要。
func strconvAtoi(s string) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, &parseErr{s: s}
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

type parseErr struct{ s string }

func (e *parseErr) Error() string { return "not a number: " + e.s }

// registerExplainHandler  POST /admin/explain  Body: {decision_id}
//
// 把指定 decision_id 对应的历史 audit 行重新跑一遍 engine.Evaluate，返回
// 完整规则评估链 + 命中详情 + score。给运营调试客户投诉 / 误伤分析用。
//
// 注意：本端点用**当前**规则集 re-evaluate；如果规则改过，结果可能跟原决策
// 不一致 — 这正是想看的（"我现在的规则会怎么判这笔？"）。
func registerExplainHandler(mux *http.ServeMux, eng *engine.Engine, sink audit.Sink, logger *zap.Logger) {
	mux.HandleFunc("/admin/explain", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			DecisionID string `json:"decision_id"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		if body.DecisionID == "" {
			http.Error(w, `{"error":"decision_id required"}`, http.StatusBadRequest)
			return
		}
		mem := findMemSink(sink)
		if mem == nil {
			http.Error(w, `{"error":"audit sink doesn't support Recent()"}`, http.StatusServiceUnavailable)
			return
		}
		// 拉最近 10k 条找匹配；生产 ClickHouse sink 应直接 SELECT WHERE id=
		audits := mem.Recent(10000)
		var found *audit.DecisionAudit
		for _, a := range audits {
			if a.DecisionID == body.DecisionID {
				found = a
				break
			}
		}
		if found == nil {
			http.Error(w, `{"error":"decision not found in audit ring"}`, http.StatusNotFound)
			return
		}
		// 用 audit.Input 重建 TxnContext，跑一遍当前 engine
		txn := &engine.TxnContext{
			PaymentIntentID: found.Input.PaymentIntentID,
			MerchantID:      found.Input.MerchantID,
			CustomerID:      found.Input.CustomerID,
			Amount:          found.Input.Amount,
			Currency:        found.Input.Currency,
			PaymentMethod:   found.Input.PaymentMethod,
			Country:         found.Input.Country,
			IPAddress:       found.Input.IPAddress,
			DeviceID:        found.Input.DeviceID,
			Metadata:        found.Input.Metadata,
		}
		res := eng.Evaluate(r.Context(), txn)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"decision_id":  body.DecisionID,
			"original": map[string]any{
				"verdict":    found.Verdict,
				"risk_score": found.RiskScore,
				"hits":       found.Hits,
				"occurred_at": found.OccurredAt,
			},
			"replayed": map[string]any{
				"verdict":      res.Decision.String(),
				"risk_score":   res.RiskScore,
				"hits":         res.Hits,
				"shadow_hits":  res.ShadowHits,
			},
			"input": found.Input,
		})
	})
}

// registerDriftHandlers admin 端点：
//
//	GET  /admin/mlscore/drift            当前快照 + 是否漂移 + 报告
//	POST /admin/mlscore/drift/baseline   把当前快照设为新基线（新模型上线后调）
func registerDriftHandlers(mux *http.ServeMux, m *mlscore.DriftMonitor, logger *zap.Logger) {
	mux.HandleFunc("/admin/mlscore/drift", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		drifted, reports := m.IsDrifted()
		body := map[string]any{
			"current":         m.Snapshot(),
			"baseline":        m.Baseline(),
			"total_observed":  m.TotalObserved(),
			"drifted":         drifted,
			"reports":         reports,
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc("/admin/mlscore/drift/baseline", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		s := m.Snapshot()
		m.SetBaseline(s)
		logger.Info("ml drift baseline updated", zap.Any("snapshot", s))
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"baseline": s})
	})
}

// findMemSink 在 MultiSink 里挑出第一个 *audit.MemSink；没有就返回 nil。
func findMemSink(s audit.Sink) *audit.MemSink {
	switch v := s.(type) {
	case *audit.MemSink:
		return v
	case audit.MultiSink:
		for _, sub := range v {
			if mem, ok := sub.(*audit.MemSink); ok {
				return mem
			}
		}
	}
	return nil
}

// findAsyncSink 在 MultiSink 里挑出第一个 AsyncBatchSink；nil 时无 file/Kafka 后端。
func findAsyncSink(s audit.Sink) *audit.AsyncBatchSink {
	if as, ok := s.(*audit.AsyncBatchSink); ok {
		return as
	}
	if multi, ok := s.(audit.MultiSink); ok {
		for _, sub := range multi {
			if as, ok := sub.(*audit.AsyncBatchSink); ok {
				return as
			}
		}
	}
	return nil
}

// findFileSink 提取 FileSink 给 metrics size gauge 用。多层 wrap 时（chain
// → multi → async → file）也要找到。
func findFileSink(s audit.Sink) *audit.FileSink {
	switch v := s.(type) {
	case *audit.FileSink:
		return v
	case audit.MultiSink:
		for _, sub := range v {
			if f := findFileSink(sub); f != nil {
				return f
			}
		}
	}
	return nil
}

// startStorageMetricsWorker 周期采样存储层内部状态写到 Prometheus gauges。
// 包括 audit async/file sink + counter cache hit rate + webhook DLQ depth。
// 15s 一次足够（dashboards 通常 1 分钟级聚合）。
//
// 改名 + 收敛：原 startAuditMetricsWorker 只处理 audit；现合并 counter +
// webhook DLQ，避免开多个 ticker。
func startStorageMetricsWorker(lc fx.Lifecycle, sink audit.Sink, counter store.Counter, wh *webhook.Publisher, logger *zap.Logger) {
	async := findAsyncSink(sink)
	file := findFileSink(sink)
	cached, _ := counter.(*store.CachedCounter)
	var dlq webhook.DLQStore
	if wh != nil {
		dlq = wh.DLQ()
	}
	// runtime gauges (goroutine / heap) 总是采样；其它仅在 sink 配了对应
	// 后端时刷新。所以 worker 永不退（即使 audit / counter / dlq 都 nil
	// 也要采 runtime 指标）。
	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			go func() {
				ticker := time.NewTicker(15 * time.Second)
				defer ticker.Stop()
				lastHits, lastMisses := uint64(0), uint64(0)
				var memStats runtime.MemStats
				for {
					// runtime gauges (goroutine 泄露 / heap pressure 早期信号)
					metrics.GoroutineCount.Set(float64(runtime.NumGoroutine()))
					runtime.ReadMemStats(&memStats)
					metrics.HeapAllocBytes.Set(float64(memStats.HeapAlloc))
					metrics.HeapInuseBytes.Set(float64(memStats.HeapInuse))
					metrics.GCCount.Set(float64(memStats.NumGC))

					if async != nil {
						metrics.AuditAsyncDropped.Set(float64(async.Dropped()))
						metrics.AuditAsyncQueueLen.Set(float64(async.QueueLen()))
					}
					if file != nil {
						metrics.AuditFileSinkSize.Set(float64(file.CurrentSize()))
					}
					if cached != nil {
						st := cached.CacheStats()
						// counter (累积) — 算 delta 加到 Prometheus counter
						if d := st.Hits - lastHits; d > 0 {
							metrics.CounterCacheHit.Add(float64(d))
						}
						if d := st.Misses - lastMisses; d > 0 {
							metrics.CounterCacheMiss.Add(float64(d))
						}
						lastHits, lastMisses = st.Hits, st.Misses
						metrics.CounterCacheSize.Set(float64(cached.CacheSize()))
					}
					if dlq != nil {
						if mem, ok := dlq.(*webhook.MemDLQStore); ok {
							metrics.WebhookDLQDepth.Set(float64(mem.Size()))
						}
					}
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
					}
				}
			}()
			return nil
		},
		OnStop: func(_ context.Context) error { cancel(); return nil },
	})
	logger.Info("storage metrics worker started (audit / counter / webhook DLQ)")
}

// registerDisputeHandler  POST /admin/feedback/dispute
//
// 给 order-core / dispute 系统在终态（lost / charge_refunded / won）调：
// 入参用 payment_intent_id（不是 decision_id），handler 反查 audit MemSink
// 拿到决策 id 再写 outcome。这样 dispute 上游不需要存 decision_id，只要
// 知道是哪笔 PI 出了 chargeback 即可。
//
// Body:
//
//	{
//	  "payment_intent_id": "pi_xxx",
//	  "dispute_id":        "dp_xxx",   // 给 idempotency
//	  "is_fraud":          true,        // 终态判定（lost / charge_refunded → true；won → false）
//	  "actor":             "order-core",
//	  "notes":             "stripe.code=fraudulent",
//	  "occurred_at":       "RFC3339",   // 可选；缺省用 now
//	}
//
// 反查不到 decision_id（PI 太老被 ring 覆写 / 风控没处理过该 PI）→ 404，
// 调用方应该 retry / fallback 到长期存储查询。
//
// 副作用：
//   - feedback.Recorder.Record 写一条 SourceDispute outcome
//   - metrics.OutcomeTotal{source=dispute,is_fraud=...} +1
//   - metrics.OutcomeLagSeconds{source=dispute} 记录 (now - decision OccurredAt)
func registerDisputeHandler(mux *http.ServeMux, fbRec feedback.Recorder, sink audit.Sink, logger *zap.Logger) {
	mem := findMemSink(sink)
	mux.HandleFunc("/admin/feedback/dispute", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			PaymentIntentID string    `json:"payment_intent_id"`
			DisputeID       string    `json:"dispute_id"`
			IsFraud         bool      `json:"is_fraud"`
			Actor           string    `json:"actor"`
			Notes           string    `json:"notes"`
			OccurredAt      time.Time `json:"occurred_at"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		if body.PaymentIntentID == "" {
			http.Error(w, `{"error":"payment_intent_id required"}`, http.StatusBadRequest)
			return
		}
		if mem == nil {
			http.Error(w, `{"error":"audit MemSink not configured"}`, http.StatusServiceUnavailable)
			return
		}
		decisionID, ok := mem.LookupByPaymentIntent(body.PaymentIntentID)
		if !ok {
			logger.Info("dispute lookup miss",
				zap.String("payment_intent_id", body.PaymentIntentID),
				zap.String("dispute_id", body.DisputeID))
			http.Error(w, `{"error":"decision not found for payment_intent_id (too old or unknown)"}`, http.StatusNotFound)
			return
		}
		if body.OccurredAt.IsZero() {
			body.OccurredAt = time.Now().UTC()
		}
		notes := body.Notes
		if body.DisputeID != "" {
			// 把 dispute_id 嵌进 notes 给后续审计追溯用，无需新加 schema 字段。
			if notes != "" {
				notes = "dispute_id=" + body.DisputeID + "; " + notes
			} else {
				notes = "dispute_id=" + body.DisputeID
			}
		}
		if err := fbRec.Record(feedback.Outcome{
			DecisionID: decisionID,
			Source:     feedback.SourceDispute,
			IsFraud:    body.IsFraud,
			At:         body.OccurredAt,
			Actor:      body.Actor,
			Notes:      notes,
		}); err != nil {
			logger.Warn("dispute feedback record failed", zap.Error(err))
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
			return
		}
		// 指标：counter + lag histogram
		fraudLabel := "false"
		if body.IsFraud {
			fraudLabel = "true"
		}
		metrics.OutcomeTotal.WithLabelValues(string(feedback.SourceDispute), fraudLabel).Inc()
		if a := mem.LookupByDecisionID(decisionID); a != nil && !a.OccurredAt.IsZero() {
			lag := body.OccurredAt.Sub(a.OccurredAt).Seconds()
			if lag > 0 {
				metrics.OutcomeLagSeconds.WithLabelValues(string(feedback.SourceDispute)).Observe(lag)
			}
		}
		logger.Info("dispute outcome recorded",
			zap.String("payment_intent_id", body.PaymentIntentID),
			zap.String("decision_id", decisionID),
			zap.String("dispute_id", body.DisputeID),
			zap.Bool("is_fraud", body.IsFraud))
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":      "ok",
			"decision_id": decisionID,
		})
	})
}

// startCoverageWorker 周期性算 outcome coverage（最近 windowDays 天的决策中
// 至少有一条 outcome 反馈的比例），按 verdict 拆分写到 OutcomeCoverageRatio
// gauge。给 SRE alerting 用："最近 7d DENY 决策 coverage < 50% 持续 1h" =
// dispute hook 挂了 / 反馈链路坏了。
//
// 实现：5 分钟一次，不挂硬依赖（fx Lifecycle 用 ctx 关闭）。
// 数据源：audit MemSink Recent + feedback.Recorder.Get(decision_id)。
// 注意：MemSink 是 ring buffer（cap=4096），所以本统计实际窗口 = min(windowDays,
// ring 容量)；生产应该用 ClickHouse 长期存储替换计算路径。
func startCoverageWorker(lc fx.Lifecycle, sink audit.Sink, fbRec feedback.Recorder, v *viper.Viper, logger *zap.Logger) {
	windowDays := v.GetInt("outcome_coverage.window_days")
	if windowDays <= 0 {
		windowDays = 7
	}
	mem := findMemSink(sink)
	if mem == nil || fbRec == nil {
		logger.Info("coverage worker: skipping (no MemSink or feedback recorder)")
		return
	}
	metrics.OutcomeCoverageWindow.Set(float64(windowDays * 86400))
	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			go func() {
				// 启动 60s 后跑首次（让 audit / feedback 数据初步进来）
				select {
				case <-ctx.Done():
					return
				case <-time.After(60 * time.Second):
				}
				ticker := time.NewTicker(5 * time.Minute)
				defer ticker.Stop()
				for {
					computeCoverage(mem, fbRec, windowDays, logger)
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
					}
				}
			}()
			return nil
		},
		OnStop: func(_ context.Context) error { cancel(); return nil },
	})
}

// registerRuleInsightsHandler  GET /admin/rules/insights
//
// 返回每条规则的 KPI 快照：last_hit_at / hits / precision / ROI。
// 给运营按 ROI 倒序看哪些规则该留 / 改 / 删。
//
// silenceWindow 走 viper rule_insights.silence_window_days（默认 7d）。
func registerRuleInsightsHandler(mux *http.ServeMux, eng *engine.Engine, sink audit.Sink, fbRec feedback.Recorder, v *viper.Viper, logger *zap.Logger) {
	mux.HandleFunc("/admin/rules/insights", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		mem := findMemSink(sink)
		var audits []*audit.DecisionAudit
		if mem != nil {
			audits = mem.Recent(0)
		}
		// 当前规则 id 列表（含从未命中的）— 让运营看到"X 规则 7 天没命中"
		var allIDs []string
		for _, d := range eng.RuleDefs() {
			allIDs = append(allIDs, d.ID)
		}
		windowDays := v.GetInt("rule_insights.silence_window_days")
		if windowDays <= 0 {
			windowDays = 7
		}
		stats := ruleinsights.Compute(audits, fbRec, allIDs, time.Duration(windowDays)*24*time.Hour)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"silence_window_days": windowDays,
			"rules":               stats,
		})
	})
	logger.Info("rule insights handler registered at /admin/rules/insights")
}

// startRuleInsightsWorker 后台周期性更新 RuleLastHitAge / RulePrecision /
// RuleROI Prometheus gauges。给 alerting 用 ("规则 X 7 天没命中" / "规则 Y
// precision < 0.3 持续 1d")。每 5 分钟一次。
func startRuleInsightsWorker(lc fx.Lifecycle, eng *engine.Engine, sink audit.Sink, fbRec feedback.Recorder, v *viper.Viper, logger *zap.Logger) {
	mem := findMemSink(sink)
	if mem == nil {
		logger.Info("rule insights worker: skipping (no MemSink)")
		return
	}
	windowDays := v.GetInt("rule_insights.silence_window_days")
	if windowDays <= 0 {
		windowDays = 7
	}
	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			go func() {
				select {
				case <-ctx.Done():
					return
				case <-time.After(60 * time.Second):
				}
				ticker := time.NewTicker(5 * time.Minute)
				defer ticker.Stop()
				for {
					updateRuleInsights(eng, mem, fbRec, windowDays, logger)
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
					}
				}
			}()
			return nil
		},
		OnStop: func(_ context.Context) error { cancel(); return nil },
	})
}

func updateRuleInsights(eng *engine.Engine, mem *audit.MemSink, fbRec feedback.Recorder, windowDays int, logger *zap.Logger) {
	var allIDs []string
	for _, d := range eng.RuleDefs() {
		allIDs = append(allIDs, d.ID)
	}
	audits := mem.Recent(0)
	stats := ruleinsights.Compute(audits, fbRec, allIDs, time.Duration(windowDays)*24*time.Hour)
	for _, s := range stats {
		ageSec := -1.0
		if !s.LastHitAt.IsZero() {
			ageSec = time.Since(s.LastHitAt).Seconds()
		}
		metrics.RuleLastHitAge.WithLabelValues(s.RuleID).Set(ageSec)
		metrics.RulePrecision.WithLabelValues(s.RuleID).Set(s.Precision)
		metrics.RuleROI.WithLabelValues(s.RuleID).Set(s.ROI)
	}
	logger.Info("rule insights sampled",
		zap.Int("rules", len(stats)),
		zap.Int("window_days", windowDays))
}

// registerRuleSimulateHandler  POST /admin/rules/simulate
//
// Body: 一个 RuleDef（同 /admin/rules/update），但**不**写入 engine。
// 处理流程：
//   1) BuildRule 跑 schema 校验 + 包装链（同 update 路径）
//   2) 把 audit MemSink 的最近 N 条决策当作 fixture 重放
//   3) 用候选规则 Evaluate 每一条，统计 hit / would_newly_block /
//      would_keep_block / 估算 precision（要求 hit 里 ≥ 5 条有 outcome）
//   4) 返回汇总 + 提示（如 "依赖 SDK 信号 → 估值偏低，建议先 shadow 跑"）
//
// query: ?sample=N 控制重放样本量，default 全量；max=10000 防止 OOM。
//
// 给"规则编辑流程"用：写完候选规则点 "试运行" 立即看到效果再决定上不上线。
// 砍掉"先 shadow 跑一周"的等待时间。
func registerRuleSimulateHandler(mux *http.ServeMux, eng *engine.Engine, sink audit.Sink, fbRec feedback.Recorder, logger *zap.Logger) {
	mux.HandleFunc("/admin/rules/simulate", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		var d engine.RuleDef
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&d); err != nil {
			http.Error(w, `{"error":"invalid json: `+err.Error()+`"}`, http.StatusBadRequest)
			return
		}
		if d.Type == "" {
			http.Error(w, `{"error":"type required"}`, http.StatusBadRequest)
			return
		}
		// 走 schema 校验。失败 → 400 让 UI 直接回显（跟 /admin/rules/update 一致）
		built, err := eng.BuildRule(d)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		mem := findMemSink(sink)
		var audits []*audit.DecisionAudit
		if mem != nil {
			audits = mem.Recent(0)
		}
		// 样本上限：避免 5000 条规则 × 10000 audit OOM 主进程。
		const maxSample = 10000
		if n := r.URL.Query().Get("sample"); n != "" {
			if v, err := strconvAtoi(n); err == nil && v > 0 && v < len(audits) {
				audits = audits[:v]
			}
		}
		if len(audits) > maxSample {
			audits = audits[:maxSample]
		}
		res := rulesim.Simulate(r.Context(), built, audits, fbRec)
		logger.Info("rule simulate",
			zap.String("rule_id", d.ID),
			zap.String("type", d.Type),
			zap.Int("sample", res.Sample),
			zap.Int("hits", res.Hits))
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(res)
	})
}

// registerCohortHandler  GET /admin/dashboard/cohort?group_by=<merchant_id|country|payment_method>&min_total=N
//
// 按运营维度（默认商户）拆分决策 + outcome，给运营 dashboard 看：
//   - X 商户最近一周的 block_rate 12%（远高于平台平均 3%）→ 触发对账
//   - Y 国家的 actual_fraud_rate 飙升 → 加规则 / 加 sanction list
//   - 某 payment_method 的 precision 暴跌 → 该渠道 fraud pattern 变了
//
// 数据源：audit.MemSink (cap=4096 ring) + feedback.Recorder。生产应该
// 接 ClickHouse 长期聚合表，本端点是 in-mem 实时近窗口视图。
func registerCohortHandler(mux *http.ServeMux, sink audit.Sink, fbRec feedback.Recorder, logger *zap.Logger) {
	mux.HandleFunc("/admin/dashboard/cohort", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		gb := cohort.GroupBy(r.URL.Query().Get("group_by"))
		switch gb {
		case cohort.GroupByMerchant, cohort.GroupByCountry, cohort.GroupByPaymentMethod:
			// ok
		case "":
			gb = cohort.GroupByMerchant // 缺省按商户
		default:
			http.Error(w, `{"error":"unsupported group_by"}`, http.StatusBadRequest)
			return
		}
		minTotal := 0
		if v := r.URL.Query().Get("min_total"); v != "" {
			if n, err := strconvAtoi(v); err == nil && n > 0 {
				minTotal = n
			}
		}
		mem := findMemSink(sink)
		var audits []*audit.DecisionAudit
		if mem != nil {
			audits = mem.Recent(0)
		}
		stats := cohort.Compute(audits, fbRec, gb, minTotal)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"group_by":  string(gb),
			"min_total": minTotal,
			"sample":    len(audits),
			"cohorts":   stats,
		})
	})
	// GET /admin/dashboard/cohort/timeseries?group_by=&key=&bucket=hour|day|week
	//
	// 时间序列 cohort：给指定 cohort（或全平台空 key）画 block_rate /
	// fraud_rate 趋势图。bucket=day 缺省。
	mux.HandleFunc("/admin/dashboard/cohort/timeseries", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		gb := cohort.GroupBy(r.URL.Query().Get("group_by"))
		switch gb {
		case cohort.GroupByMerchant, cohort.GroupByCountry, cohort.GroupByPaymentMethod:
		case "":
			gb = cohort.GroupByMerchant
		default:
			http.Error(w, `{"error":"unsupported group_by"}`, http.StatusBadRequest)
			return
		}
		key := r.URL.Query().Get("key")
		bucket := cohort.Bucket(r.URL.Query().Get("bucket"))
		switch bucket {
		case cohort.BucketHour, cohort.BucketDay, cohort.BucketWeek:
		case "":
			bucket = cohort.BucketDay
		default:
			http.Error(w, `{"error":"unsupported bucket"}`, http.StatusBadRequest)
			return
		}
		mem := findMemSink(sink)
		var audits []*audit.DecisionAudit
		if mem != nil {
			audits = mem.Recent(0)
		}
		series := cohort.TimeSeries(audits, fbRec, gb, key, bucket)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"group_by": string(gb),
			"key":      key,
			"bucket":   string(bucket),
			"sample":   len(audits),
			"series":   series,
		})
	})
	logger.Info("cohort handler registered at /admin/dashboard/cohort and /timeseries")
}

// computeCoverage 单次计算覆盖率。public 拆出来方便单测。
func computeCoverage(mem *audit.MemSink, fbRec feedback.Recorder, windowDays int, logger *zap.Logger) {
	cutoff := time.Now().Add(-time.Duration(windowDays) * 24 * time.Hour)
	all := mem.Recent(0)
	var totals, covered map[string]int = map[string]int{}, map[string]int{}
	for _, a := range all {
		if a == nil || a.OccurredAt.Before(cutoff) {
			continue
		}
		v := a.Verdict
		if v == "" {
			v = "UNKNOWN"
		}
		totals[v]++
		if outs := fbRec.Get(a.DecisionID); len(outs) > 0 {
			covered[v]++
		}
	}
	for v, n := range totals {
		ratio := 0.0
		if n > 0 {
			ratio = float64(covered[v]) / float64(n)
		}
		metrics.OutcomeCoverageRatio.WithLabelValues(v).Set(ratio)
	}
	logger.Info("outcome coverage sampled",
		zap.Int("window_days", windowDays),
		zap.Any("totals", totals),
		zap.Any("covered", covered))
}

// registerABTestHandler  GET /admin/mlscore/abtest?min_labeled=N&bootstrap=N
//
// 跑 A/B 显著性检验：从 ABTracker 取 (decision_id, champion_score,
// challenger_scores)，跟 feedback.Recorder 反查 outcome label，对每个
// challenger 算：
//   - LabeledSamples
//   - ChampionAUC / ChallengerAUC / AUCDiff
//   - 95% bootstrap CI on AUC diff
//   - Recommendation: promote / hold / drop（CI 全 > 0 / 全 < 0 / 跨 0）
//
// 给运营升级 ML 模型的 quantitative 决策依据，避免靠肉眼看 "AUC 高 0.02"。
func registerABTestHandler(mux *http.ServeMux, tr *mlscore.ABTracker, fbRec feedback.Recorder, logger *zap.Logger) {
	mux.HandleFunc("/admin/mlscore/abtest", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		if tr == nil || fbRec == nil {
			http.Error(w, `{"error":"ABTracker or feedback recorder not configured"}`, http.StatusServiceUnavailable)
			return
		}
		minLabeled := 30
		if v := r.URL.Query().Get("min_labeled"); v != "" {
			if n, err := strconvAtoi(v); err == nil && n > 0 {
				minLabeled = n
			}
		}
		bootstrap := 1000
		if v := r.URL.Query().Get("bootstrap"); v != "" {
			if n, err := strconvAtoi(v); err == nil && n >= 0 {
				bootstrap = n
			}
		}
		// adapter：用 feedback.Recorder.Get 拉 outcome label。多条 outcome
		// 时只要一条 IsFraud=true 就算 fraud（多源反馈通常一致）。
		getOutcome := func(decisionID string) (bool, bool) {
			outs := fbRec.Get(decisionID)
			if len(outs) == 0 {
				return false, false
			}
			for _, o := range outs {
				if o.IsFraud {
					return true, true
				}
			}
			return false, true
		}
		report := tr.Report(getOutcome, minLabeled, bootstrap)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"min_labeled": minLabeled,
			"bootstrap":   bootstrap,
			"report":      report,
		})
	})
	logger.Info("abtest handler registered at /admin/mlscore/abtest")
}

// startABTrackerWiring 把 ABTracker.Record 接到 ChampionChallengerService
// 的 SideEffect 钩子。SideEffect 只在 mlSvc 是 *ChampionChallengerService
// 时有效（默认 newMLScore 总是返回 C-C 包装，所以总会接上）。
//
// 注意：现在 ChampionChallengerService.Score 调用方传 "" 作 decisionID
// （只有 ScoreWithDecisionID 才传真实 id）。要让 tracker 真正收到数据，
// 调用 service/risk.go 必须改用 ScoreWithDecisionID — 见后续 PR。
func startABTrackerWiring(mlSvc mlscore.Service, tr *mlscore.ABTracker, logger *zap.Logger) {
	cc, ok := mlSvc.(*mlscore.ChampionChallengerService)
	if !ok {
		logger.Info("abtest: mlSvc not a champion-challenger; ABTracker idle")
		return
	}
	cc.SideEffect = func(decisionID string, championResult mlscore.Result, results []mlscore.NamedResult) {
		tr.Record(decisionID, championResult.Score, results)
	}
	logger.Info("abtest: SideEffect wired to ABTracker")
}

// registerRuleIOHandler 规则集 YAML 导入/导出，给运营跨环境推送规则用：
//
//	GET  /admin/rules/export  → text/yaml，当前规则集（可直接 paste 到下个环境）
//	POST /admin/rules/import  Body: YAML; ?dry_run=true 默认（只验证 + 返回 diff）
//
// dry_run=false 时所有规则过 BuildRule 全部成功才落地，单条失败 → 整批拒绝
// （不会半套生效）。每条变更都写 rule_audit。
func registerRuleIOHandler(mux *http.ServeMux, eng *engine.Engine, ruleAuditStore audit.RuleAuditStore, logger *zap.Logger) {
	mux.HandleFunc("/admin/rules/export", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		data, err := ruleio.Marshal(eng.RuleDefs())
		if err != nil {
			http.Error(w, `{"error":"marshal failed"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="risk-rules.yaml"`)
		_, _ = w.Write(data)
	})

	mux.HandleFunc("/admin/rules/import", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		// ?dry_run=true 默认。只有显式 false 才真正写。
		dryRun := true
		if v := r.URL.Query().Get("dry_run"); v == "false" {
			dryRun = false
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20)) // 1MB max
		if err != nil {
			http.Error(w, `{"error":"read body failed"}`, http.StatusBadRequest)
			return
		}
		imported, err := ruleio.Unmarshal(body)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		// 全部规则过 BuildRule schema 校验；任一失败 → 整批拒绝
		built := make([]engine.Rule, 0, len(imported))
		for _, d := range imported {
			r, err := eng.BuildRule(d)
			if err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{
					"error": "rule " + d.ID + ": " + err.Error(),
				})
				return
			}
			built = append(built, r)
		}
		current := eng.RuleDefs()
		diff := ruleio.Diff(current, imported)
		resp := map[string]any{
			"dry_run": dryRun,
			"imported": len(imported),
			"diff":    diff,
		}
		if dryRun {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		// 真正落地：先 Replace（atomic），再写 audit
		// LoadRules 会再走一遍 factory，已经在上面预校验过所以应该全过；
		// 二次失败 → 内部 race 或 factory 注册变了 → 500
		_ = built // 上面预校验时构造的 Rule slice 留给 LoadRules 重新生成
		if err := eng.LoadRules(imported); err != nil {
			http.Error(w, `{"error":"load failed: `+err.Error()+`"}`, http.StatusInternalServerError)
			return
		}
		actor := "unknown"
		if p, ok := auth.PrincipalFrom(r.Context()); ok && p != nil {
			actor = p.KeyID
		}
		// 每条 diff 写一行 audit（让运营在审计页看清楚谁在什么时候改了什么）
		for _, e := range diff {
			if e.Action == "unchanged" {
				continue
			}
			beforeJSON, _ := json.Marshal(e.Before)
			afterJSON, _ := json.Marshal(e.After)
			_ = ruleAuditWrite(ruleAuditStore, audit.RuleAuditEntry{
				Action: "import_" + e.Action,
				Actor:  actor,
				RuleID: e.RuleID,
				Before: beforeJSON,
				After:  afterJSON,
			})
		}
		logger.Info("rules imported",
			zap.Int("count", len(imported)), zap.String("actor", actor))
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(resp)
	})
	logger.Info("rule export/import handlers registered at /admin/rules/{export,import}")
}

// registerChallengerAdminHandler  ML challenger 管理：注册 / 列表 / promote /
// drop。运营在 admin-web 注册新模型 → C-C 框架并行打分（不影响主决策）→
// /admin/mlscore/abtest 看显著性 → promote 或 drop。
//
// 端点：
//
//	GET  /admin/mlscore/challengers
//	     → {champion, challengers: [{name, model_ver}], scoring_disabled}
//
//	POST /admin/mlscore/challengers/register
//	     Body: {name, model_ver, intercept, weights, platt_a, platt_b}
//	     注册一个 LogisticService challenger（基于显式权重；用 cmd/retrain
//	     训出来后填到这）。同名 → 替换。
//
//	POST /admin/mlscore/challengers/promote
//	     Body: {name}
//	     把 challenger 升为 champion；老 champion 降级为 challenger 留观
//
//	POST /admin/mlscore/challengers/drop
//	     Body: {name}
//	     移除 challenger（不再并行打分）
//
// mlSvc 不是 *ChampionChallengerService 时所有写路径返 503（NoopService
// 配置场景；GET 仍然返空状态）。
func registerChallengerAdminHandler(mux *http.ServeMux, mlSvc mlscore.Service, logger *zap.Logger) {
	cc, isCC := mlSvc.(*mlscore.ChampionChallengerService)
	mux.HandleFunc("/admin/mlscore/challengers", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		resp := map[string]any{"scoring_disabled": !isCC}
		if isCC {
			resp["champion"] = cc.ChampionName()
			names := cc.ChallengerNames()
			items := make([]map[string]any, 0, len(names))
			for _, n := range names {
				items = append(items, map[string]any{"name": n})
			}
			resp["challengers"] = items
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/admin/mlscore/challengers/register", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		if !isCC {
			http.Error(w, `{"error":"mlSvc not a champion-challenger; check mlscore.enabled"}`, http.StatusServiceUnavailable)
			return
		}
		var body struct {
			Name     string                  `json:"name"`
			ModelVer string                  `json:"model_ver"`
			Intercept float64                `json:"intercept"`
			Weights  mlscore.FeatureWeights  `json:"weights"`
			PlattA   float64                 `json:"platt_a"`
			PlattB   float64                 `json:"platt_b"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		if body.Name == "" {
			http.Error(w, `{"error":"name required"}`, http.StatusBadRequest)
			return
		}
		if body.Name == cc.ChampionName() {
			http.Error(w, `{"error":"name conflicts with champion; use a different name"}`, http.StatusBadRequest)
			return
		}
		challenger := mlscore.NewLogisticServiceWith(mlscore.LogisticConfig{
			ModelVer: body.ModelVer,
			Intercept: body.Intercept,
			Weights:  body.Weights,
			PlattA:   body.PlattA,
			PlattB:   body.PlattB,
		})
		cc.RegisterChallenger(body.Name, challenger)
		logger.Info("challenger registered", zap.String("name", body.Name), zap.String("model_ver", body.ModelVer))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "name": body.Name})
	})

	mux.HandleFunc("/admin/mlscore/challengers/promote", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		if !isCC {
			http.Error(w, `{"error":"mlSvc not a champion-challenger"}`, http.StatusServiceUnavailable)
			return
		}
		var body struct{ Name string `json:"name"` }
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<13)).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		if body.Name == "" {
			http.Error(w, `{"error":"name required"}`, http.StatusBadRequest)
			return
		}
		oldChamp := cc.ChampionName()
		if !cc.PromoteChallenger(body.Name) {
			http.Error(w, `{"error":"challenger not found"}`, http.StatusNotFound)
			return
		}
		logger.Info("challenger promoted",
			zap.String("new_champion", body.Name), zap.String("demoted", oldChamp))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "champion": body.Name, "demoted": oldChamp,
		})
	})

	mux.HandleFunc("/admin/mlscore/challengers/drop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		if !isCC {
			http.Error(w, `{"error":"mlSvc not a champion-challenger"}`, http.StatusServiceUnavailable)
			return
		}
		var body struct{ Name string `json:"name"` }
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<13)).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		if !cc.RemoveChallenger(body.Name) {
			http.Error(w, `{"error":"challenger not found"}`, http.StatusNotFound)
			return
		}
		logger.Info("challenger dropped", zap.String("name", body.Name))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	logger.Info("challenger admin handlers registered at /admin/mlscore/challengers/{,register,promote,drop}")
}

// registerRuleOverlapHandler  GET /admin/rules/overlap?min_both=N
//
// 算所有规则两两 co-fire 矩阵：哪些规则同时命中？比例多少？verdict 是否
// 冲突？给运营找规则冗余 / 设计冲突用：
//
//   - Jaccard > 0.8  →  规则功能高度重叠，可考虑删一个
//   - A→B = 1.0 (蕴含)  →  A 是 B 的子集，A 可能冗余
//   - IsConflict=true (DENY vs REVIEW 等不一致) → 设计意图冲突
//
// min_both 缺省 5（过滤长尾噪音）。生产应该接 ClickHouse 长期统计。
func registerRuleOverlapHandler(mux *http.ServeMux, sink audit.Sink, logger *zap.Logger) {
	mux.HandleFunc("/admin/rules/overlap", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		minBoth := 5
		if v := r.URL.Query().Get("min_both"); v != "" {
			if n, err := strconvAtoi(v); err == nil && n > 0 {
				minBoth = n
			}
		}
		mem := findMemSink(sink)
		var audits []*audit.DecisionAudit
		if mem != nil {
			audits = mem.Recent(0)
		}
		pairs := ruleoverlap.Compute(audits, minBoth)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"min_both": minBoth,
			"sample":   len(audits),
			"pairs":    pairs,
		})
	})
	logger.Info("rule overlap handler registered at /admin/rules/overlap")
}

// registerMLOverrideHandler  ML 降级开关的 admin 端点：
//
//	GET  /admin/mlscore/override     当前状态 {disabled, force_score, reason, set_at, set_by, is_active}
//	POST /admin/mlscore/override     Body: {disabled, force_score, reason}
//	POST /admin/mlscore/override/clear  清空（恢复正常运行）
//
// 给运营 / SRE 紧急工具：
//   - ML 突然推理质量下降（drift / data poisoning）→ 立即 disabled=true 跳过 ML
//   - 怀疑 ML 误伤合法交易 → 强制 force_score=0
//   - 调试 ml_threshold 规则 → 强制 force_score=0.99 看下游链路
//
// 写入立即生效（atomic.Pointer），不需要重启。
func registerMLOverrideHandler(mux *http.ServeMux, store *mloverride.Store, logger *zap.Logger) {
	mux.HandleFunc("/admin/mlscore/override", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			cur := store.Get()
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"disabled":    cur.Disabled,
				"force_score": cur.ForceScore,
				"reason":      cur.Reason,
				"set_at":      cur.SetAt,
				"set_by":      cur.SetBy,
				"is_active":   cur.IsActive(),
			})
		case http.MethodPost:
			var body struct {
				Disabled   bool    `json:"disabled"`
				ForceScore float64 `json:"force_score"`
				Reason     string  `json:"reason"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&body); err != nil {
				http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
				return
			}
			if body.ForceScore < 0 || body.ForceScore > 1 {
				http.Error(w, `{"error":"force_score must be in [0, 1]"}`, http.StatusBadRequest)
				return
			}
			actor := "unknown"
			if p, ok := auth.PrincipalFrom(r.Context()); ok && p != nil {
				actor = p.KeyID
			}
			store.Set(mloverride.Override{
				Disabled:   body.Disabled,
				ForceScore: body.ForceScore,
				Reason:     body.Reason,
				SetBy:      actor,
			})
			logger.Warn("ml override set",
				zap.Bool("disabled", body.Disabled),
				zap.Float64("force_score", body.ForceScore),
				zap.String("reason", body.Reason),
				zap.String("actor", actor))
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/admin/mlscore/override/clear", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		actor := "unknown"
		if p, ok := auth.PrincipalFrom(r.Context()); ok && p != nil {
			actor = p.KeyID
		}
		store.Clear(actor)
		logger.Info("ml override cleared", zap.String("actor", actor))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "cleared"})
	})
	logger.Info("ml override handlers registered at /admin/mlscore/override{,/clear}")
}

// registerAuditSearchHandler  GET /admin/audit/search
//
// query params (全部可选):
//   merchant_id   按商户过滤
//   customer_id   按客户过滤
//   ip            按 IP 过滤
//   verdict       ALLOW / REVIEW / DENY
//   since         RFC3339 / 秒级 unix
//   until         RFC3339 / 秒级 unix
//   limit         返回上限，缺省 200
//
// 给运营查 "商户 X 最近 24h 的所有 DENY" 这类用例，比 /admin/audit/decisions
// 只能拉最新 N 条更精确。生产 ClickHouse sink 应该走 SQL，本端点是 in-mem
// ring 范围内的 quick search。
func registerAuditSearchHandler(mux *http.ServeMux, sink audit.Sink, logger *zap.Logger) {
	mem := findMemSink(sink)
	mux.HandleFunc("/admin/audit/search", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		if mem == nil {
			http.Error(w, `{"error":"audit MemSink not configured"}`, http.StatusServiceUnavailable)
			return
		}
		q := r.URL.Query()
		f := audit.SearchFilter{
			MerchantID: q.Get("merchant_id"),
			CustomerID: q.Get("customer_id"),
			IPAddress:  q.Get("ip"),
			Verdict:    q.Get("verdict"),
		}
		if v := q.Get("since"); v != "" {
			if t, err := parseTimeFlexible(v); err == nil {
				f.Since = t
			}
		}
		if v := q.Get("until"); v != "" {
			if t, err := parseTimeFlexible(v); err == nil {
				f.Until = t
			}
		}
		if v := q.Get("limit"); v != "" {
			if n, err := strconvAtoi(v); err == nil && n > 0 {
				f.Limit = n
			}
		}
		results := mem.Search(f)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": results,
			"total": len(results),
		})
	})
	logger.Info("audit search handler registered at /admin/audit/search")
}

// parseTimeFlexible 支持 RFC3339 / 秒级 unix timestamp 两种格式。
func parseTimeFlexible(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if n, err := strconvAtoi(s); err == nil {
		return time.Unix(int64(n), 0).UTC(), nil
	}
	return time.Time{}, errors.New("unrecognized time format")
}

// registerAuditChainVerifyHandler  POST /admin/audit/chain/verify
//
// 给合规 / 取证 / 内审用：从 audit MemSink 拉最近 N 条 (按 OccurredAt 升序)
// 跑 audit.VerifyChain，验证 chain_prev_hash / chain_row_hash 没被篡改。
//
// query/body 参数：
//   limit   验证条数 (默认 1000)
//   start_prev  起点 prev_hash (空 = genesis 全零；从某条之后续验时填)
//
// 返回 {ok, verified, failed_at_index, error_field, want, got}。
//
// 注意：本端点用 in-process MemSink 数据。生产 ClickHouse / PG 落库后应该
// 写一个独立 cmd/audit-verify 离线 CLI 走 SQL 流式扫，而不是从服务进程
// 拉 — ring 上限 ~4096 条是 in-mem 验证上限。
func registerAuditChainVerifyHandler(mux *http.ServeMux, sink audit.Sink, logger *zap.Logger) {
	mem := findMemSink(sink)
	mux.HandleFunc("/admin/audit/chain/verify", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		if mem == nil {
			http.Error(w, `{"error":"audit MemSink not configured"}`, http.StatusServiceUnavailable)
			return
		}
		var body struct {
			Limit     int    `json:"limit"`
			StartPrev string `json:"start_prev"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&body)
		if body.Limit <= 0 || body.Limit > 5000 {
			body.Limit = 1000
		}
		// MemSink Recent 是 newest-first；VerifyChain 需要 oldest-first → 反转
		recent := mem.Recent(body.Limit)
		records := make([]*audit.DecisionAudit, len(recent))
		for i, a := range recent {
			records[len(recent)-1-i] = a
		}
		idx, err := audit.VerifyChain(records, body.StartPrev)
		resp := map[string]any{
			"verified": len(records),
			"ok":       err == nil,
		}
		if err != nil {
			resp["failed_at_index"] = idx
			resp["error"] = err.Error()
			if ve, ok := err.(*audit.VerifyError); ok {
				resp["error_field"] = ve.Field
				resp["want"] = ve.Want
				resp["got"] = ve.Got
			}
		}
		logger.Info("audit chain verify",
			zap.Int("verified", len(records)),
			zap.Bool("ok", err == nil))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	logger.Info("audit chain verify handler registered at /admin/audit/chain/verify")
}

// registerWebhookDLQHandler  webhook DLQ admin 端点：
//
//	GET  /admin/webhook/dlq?merchant_id=&status=&limit=  → 列表
//	GET  /admin/webhook/dlq/get?id=                       → 单条
//	POST /admin/webhook/dlq/replay   {event_id}           → 重投递
//	POST /admin/webhook/dlq/discard  {event_id, reason}   → 标已知不可送达
//	POST /admin/webhook/dlq/delete   {event_id}           → 删（清理用）
//
// 给运营 dashboard 用：商户 webhook URL 失败 → 列表筛选 → fix 后 replay。
func registerWebhookDLQHandler(mux *http.ServeMux, wh *webhook.Publisher, logger *zap.Logger) {
	if wh == nil || wh.DLQ() == nil {
		return
	}
	dlq := wh.DLQ()
	mux.HandleFunc("/admin/webhook/dlq", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		merchantID := r.URL.Query().Get("merchant_id")
		status := r.URL.Query().Get("status")
		limit := 100
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconvAtoi(v); err == nil && n > 0 {
				limit = n
			}
		}
		entries := dlq.List(merchantID, status, limit)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": entries,
			"total": len(entries),
		})
	})
	mux.HandleFunc("/admin/webhook/dlq/get", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		id := r.URL.Query().Get("id")
		if id == "" {
			http.Error(w, `{"error":"id required"}`, http.StatusBadRequest)
			return
		}
		e, ok := dlq.Get(id)
		if !ok {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(e)
	})
	mux.HandleFunc("/admin/webhook/dlq/replay", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		var body struct{ EventID string `json:"event_id"` }
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		if err := wh.Replay(body.EventID); err != nil {
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
			return
		}
		logger.Info("webhook DLQ replay", zap.String("event_id", body.EventID))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "queued"})
	})
	mux.HandleFunc("/admin/webhook/dlq/discard", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			EventID string `json:"event_id"`
			Reason  string `json:"reason"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		actor := "unknown"
		if p, ok := auth.PrincipalFrom(r.Context()); ok && p != nil {
			actor = p.KeyID
		}
		_ = dlq.Discard(body.EventID, actor, body.Reason)
		logger.Info("webhook DLQ discarded",
			zap.String("event_id", body.EventID), zap.String("actor", actor))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "discarded"})
	})
	mux.HandleFunc("/admin/webhook/dlq/delete", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		var body struct{ EventID string `json:"event_id"` }
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		_ = dlq.Delete(body.EventID)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
	})
	logger.Info("webhook DLQ admin handlers registered at /admin/webhook/dlq{,/get,/replay,/discard,/delete}")
}

// splitCommaList "a:9000,b:9000" → ["a:9000","b:9000"]，trim 空格。
func splitCommaList(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// registerWhoamiHandler GET /admin/whoami → 当前 admin token 的 role + KeyID。
// 给 admin-web header 显示 "你以 danger 角色登录"；前端按 role 隐藏 danger
// 按钮（PII delete / rule danger 操作 / ML override / audit chain verify 等）。
func registerWhoamiHandler(mux *http.ServeMux, logger *zap.Logger) {
	mux.HandleFunc("/admin/whoami", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		resp := map[string]any{
			"authenticated": false,
			"role":          "",
			"key_id":        "",
		}
		if p, ok := auth.PrincipalFrom(r.Context()); ok && p != nil {
			resp["authenticated"] = true
			resp["role"] = p.AdminRole
			resp["key_id"] = p.KeyID
			if resp["role"] == "" {
				// AdminAuth 没注 role 时 (legacy AdminAuth(map[string]struct{}))
				// HasRole 默认 danger；前端按 danger 显示
				resp["role"] = "danger"
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
}
