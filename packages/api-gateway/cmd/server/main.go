// Command server 启动 api-gateway HTTP 网关。
//
// fx 装配顺序：config → logger → otel → metrics → server.NewServer →
// 启动公网 + admin + metrics 端口。优雅关停时先 drain 公网 server，再关
// admin / metrics。
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"
	"github.com/xiongwp/payment-util/configcenter"
	"github.com/xiongwp/payment-util/kitexutil"
	"github.com/xiongwp/payment-util/trace"
	"go.uber.org/fx"
	"go.uber.org/zap"

	"github.com/xiongwp/user-merchant-core/kitex_gen/usermerchant/v1/userservice"

	"github.com/xiongwp/api-gateway/internal/metrics"
	"github.com/xiongwp/api-gateway/internal/server"
	"github.com/xiongwp/api-gateway/internal/userweb"
)

func main() {
	metrics.Register()

	otelShutdown, otelErr := trace.InitOTel(context.Background(), "api-gateway", os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
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
			newConfigCenterClient,
			newRateLimitHub,
			newServerConfig,
			newUserwebHandler,
			newCardHandler,
			newServer,
		),
		fx.Invoke(bindRateLimitToConfigCenter, startServer, startMetrics),
	)
	app.Run()
}

// newConfigCenterClient 启动期同步连 config-center；prod 不可达 fail-fast。
//
// **替代**：旧版 rate_limit.* / cors / cookie 等动态配置块从 yaml 读；
// 现在 yaml 只留 bootstrap 默认（启动期 config-center 不可达兜底）。
func newConfigCenterClient(v *viper.Viper, logger *zap.Logger) (*configcenter.Client, error) {
	endpoint := v.GetString("configcenter.endpoint")
	if endpoint == "" {
		endpoint = "http://config-center:9691"
	}
	namespace := v.GetString("configcenter.namespace")
	if namespace == "" {
		namespace = "api-gateway"
	}
	instanceID := v.GetString("configcenter.instance_id")
	if instanceID == "" {
		instanceID, _ = os.Hostname()
	}
	rpc := configcenter.NewHTTPClient(endpoint, nil)
	cli, err := configcenter.NewWithRPC(rpc, configcenter.Config{
		Namespace:        namespace,
		InstanceID:       instanceID,
		ReconnectBackoff: 1 * time.Second,
		InitTimeout:      10 * time.Second,
		Logger:           logger,
	})
	if err != nil {
		env := strings.ToLower(strings.TrimSpace(v.GetString("env")))
		if env == "prod" || env == "production" {
			return nil, fmt.Errorf("config-center init failed (prod fail-fast): %w", err)
		}
		logger.Warn("config-center init failed; dev mode degrades to yaml bootstrap defaults", zap.Error(err))
		return nil, nil
	}
	logger.Info("config-center connected",
		zap.String("endpoint", endpoint),
		zap.String("namespace", namespace),
		zap.String("instance_id", instanceID))
	return cli, nil
}

// newRateLimitHub 用 hardcoded safe default 起 hub；config-center 启动期立刻覆盖。
//
// yaml 不再含 rate_limit 块；100% 由 config-center namespace=api-gateway，
// key=rate_limit JSON 推送：
//   {"ip_rps":100,"ip_burst":200,"merchant_rps":1000,"merchant_burst":2000}
//
// hardcoded default 仅在 dev / CI 且 config-center 不可达时兜底。
func newRateLimitHub(logger *zap.Logger) *server.RateLimitHub {
	return server.NewRateLimitHub(server.RateLimitParams{
		IPRPS:         100,
		IPBurst:       200,
		MerchantRPS:   1000,
		MerchantBurst: 2000,
	}, logger)
}

