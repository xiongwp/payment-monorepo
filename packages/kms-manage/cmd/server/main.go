// Command server 启动 kms-manage gRPC 服务。
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
	"github.com/xiongwp/payment-util/trace"
	"go.uber.org/fx"
	"go.uber.org/zap"

	"github.com/xiongwp/kms-manage/internal/keystore"
	"github.com/xiongwp/kms-manage/internal/metrics"
	"github.com/xiongwp/kms-manage/internal/server"
	"github.com/xiongwp/kms-manage/internal/service"
)

func main() {
	metrics.Register()

	// OTel 初始化：OTEL_EXPORTER_OTLP_ENDPOINT 空 → no-op；非空 → 走 OTLP gRPC
	// 推 span 到 collector（Jaeger / Tempo / Grafana Agent）。
	otelShutdown, otelErr := trace.InitOTel(context.Background(), "kms-manage", os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
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
			// config-center: rate_limit / auth tokens / SAN whitelist (敏感，建议 TARGETED 推)
			configcenter.FxProvider("kms-manage"),
			newKeystore,
			newKMSSvc,
			newServer,
		),
		fx.Invoke(startGRPC, startMetricsHTTP, startServiceRegistrar),
	)
	app.Run()
}

func loadConfig() (*viper.Viper, error) {
	v := viper.New()
	v.SetEnvPrefix("KMS")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	_ = v.BindEnv("env")
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath("./config")
	v.AddConfigPath(".")
	v.AddConfigPath("/etc/kms-manage")
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

// assertProdSafety KMS 是密钥服务，prod 安全要求最严：
//  1. auth.allow_unauthenticated 必须 false（任何人能解密 = 全平台密钥泄漏）
//  2. auth.tokens 必须配（mesh 内服务凭 token 调本服务）
//  3. keystore.dir 必须明确（默认 /var/lib/kms-manage/keys 但生产应显式声明）
//  4. **mTLS 三件齐全**（P0-4）：tls.server_cert / tls.server_key / tls.client_ca
//     不齐全 = 退化单向 TLS = 谁都能拿 PAN 加解密，PCI Req 4 / Req 8 不达标。
//  5. **Client identity 白名单非空**（P0-4）：auth.allowed_client_ids 至少 1 项，
//     防止 CA 误签的 cert 也能调 KMS。
func assertProdSafety(v *viper.Viper) error {
	env := strings.ToLower(strings.TrimSpace(v.GetString("env")))
	if env != "prod" && env != "production" {
		return nil
	}
	if v.GetBool("auth.allow_unauthenticated") {
		return fmt.Errorf("PROD-SAFETY: KMS auth.allow_unauthenticated=true is forbidden in env=prod (anyone could decrypt all merchant secrets)")
	}
	if len(v.GetStringSlice("auth.tokens")) == 0 {
		return fmt.Errorf("PROD-SAFETY: KMS auth.tokens must be configured in env=prod (mesh services authenticate with tokens)")
	}
	if strings.TrimSpace(v.GetString("keystore.dir")) == "" {
		return fmt.Errorf("PROD-SAFETY: KMS keystore.dir must be explicitly configured in env=prod (don't rely on default path)")
	}
	for _, k := range []string{"tls.server_cert", "tls.server_key", "tls.client_ca"} {
		if strings.TrimSpace(v.GetString(k)) == "" {
			return fmt.Errorf("PROD-SAFETY: KMS %s required in env=prod (mTLS mandatory; P0-4)", k)
		}
	}
	if len(v.GetStringSlice("auth.allowed_client_ids")) == 0 {
		return fmt.Errorf("PROD-SAFETY: KMS auth.allowed_client_ids must be non-empty in env=prod (at least one CN/SAN whitelisted; P0-4)")
	}
	return nil
}

func newLogger() (*zap.Logger, error) {
	if os.Getenv("KMS_LOG_DEV") == "true" {
		return zap.NewDevelopment()
	}
	return zap.NewProduction()
}

func newKeystore(v *viper.Viper, logger *zap.Logger) (*keystore.Store, error) {
	dir := v.GetString("keystore.dir")
	if dir == "" {
		dir = "/var/lib/kms-manage/keys"
	}
	s, err := keystore.Load(dir)
	if err != nil {
		return nil, err
	}
	// 暴露给 Prometheus：active key 年龄 + loaded key 总数。
	// 监控告警阈值参考 PCI-DSS 3.6.4：超过 90 天未轮换报 P1。
	metrics.LoadedKeyCount.Set(float64(len(s.List())))
	if meta, ok := s.Meta(s.ActiveKeyID()); ok {
		age := time.Since(meta.CreatedAt)
		metrics.ActiveKeyAgeSeconds.Set(age.Seconds())
		logger.Info("keystore loaded",
			zap.String("dir", dir),
			zap.String("active_key", s.ActiveKeyID()),
			zap.Int("total_keys", len(s.List())),
			zap.Duration("active_key_age", age),
			zap.Time("active_key_created_at", meta.CreatedAt),
		)
		// PCI-DSS 90 天: 7,776,000 秒。超过给 WARN 提醒运维。
		if age > 90*24*time.Hour {
			logger.Warn("active master key is older than 90 days; consider rotating (PCI-DSS 3.6.4)",
				zap.Duration("age", age))
		}
	} else {
		logger.Info("keystore loaded",
			zap.String("dir", dir),
			zap.String("active_key", s.ActiveKeyID()),
			zap.Int("total_keys", len(s.List())),
		)
	}
	return s, nil
}

func newKMSSvc(s *keystore.Store, logger *zap.Logger) *service.KMSService {
	return service.NewKMSService(s, logger)
}

func newServer(svc *service.KMSService, v *viper.Viper, logger *zap.Logger) (*server.Server, error) {
	tokens := map[string]string{}
	for _, t := range v.GetStringSlice("auth.tokens") {
		tokens[t] = "ok"
	}
	return server.NewServer(server.Deps{
		KMSSvc:       svc,
		AuthTokens:   tokens,
		AllowedIDs:   v.GetStringSlice("auth.allowed_client_ids"),
		TLS: server.TLSPaths{
			ServerCert: v.GetString("tls.server_cert"),
			ServerKey:  v.GetString("tls.server_key"),
			ClientCA:   v.GetString("tls.client_ca"),
		},
		RateLimitRPS: v.GetFloat64("rate_limit.rps"),
		RateBurst:    v.GetInt("rate_limit.burst"),
		Logger:       logger,
	})
}

func startGRPC(lc fx.Lifecycle, s *server.Server, v *viper.Viper, logger *zap.Logger) {
	port := v.GetInt("server.grpc_port")
	if port == 0 {
		port = 9290
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

func startMetricsHTTP(lc fx.Lifecycle, v *viper.Viper, ks *keystore.Store, logger *zap.Logger) {
	addr := v.GetString("metrics.addr")
	if addr == "" {
		addr = ":9390"
	}
	// readiness probe: 报告 active key + 总加载 key 数；keystore 不可用时 fail。
	probe := func() (string, int, error) {
		active := ks.ActiveKeyID()
		list := ks.List()
		return active, len(list), nil
	}
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			metrics.StartServer(addr, logger, probe)
			return nil
		},
	})
}
