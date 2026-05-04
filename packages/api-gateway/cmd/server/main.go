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
		"registry.endpoints",
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
	return v, nil
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

func newServer(cfg server.Config, uw *userweb.Handler, logger *zap.Logger) *server.Server {
	var regs []server.MuxRegister
	if uw != nil {
		regs = append(regs, uw.Register)
	}
	return server.NewServer(cfg, logger, regs...)
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
