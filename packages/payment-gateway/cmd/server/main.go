// payment-gateway server 入口 — uber/fx 装配, 跟 order-core / accounting-system / payment-core 同款风格.
//
// 启动:
//
//	PAYMENT_GATEWAY_HTTP_PORT=8080 ./payment-gateway
//
// fx 装配:
//   - Logger / port 配置 / Vault / KMS / Tokenizer / Channels / Router / adminhttp 全部走 fx.Provide
//   - http.Server 走 fx.Lifecycle.OnStart + OnStop, SIGTERM 时 10s graceful shutdown
//   - 跟 payment-channel / payment-core / order-core 一致, 后续接入 config-center / metrics
//     / Kafka 等组件直接 fx.Provide 注入, 不再散在 main()
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
	"go.uber.org/zap"

	"reconcile-system/packages/payment-gateway/internal/adminhttp"
	"reconcile-system/packages/payment-gateway/internal/repository"
	"reconcile-system/packages/payment-gateway/internal/routing"
	"reconcile-system/packages/payment-gateway/internal/tokenize"
)

func main() {
	fx.New(
		fx.Provide(
			newLogger,
			newHTTPPort,
			newVault,
			newKMS,
			newTokenizer,
			newChannels,
			newRouter,
			newAdminAPI,
			newHTTPServer,
		),
		fx.Invoke(startHTTPServer),
		fx.WithLogger(func(log *zap.Logger) fxevent.Logger {
			return &fxevent.ZapLogger{Logger: log.Named("fx")}
		}),
	).Run()
}

// ─── infra Providers ───────────────────────────────────────────────────────

// newLogger zap.NewProduction; lifecycle OnStop 注册 Sync 防止退出时缓冲日志丢失.
//
// 跟 order-core / payment-core 一致 — 全局只构造一次 *zap.Logger, 其它 Provider 注入复用.
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

// httpPort 类型别名 — fx 用类型识别 Provider, 避免跟其它 string 撞型.
type httpPort string

// newHTTPPort 读 PAYMENT_GATEWAY_HTTP_PORT (env override), 默认 8080.
//
// 后续接 config-center 时只需替换这个 Provider 实现, fx 会自动重接所有下游.
func newHTTPPort(log *zap.Logger) httpPort {
	if v := os.Getenv("PAYMENT_GATEWAY_HTTP_PORT"); v != "" {
		log.Info("payment-gateway http port from env", zap.String("port", v))
		return httpPort(v)
	}
	return httpPort("8080")
}

// ─── tokenize Providers ────────────────────────────────────────────────────

// newVault 当前 memory 实现 — dev / 单测. 生产换 Redis / Vault 后端时只替换这个 Provider.
func newVault() *repository.MemoryVault {
	return repository.NewMemoryVault()
}

// newKMS 当前 NopKMS 占位 (固定 key). 生产接 AWS KMS / kms-manage 服务时替换 Provider.
func newKMS() repository.NopKMS {
	return repository.NopKMS{Key: 0x42}
}

// newTokenizer 把 KMS + Vault 注入, 暴露 *tokenize.Service 给 adminhttp.
func newTokenizer(kms repository.NopKMS, vault *repository.MemoryVault) *tokenize.Service {
	return tokenize.New(kms, vault)
}

// ─── routing Providers ─────────────────────────────────────────────────────

// newChannels 硬编码示例 channel 列表 — TODO: 接 config-center 后改 Provider 内部从配置读.
//
// 同步 cmd/server/main.go 历史行为 (4 条 sample channel: visa/mc/gcash/bank-ph),
// 保证现有 dev 行为不变.
func newChannels() []routing.Channel {
	return []routing.Channel{
		{ID: "visa-stripe", Name: "Visa via Stripe", Products: []string{"card_charge"},
			SupportedBINs: []string{"400000-499999"}, SupportedCcy: []string{"USD", "PHP", "SGD"},
			Regions: []string{"PH", "SG", "US"}, FeeBPS: 290, Priority: 80},
		{ID: "mc-adyen", Name: "Mastercard via Adyen", Products: []string{"card_charge"},
			SupportedBINs: []string{"510000-559999"}, SupportedCcy: []string{"USD", "PHP", "SGD"},
			Regions: []string{"PH", "SG"}, FeeBPS: 270, Priority: 70},
		{ID: "gcash-direct", Name: "GCash direct", Products: []string{"wallet"},
			SupportedCcy: []string{"PHP"}, Regions: []string{"PH"}, FeeBPS: 150, Priority: 90},
		{ID: "bank-ph", Name: "PH bank transfer", Products: []string{"bank_transfer"},
			SupportedCcy: []string{"PHP"}, Regions: []string{"PH"}, FeeBPS: 50,
			MinAmountMinor: 100000, Priority: 50},
	}
}

// newRouter routing.Router — 注入 channels, 后续 SwapChannels 由 admin API 路径触发 (热更新).
func newRouter(channels []routing.Channel) *routing.Router {
	return routing.New(channels)
}

// ─── admin HTTP server ─────────────────────────────────────────────────────

// newAdminAPI 拼装 adminhttp.Server — tokenize + routing + logger 都注入.
func newAdminAPI(tok *tokenize.Service, router *routing.Router, log *zap.Logger) *adminhttp.Server {
	return adminhttp.New(tok, router, log)
}

// newHTTPServer 构造 http.Server, 路由由 adminhttp.Server.Mount(mux) 注册.
//
// 跟 payment-channel / payment-core 同款: Server 仅作为对象 Provide, ListenAndServe / Shutdown
// 由 startHTTPServer (fx.Invoke) 通过 lifecycle hook 管理.
func newHTTPServer(api *adminhttp.Server, port httpPort) *http.Server {
	mux := http.NewServeMux()
	api.Mount(mux)
	return &http.Server{
		Addr:              ":" + string(port),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

// startHTTPServer lifecycle: OnStart 后台 ListenAndServe; OnStop 10s graceful shutdown.
//
// 跟 order-core/startGRPC / payment-core/startAdminHTTP 一致 — fx 退出协议确保
// SIGTERM → fx.Stop → OnStop hooks 依次执行 → http.Server.Shutdown.
func startHTTPServer(lc fx.Lifecycle, srv *http.Server, log *zap.Logger) {
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			log.Info("payment-gateway listening", zap.String("addr", srv.Addr))
			go func() {
				if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					// 跟历史行为一致 — 监听失败仅 log, fx 主流程靠 fx-managed signal 走优雅退出.
					log.Error("http listen failed", zap.String("addr", srv.Addr), zap.Error(err))
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			shutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if err := srv.Shutdown(shutCtx); err != nil {
				log.Warn("http shutdown error", zap.Error(err))
				return err
			}
			log.Info("payment-gateway http server stopped")
			return nil
		},
	})
}