// bindRateLimitToConfigCenter 启动期 + watch 推送时把 config-center 的
// "rate_limit" JSON key 解到 RateLimitParams 并热更新 hub。
//
// config-center 推荐 key（namespace=api-gateway）：
//
//	rate_limit  → {"ip_rps":100,"ip_burst":200,"merchant_rps":1000,"merchant_burst":2000}
//
// 改这一个 JSON key，集群所有 api-gateway 副本秒级同步。
func bindRateLimitToConfigCenter(cli *configcenter.Client, hub *server.RateLimitHub, logger *zap.Logger) error {
	if cli == nil {
		// dev 没接 config-center；hub 用 hardcoded safe default 跑
		return nil
	}
	// 立即读一次（如果有）
	var p server.RateLimitParams
	if err := configcenter.GetJSON(cli, context.Background(), "rate_limit", &p); err == nil {
		hub.ApplyParams(p)
	} else {
		logger.Info("config-center rate_limit key absent; using hardcoded safe default")
	}
	// 注册 OnChange 回调：admin PUT /api/v1/configs/api-gateway/rate_limit 后秒级触发
	cli.OnChange("rate_limit", func(v *configcenter.ConfigValue) {
		if v == nil || v.Value == "" {
			return
		}
		var np server.RateLimitParams
		if err := configcenter.GetJSON(cli, context.Background(), "rate_limit", &np); err != nil {
			logger.Warn("rate_limit reload decode failed", zap.Error(err))
			return
		}
		hub.ApplyParams(np)
	})
	return nil
}

// loadConfig 读 yaml + 环境变量。环境变量前缀 APIGW_，例如 APIGW_SERVER_HTTP_PORT=8080。
func loadConfig() (*viper.Viper, error) {
	v := viper.New()
	v.SetEnvPrefix("APIGW")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	// AutomaticEnv 仅自动绑定已在 yaml 出现的 key；BindEnv 兜底保证 endpoint /
	// registry.endpoints 能被 APIGW_<KEY> env 读到。
	for _, k := range []string{
		"user_merchant.endpoint", "order_core.endpoint", "risk.endpoint", "kms.endpoint",
		"registry.endpoints", "env",
		// 卡支付相关，env 优先级覆盖 yaml
		"cards.enabled",
		"cards.user_card_service_endpoint",
		"cards.payment_service_endpoint",
		// 浏览器端 SDK / form JS 直发 card-center 的公网 URL（PAN 单跳）
		"cards.card_center_url",
	} {
		_ = v.BindEnv(k)
	}
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath("./config")
	v.AddConfigPath(".")
	v.AddConfigPath("/etc/api-gateway")
	if err := v.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if !errors.As(err, &notFound) {
			return nil, err
		}
	}
	if err := assertProdSafety(v); err != nil {
		return nil, err
	}
	return v, nil
}

// assertProdSafety env=prod 下的 fail-fast 安全校验。
//
// api-gateway 是公网边界，要求最严：
//  1. admin.token 必须配（管理面接口必须鉴权）
//  2. 不允许 X-Shadow header 不限来源（trusted_cidrs 必填）
//  3. user_merchant.endpoint 必须配
//  4. tls 必须开（外部入口走 HTTPS）
func assertProdSafety(v *viper.Viper) error {
	env := strings.ToLower(strings.TrimSpace(v.GetString("env")))
	if env != "prod" && env != "production" {
		return nil
	}
	if strings.TrimSpace(v.GetString("admin.token")) == "" && os.Getenv("ADMIN_HTTP_TOKEN") == "" {
		return fmt.Errorf("PROD-SAFETY: admin.token (or ADMIN_HTTP_TOKEN env) must be set in env=prod")
	}
	if strings.TrimSpace(v.GetString("user_merchant.endpoint")) == "" && len(v.GetStringSlice("registry.endpoints")) == 0 {
		return fmt.Errorf("PROD-SAFETY: user_merchant.endpoint must be configured in env=prod")
	}
	// shadow header 安全：必须配 trusted_cidrs 或 trusted_header，否则任何外部
	// IP 都能伪造压测流量进生产。
	if len(v.GetStringSlice("shadow.trusted_cidrs")) == 0 && strings.TrimSpace(v.GetString("shadow.trusted_header")) == "" {
		return fmt.Errorf("PROD-SAFETY: shadow.trusted_cidrs or shadow.trusted_header must be configured in env=prod (otherwise any client can inject X-Shadow)")
	}
	// rate limit 已 100% 迁到 config-center；yaml 不再保留 rate_limit 块。
	// 启动期 SDK 拉一次 namespace=api-gateway, key=rate_limit；admin 改后秒级热更新。
	if strings.TrimSpace(v.GetString("configcenter.endpoint")) == "" {
		return fmt.Errorf("PROD-SAFETY: configcenter.endpoint must be configured in env=prod (rate_limit 等动态配置强依赖)")
	}
	// 卡支付路径：env=prod 下卡服务 endpoint 必须显式开 / 关。开了就不能用 stub。
	// 没显式开（cards.enabled 缺省 false）→ 跳过，CardHandler 会用 stub 直接报错（不会泄漏 PAN）。
	if v.GetBool("cards.enabled") {
		if strings.TrimSpace(v.GetString("cards.user_card_service_endpoint")) == "" {
			return fmt.Errorf("PROD-SAFETY: cards.enabled=true requires cards.user_card_service_endpoint")
		}
		if strings.TrimSpace(v.GetString("cards.payment_service_endpoint")) == "" {
			return fmt.Errorf("PROD-SAFETY: cards.enabled=true requires cards.payment_service_endpoint")
		}
	}
	return nil
}

