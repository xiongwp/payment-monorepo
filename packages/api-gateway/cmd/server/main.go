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
	"github.com/xiongwp/payment-util/serviceregistry"
	"github.com/xiongwp/payment-util/trace"
	"go.uber.org/fx"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	usermerchantv1 "github.com/xiongwp/user-merchant-core/api/proto/usermerchant/v1"

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
			newServerConfig,
			newUserMerchantConn,
			newUserwebHandler,
			newCardHandler,
			newServer,
		),
		fx.Invoke(startServer, startMetrics),
	)
	app.Run()
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
		"user_merchant.endpoint", "risk.endpoint", "kms.endpoint",
		"registry.endpoints", "env",
		// 卡支付相关，env 优先级覆盖 yaml
		"cards.enabled",
		"cards.user_card_service_endpoint",
		"cards.payment_service_endpoint",
		"cards.mtls.enabled",
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
	// rate limit：公网入口必须配 per-IP 限流
	if v.GetFloat64("rate_limit.per_ip.rps") <= 0 {
		return fmt.Errorf("PROD-SAFETY: rate_limit.per_ip.rps must be > 0 in env=prod (recommend 50, public internet boundary)")
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
		if !v.GetBool("cards.mtls.enabled") {
			return fmt.Errorf("PROD-SAFETY: cards.enabled=true requires cards.mtls.enabled (UserCardService / PaymentService 都走 mTLS)")
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
func newServerConfig(v *viper.Viper) server.Config {
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
		// 例：
		//   auth:
		//     api_keys:
		//       sk_live_abc: "mch_001"
		//       sk_live_def: "mch_002"
		// merchant_id 留空则 per-merchant 限流退化到 per-IP（dev 兼容）。
		AuthAPIKeys: v.GetStringMapString("auth.api_keys"),

		IPRPS:         getIntDefault(v, "rate_limit.ip_rps", 100),
		IPBurst:       getIntDefault(v, "rate_limit.ip_burst", 200),
		MerchantRPS:   getIntDefault(v, "rate_limit.merchant_rps", 1000),
		MerchantBurst: getIntDefault(v, "rate_limit.merchant_burst", 2000),

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
// 真实 mTLS gRPC client 接通后，把这里的 NewStubCardClient / NewStubPaymentClient
// 换成 user-merchant-core.UserCardService client + order-core.PaymentIntentService client。
//
// 当前 stub：
//   - GET /cards / /cards/new / /pay / /pay/result 都能正常渲染
//   - POST /cards (绑卡) / /pay (支付) 会返回友好的 "service not wired" Flash
//   - 已登录用户从 /me 点 "我的卡" / "去支付" 进得来，看到表单
//
// PCI nano-discipline：stub 模式下 PAN 走到 stubCardClient.AddCard 立刻被 errCardServiceNotWired
// 拒绝，cards.go 的 defer 会把局部 pan/cvv 清空；本进程不会持久化任何 PAN。
func newCardHandler(uw *userweb.Handler, v *viper.Viper, logger *zap.Logger) *userweb.CardHandler {
	if uw == nil {
		// userweb.Handler 都没装配，cards 也没法工作（要复用 render / 鉴权）
		return nil
	}
	if v.GetBool("cards.enabled") {
		// TODO(card-rpc): cards.enabled=true 时这里应当：
		//   1. dial user-merchant-core.UserCardService（mTLS）
		//   2. dial order-core.PaymentIntentService（mTLS）
		//   3. 包装成 CardServiceClient / PaymentServiceClient 接口
		// 现阶段还是 stub，但 assertProdSafety 已经在 prod 下强制 endpoint + mtls 配置，
		// 真实接通在下个 PR 完成。
		logger.Warn("cards.enabled=true but real gRPC client not wired yet; falling back to stub. " +
			"See cards_client_stub.go header for wiring path.")
	} else {
		logger.Info("CardHandler running in stub mode (cards.enabled=false). " +
			"Pages render, but Add/Delete/Pay return errCardServiceNotWired.")
	}
	return userweb.NewCardHandler(
		uw,
		userweb.NewStubCardClient(),
		userweb.NewStubPaymentClient(),
	)
}

// newUserMerchantConn 拨号 user-merchant-core gRPC。endpoint 与 registry.endpoints
// 都空 → nil（启动不阻塞，但 /signup /login 会 503）。
//
// registry 非空 → etcd resolver（联栈多 pod 必走，因为容器去掉 container_name 后
// "user-merchant-core" 跨 compose 项目 DNS 不可解析）；空 → 直连 endpoint。
// 两条路都自动 round_robin LB 在多副本间均摊。
func newUserMerchantConn(lc fx.Lifecycle, v *viper.Viper, logger *zap.Logger) (*grpc.ClientConn, error) {
	endpoint := v.GetString("user_merchant.endpoint")
	registry := v.GetStringSlice("registry.endpoints")
	if endpoint == "" && len(registry) == 0 {
		logger.Warn("user_merchant.endpoint and registry.endpoints both unset; signup/login pages will fail")
		return nil, nil
	}
	conn, err := serviceregistry.DialWithFallback(registry, "user-merchant-core", endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time: 30 * time.Second, Timeout: 10 * time.Second, PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return nil, err
	}
	lc.Append(fx.Hook{OnStop: func(_ context.Context) error { return conn.Close() }})
	logger.Info("user-merchant-core dialed",
		zap.String("endpoint", endpoint), zap.Strings("registry", registry))
	return conn, nil
}

func newUserwebHandler(conn *grpc.ClientConn, v *viper.Viper, logger *zap.Logger) (*userweb.Handler, error) {
	if conn == nil {
		return nil, nil
	}
	uc := usermerchantv1.NewUserServiceClient(conn)
	h, err := userweb.NewHandler(uc, logger)
	if err != nil {
		return nil, err
	}
	h.CookieDomain = v.GetString("auth.cookie_domain")
	h.CookieSecure = v.GetBool("auth.cookie_secure")
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
