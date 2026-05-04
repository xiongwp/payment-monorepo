// Command server 启动 payment-core gRPC 服务。
package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"
	"github.com/xiongwp/payment-util/trace"
	"go.uber.org/fx"
	"go.uber.org/zap"

	"github.com/xiongwp/payment-core/internal/channelclient"
	"github.com/xiongwp/payment-core/internal/kmsclient"
	"github.com/xiongwp/payment-core/internal/metrics"
	"github.com/xiongwp/payment-core/internal/riskclient"
	"github.com/xiongwp/payment-core/internal/routing"
	"github.com/xiongwp/payment-core/internal/secret"
	"github.com/xiongwp/payment-core/internal/server"
	"github.com/xiongwp/payment-core/internal/service"
)

func main() {
	metrics.Register()
	otelShutdown, otelErr := trace.InitOTel(context.Background(), "payment-core", os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
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
			newKMSClient,
			newRiskClient,
			newRouter,
			newChannelClient,
			newPaymentSvc,
			newWebhookSvc,
			newServer,
		),
		fx.Invoke(startGRPC, startMetricsHTTP, startAdminHTTP, startServiceRegistrar),
	)
	app.Run()
}

func loadConfig() (*viper.Viper, error) {
	v := viper.New()
	v.SetEnvPrefix("PAYCORE")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	// AutomaticEnv 仅自动绑定已在 yaml 出现的 key；下面这些 key 在 base config.yaml
	// 里可能没列，BindEnv 兜底确保 PAYCORE_<KEY> env 能读到。
	for _, k := range []string{
		"channel.endpoint", "risk.endpoint", "kms.endpoint",
		"registry.endpoints",
	} {
		_ = v.BindEnv(k)
	}
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath("./config")
	v.AddConfigPath(".")
	v.AddConfigPath("/etc/payment-core")
	if err := v.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if !errors.As(err, &notFound) {
			return nil, err
		}
	}
	return v, nil
}

func newLogger() (*zap.Logger, error) {
	if os.Getenv("PAYCORE_LOG_DEV") == "true" {
		return zap.NewDevelopment()
	}
	return zap.NewProduction()
}

func newRouter(v *viper.Viper, logger *zap.Logger) (*routing.Router, error) {
	rules, err := routing.RuleLoader(v)
	if err != nil {
		return nil, err
	}
	if len(rules) == 0 {
		logger.Warn("no routing rules configured; every charge will 404")
	}
	r := routing.NewRouter(rules)
	logger.Info("routing rules loaded", zap.Int("count", len(rules)))
	return r, nil
}

func newChannelClient(v *viper.Viper, logger *zap.Logger) (channelclient.Client, error) {
	endpoint := v.GetString("channel.endpoint")
	registry := v.GetStringSlice("registry.endpoints")
	if endpoint == "" && len(registry) == 0 {
		return nil, fmt.Errorf("channel.endpoint or registry.endpoints is required")
	}
	cli, err := channelclient.Dial(registry, endpoint, v.GetDuration("channel.rpc_timeout"))
	if err != nil {
		return nil, err
	}
	logger.Info("payment-channel client ready",
		zap.String("endpoint", endpoint), zap.Strings("registry", registry))
	return cli, nil
}

func newRiskClient(v *viper.Viper, logger *zap.Logger) (riskclient.Client, error) {
	endpoint := v.GetString("risk.endpoint")
	registry := v.GetStringSlice("registry.endpoints")
	if endpoint == "" && len(registry) == 0 {
		logger.Info("risk.endpoint and registry.endpoints both unset; risk screening disabled (NoopClient, all-allow)")
		return riskclient.NoopClient{}, nil
	}
	cli, err := riskclient.Dial(registry, endpoint, v.GetDuration("risk.rpc_timeout"))
	if err != nil {
		return nil, err
	}
	logger.Info("risk-manage client ready",
		zap.String("endpoint", endpoint), zap.Strings("registry", registry))
	return cli, nil
}

func newPaymentSvc(r *routing.Router, c channelclient.Client, risk riskclient.Client, v *viper.Viper, logger *zap.Logger) *service.PaymentService {
	svc := service.NewPaymentService(r, c, risk, logger)
	if v.GetBool("risk.fail_close") {
		svc.SetRiskFailClose(true)
		logger.Info("risk fail-close mode ENABLED — risk outage will block all payments")
	}

	// 默认风控超时（兜底）。risk.rpc_timeout 是 client dial / unary 整体超时；
	// 这里再加 per-call 上限，避免某次调用拖慢整个 Charge 路径。
	if d := v.GetDuration("risk.default_timeout"); d > 0 {
		svc.SetRiskTimeoutDefault(d)
		logger.Info("risk default timeout set", zap.Duration("timeout", d))
	}

	// risk.merchant_timeout: { "<merchant_id>": "500ms", ... }
	// 头部商户走 500ms 等小窗口快速决策，长尾商户用 default。
	mtRaw := v.GetStringMapString("risk.merchant_timeout")
	if len(mtRaw) > 0 {
		mt := make(map[string]time.Duration, len(mtRaw))
		for mid, val := range mtRaw {
			d, err := time.ParseDuration(val)
			if err != nil {
				logger.Warn("invalid merchant_timeout value, skipped",
					zap.String("merchant_id", mid),
					zap.String("value", val),
					zap.Error(err))
				continue
			}
			mt[mid] = d
		}
		svc.SetRiskTimeoutByMerchant(mt)
		logger.Info("risk per-merchant timeout map loaded",
			zap.Int("merchants", len(mt)))
	}

	// risk.review_step_up: bool — verdict=REVIEW 时是否对支持 3DS 的渠道发起 step-up
	// risk.review_step_up_methods: ["CARD", ...] — 哪些 payment_method 走 step-up
	if v.GetBool("risk.review_step_up") {
		methods := v.GetStringSlice("risk.review_step_up_methods")
		svc.SetRiskReviewStepUp(true, methods)
		logger.Info("risk review step-up ENABLED",
			zap.Strings("methods", methods))
	}

	return svc
}