func newLogger() (*zap.Logger, error) {
	if os.Getenv("APIGW_LOG_DEV") == "true" {
		return zap.NewDevelopment()
	}
	return zap.NewProduction()
}

// newServerConfig 把 viper 字段铺平成 server.Config。优先读 ADMIN_HTTP_TOKEN env。
func newServerConfig(v *viper.Viper, hub *server.RateLimitHub) server.Config {
	adminToken := os.Getenv("ADMIN_HTTP_TOKEN")
	if adminToken == "" {
		adminToken = v.GetString("admin.token")
	}
	return server.Config{
		HTTPPort:          getIntDefault(v, "server.http_port", 8080),
		AdminPort:         getIntDefault(v, "admin.port", 8081),
		MaxRequestBytes:   v.GetInt64("server.max_request_bytes"),
		ReadHeaderTimeout: getDurDefault(v, "server.read_header_timeout", 5*time.Second),
		ReadTimeout:       getDurDefault(v, "server.read_timeout", 10*time.Second),
		WriteTimeout:      getDurDefault(v, "server.write_timeout", 30*time.Second),
		IdleTimeout:       getDurDefault(v, "server.idle_timeout", 60*time.Second),

		AuthEnabled: v.GetBool("auth.enabled"),
		// auth.api_keys 是 yaml map：apikey → merchant_id（server-trusted）
		// merchant_id 留空则 per-merchant 限流退化到 per-IP（dev 兼容）。
		AuthAPIKeys: v.GetStringMapString("auth.api_keys"),

		// rate_limit 静态字段已迁到 RateLimitHub（绑 config-center）。
		RateLimitHub: hub,

		AdminToken: adminToken,

		// shadow 流量边界（防 X-Shadow header 外部伪造）。
		// 默认全空 = 所有外部 X-Shadow 都被 strip。
		// 内网压测平台 / IDC 内部要透传必须配下面任一字段。
		// yaml: shadow.trusted_cidrs / shadow.trusted_header_name / shadow.trusted_header_value
		ShadowTrustedCIDRs:       v.GetStringSlice("shadow.trusted_cidrs"),
		ShadowTrustedHeaderName:  v.GetString("shadow.trusted_header_name"),
		ShadowTrustedHeaderValue: v.GetString("shadow.trusted_header_value"),
	}
}

func newServer(cfg server.Config, uw *userweb.Handler, ch *userweb.CardHandler, logger *zap.Logger) *server.Server {
	var regs []server.MuxRegister
	if uw != nil {
		regs = append(regs, uw.Register)
	}
	if ch != nil {
		regs = append(regs, ch.Register)
	}
	return server.NewServer(cfg, logger, regs...)
}

