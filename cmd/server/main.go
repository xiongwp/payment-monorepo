// Command server 启动 kms-manage gRPC 服务。
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/viper"
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
	return v, nil
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
	logger.Info("keystore loaded",
		zap.String("dir", dir),
		zap.String("active_key", s.ActiveKeyID()),
		zap.Int("total_keys", len(s.List())),
	)
	return s, nil
}

func newKMSSvc(s *keystore.Store, logger *zap.Logger) *service.KMSService {
	return service.NewKMSService(s, logger)
}

func newServer(svc *service.KMSService, v *viper.Viper, logger *zap.Logger) *server.Server {
	tokens := map[string]string{}
	for _, t := range v.GetStringSlice("auth.tokens") {
		tokens[t] = "ok"
	}
	return server.NewServer(server.Deps{
		KMSSvc:       svc,
		AuthTokens:   tokens,
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
