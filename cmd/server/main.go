// Command server 启动 clearing-settlement 服务。
//
// 当前是 skeleton：只起 admin HTTP + metrics + skeleton SettlementService（no-op）。
// 后续 PR 接 DB（settlement_run / settlement_record） + accounting client +
// cron scheduler + gRPC server。
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"
	"github.com/xiongwp/payment-util/trace"
	"go.uber.org/fx"
	"go.uber.org/zap"

	"github.com/xiongwp/clearing-settlement/internal/metrics"
	"github.com/xiongwp/clearing-settlement/internal/server"
	"github.com/xiongwp/clearing-settlement/internal/service"
)

func main() {
	metrics.Register()

	otelShutdown, otelErr := trace.InitOTel(context.Background(), "clearing-settlement", os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
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
			newSettlementService,
			newAdminConfig,
			newAdminServer,
		),
		fx.Invoke(startAdmin, startMetrics),
	)
	app.Run()
}

func loadConfig() (*viper.Viper, error) {
	v := viper.New()
	v.SetEnvPrefix("CLR")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath("./config")
	v.AddConfigPath(".")
	v.AddConfigPath("/etc/clearing-settlement")
	if err := v.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if !errors.As(err, &notFound) {
			return nil, err
		}
	}
	return v, nil
}

func newLogger() (*zap.Logger, error) {
	if os.Getenv("CLR_LOG_DEV") == "true" {
		return zap.NewDevelopment()
	}
	return zap.NewProduction()
}

func newSettlementService(logger *zap.Logger) service.SettlementService {
	return service.NewSettlementService(logger)
}

func newAdminConfig(v *viper.Viper) server.AdminConfig {
	port := v.GetInt("admin.port")
	if port == 0 {
		port = 9891
	}
	token := os.Getenv("ADMIN_HTTP_TOKEN")
	if token == "" {
		token = v.GetString("admin.token")
	}
	return server.AdminConfig{Port: port, Token: token}
}

func newAdminServer(cfg server.AdminConfig, svc service.SettlementService, logger *zap.Logger) *server.AdminServer {
	return server.NewAdminServer(cfg, svc, logger)
}

func startAdmin(lc fx.Lifecycle, srv *server.AdminServer, logger *zap.Logger) {
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error { return srv.Start() },
		OnStop: func(ctx context.Context) error {
			logger.Info("clearing-settlement admin shutting down")
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			return srv.Stop(ctx)
		},
	})
}

func startMetrics(lc fx.Lifecycle, v *viper.Viper, logger *zap.Logger) {
	port := v.GetInt("metrics.port")
	if port == 0 {
		port = 9892
	}
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			metrics.Serve(port, logger)
			return nil
		},
	})
}