// newCardHandler 装配卡支付 handler。
//
// 接通逻辑：
//   - cards.enabled=true + user_merchant.endpoint 已配 → 真 gRPC client
//     (NewGRPCCardClient → user-merchant-core.UserCardService.Attach/List/Delete/SetDefault)
//   - 其它情况 → stub client（页面正常渲染，操作返 not-wired）
//
// PaymentServiceClient 现仍是 stub（task: order-core PaymentIntentService 加 user_card_id
// 字段后接通真实 client）。
//
// PCI nano-discipline：cards_new.html 是 JS-only 直发 card-center HTTPS，
// PAN 永远不经过 api-gateway。本 handler 的 /cards/attach 只接 stored_token + masked。
func newCardHandler(uw *userweb.Handler, v *viper.Viper, logger *zap.Logger) *userweb.CardHandler {
	if uw == nil {
		return nil
	}
	// gRPC card / payment clients 已切 Kitex — 真接通需要 Kitex client
	// (userservice / paymentintentservice). 此处先走 stub, real wiring 是
	// 后续 KX-WIDE 收尾的活儿.
	cardClient := userweb.NewStubCardClient()
	payClient := userweb.NewStubPaymentClient()
	logger.Info("CardHandler: stub mode (Kitex card/payment client wiring pending)")

	ch := userweb.NewCardHandler(uw, cardClient, payClient)
	ch.CardCenterURL = v.GetString("cards.card_center_url")
	return ch
}

// newUserwebHandler 构造 userweb.Handler — 直接走 Kitex userservice client
// (etcd resolver / endpoint 由 kitex client 自己解决, 这里不再 dial *grpc.ClientConn).
func newUserwebHandler(v *viper.Viper, logger *zap.Logger) (*userweb.Handler, error) {
	if v.GetString("user_merchant.endpoint") == "" && len(v.GetStringSlice("registry.endpoints")) == 0 {
		logger.Warn("user_merchant.endpoint and registry.endpoints both unset; signup/login pages will fail")
		return nil, nil
	}
	h, err := userweb.NewHandler(kitexutil.MustKitexClient(userservice.NewClient("user-merchant-core",
		kitexutil.DefaultClientOptions("user-merchant-core")...,
	)), logger)
	if err != nil {
		return nil, err
	}
	h.CookieDomain = v.GetString("auth.cookie_domain")
	h.CookieSecure = v.GetBool("auth.cookie_secure")
	h.CookieSameSiteNone = v.GetBool("auth.cookie_samesite_none")
	return h, nil
}

// startServer wires fx lifecycle hooks for the HTTP servers.
//
// 关停顺序：
//  1. BeginDrain 让 /readyz 立刻 503 → K8s 摘流量
//  2. 给 K8s 一个 readiness 检查周期（默认 ~5s）让流量切走
//  3. srv.Stop 真正关 HTTP server（带 ctx timeout 优雅 drain in-flight）
func startServer(lc fx.Lifecycle, srv *server.Server, logger *zap.Logger) {
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			return srv.Start()
		},
		OnStop: func(ctx context.Context) error {
			logger.Info("api-gateway draining: /readyz now returns 503")
			server.BeginDrain()
			// 留窗口让 K8s readiness probe 看见 503 + LB 摘流量；时长应 ≥
			// readinessProbe.periodSeconds 默认 10s。生产化时改成 config。
			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
			}
			logger.Info("api-gateway shutting down http servers")
			return srv.Stop(ctx)
		},
	})
}

// startMetrics 起 Prometheus 端口。fx 不接管 shutdown 因为 process 退出时
// 内核会回收 listener；metrics scrape 失败一两次可接受。
func startMetrics(lc fx.Lifecycle, v *viper.Viper, logger *zap.Logger) {
	port := getIntDefault(v, "metrics.port", 9090)
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			metrics.Serve(port, logger)
			return nil
		},
	})
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func getIntDefault(v *viper.Viper, key string, def int) int {
	if v.IsSet(key) {
		return v.GetInt(key)
	}
	return def
}

func getDurDefault(v *viper.Viper, key string, def time.Duration) time.Duration {
	if v.IsSet(key) {
		return v.GetDuration(key)
	}
	return def
}