func newWebhookSvc(logger *zap.Logger) *service.WebhookService {
	return service.NewWebhookService(logger)
}

func newKMSClient(v *viper.Viper, logger *zap.Logger) (kmsclient.Client, error) {
	endpoint := v.GetString("kms.endpoint")
	registry := v.GetStringSlice("registry.endpoints")
	if endpoint == "" && len(registry) == 0 {
		logger.Info("kms.endpoint and registry.endpoints both unset; secret passthrough enabled (NoopClient)")
		return kmsclient.NoopClient{}, nil
	}
	cli, err := kmsclient.Dial(registry, endpoint, v.GetString("kms.bearer_token"), v.GetDuration("kms.rpc_timeout"))
	if err != nil {
		return nil, err
	}
	logger.Info("kms-manage client ready",
		zap.String("endpoint", endpoint), zap.Strings("registry", registry))
	return cli, nil
}

func newServer(svc *service.PaymentService, wh *service.WebhookService, kms kmsclient.Client, v *viper.Viper, logger *zap.Logger) (*server.Server, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tokens, err := secret.ResolveStringSlice(ctx, kms, "svc:paycore:auth_tokens", v.GetStringSlice("auth.tokens"))
	if err != nil {
		return nil, fmt.Errorf("resolve auth tokens: %w", err)
	}
	tokenSet := map[string]string{}
	for _, t := range tokens {
		tokenSet[t] = "ok"
	}
	return server.NewServer(server.Deps{
		PaymentSvc:   svc,
		WebhookSvc:   wh,
		AuthTokens:   tokenSet,
		RateLimitRPS: v.GetFloat64("rate_limit.rps"),
		RateBurst:    v.GetInt("rate_limit.burst"),
		Logger:       logger,
	}), nil
}

func startGRPC(lc fx.Lifecycle, s *server.Server, v *viper.Viper, logger *zap.Logger) {
	port := v.GetInt("server.grpc_port")
	if port == 0 {
		port = 9090
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

func startMetricsHTTP(lc fx.Lifecycle, v *viper.Viper, logger *zap.Logger, svc *service.PaymentService) {
	addr := v.GetString("metrics.addr")
	if addr == "" {
		addr = ":9190"
	}
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			metrics.StartServer(addr, logger, svc.Breakers())
			return nil
		},
		OnStop: func(_ context.Context) error {
			metrics.BeginDrain()
			logger.Info("payment-core draining: /readyz now returns 503")
			return nil
		},
	})
}

func startAdminHTTP(lc fx.Lifecycle, v *viper.Viper, router *routing.Router, logger *zap.Logger) {
	addr := v.GetString("admin_http.addr")
	if addr == "" {
		addr = ":9290"
	}
	handler := routing.AdminHandler(router, v, logger)
	// 资安/系统安全：admin_http.token 非空时强制 X-Admin-Token 头匹配；缺失或不
	// 匹配返 401。空 token 仅用于 dev / 单测，生产部署 yaml 必须配（main 启动时
	// 会 Warn 提醒）。
	//
	// 历史背景：AdminHandler 的注释说"鉴权由 caller wrap middleware 注入"，
	// 但 cmd/server 之前直接挂 handler 没 wrap → /admin/routing/reload 在内网
	// 任何节点都能触发，攻击者可以热替换 routing 规则把 GCASH 流量打到
	// attacker-controlled adapter，或重复 reload 当 DoS。
	if tok := v.GetString("admin_http.token"); tok != "" {
		handler = adminTokenMiddleware(handler, tok, logger)
	} else {
		logger.Warn("admin_http.token not set; /admin/routing/* is UNAUTHENTICATED",
			zap.String("addr", addr),
			zap.String("hint", "set admin_http.token in yaml or PAYMENTCORE_ADMIN_HTTP_TOKEN env"))
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			go func() {
				logger.Info("admin http listening", zap.String("addr", addr))
				if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					logger.Error("admin http server exited", zap.Error(err))
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error { return srv.Shutdown(ctx) },
	})
}

// adminTokenMiddleware 保护 /admin/* 端点：要求 X-Admin-Token header 与配置
// token 严格相等。constant-time 比较避免 timing oracle。
//
// 401 响应不区分 "missing token" / "wrong token"，避免给攻击者反馈是否
// 配了 token / 是否猜对前缀。
func adminTokenMiddleware(next http.Handler, expected string, logger *zap.Logger) http.Handler {
	expectedBytes := []byte(expected)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("X-Admin-Token")
		// constant-time compare; 长度不一致也走 fixed-length 路径再返 401。
		if subtle.ConstantTimeCompare([]byte(got), expectedBytes) != 1 {
			logger.Warn("admin http: unauthorized request",
				zap.String("path", r.URL.Path),
				zap.String("remote", r.RemoteAddr),
				zap.String("method", r.Method))
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
