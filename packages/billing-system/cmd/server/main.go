// billing-system server 入口 — uber/fx 装配, 跟 order-core / accounting-system 同款风格.
//
// 启动:
//
//	BILLING_HTTP_PORT=8080 ./billing-system
//
// 默认 in-memory repo + 一组示例 fee rules (从 configs/fee_rules.yaml 拉).
// 生产需要换 MySQL repo + Kafka ingest (监听 order-core 事件流自动算 fee).
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"

	"reconcile-system/packages/billing-system/internal/adminhttp"
	"reconcile-system/packages/billing-system/internal/domain"
	"reconcile-system/packages/billing-system/internal/feecalc"
	"reconcile-system/packages/billing-system/internal/repository"
	"reconcile-system/packages/billing-system/internal/statement"
)

func main() {
	fx.New(
		fx.Provide(
			newLogger,
			newRules,
			newRepo,
			newFeeCalc,
			newAggregator,
			newAdminAPI,
			newHTTPServer,
		),
		fx.Invoke(
			startHTTPServer,
			startDailyAggregator,
		),
		fx.WithLogger(func(log *zap.Logger) fxevent.Logger {
			return &fxevent.ZapLogger{Logger: log.Named("fx")}
		}),
	).Run()
}

func newLogger(lc fx.Lifecycle) (*zap.Logger, error) {
	logger, err := zap.NewProduction()
	if err != nil {
		return nil, fmt.Errorf("zap build: %w", err)
	}
	lc.Append(fx.Hook{OnStop: func(_ context.Context) error { _ = logger.Sync(); return nil }})
	return logger, nil
}

// newRules 加载 fee rules — yaml 找不到时返空切片 + Warn, 不 fail-fast (跟 v1 行为对齐).
func newRules(log *zap.Logger) []domain.FeeRule {
	path := envOr("BILLING_RULES_YAML", "/app/configs/fee_rules.yaml")
	rules, err := loadRules(path)
	if err != nil {
		log.Warn("load rules failed; starting with empty",
			zap.String("path", path), zap.Error(err))
		return nil
	}
	log.Info("rules loaded", zap.Int("count", len(rules)), zap.String("path", path))
	return rules
}

func newRepo() *repository.MemoryRepo { return repository.NewMemoryRepo() }

func newFeeCalc(rules []domain.FeeRule, repo *repository.MemoryRepo, log *zap.Logger) *feecalc.Service {
	baseCurrency := envOr("BILLING_BASE_CURRENCY", "USD")
	return feecalc.New(rules, repo, nopFX{}, baseCurrency, log)
}

func newAggregator(repo *repository.MemoryRepo, log *zap.Logger) *statement.Aggregator {
	return statement.New(repo, log)
}

func newAdminAPI(repo *repository.MemoryRepo, svc *feecalc.Service, agg *statement.Aggregator,
	rules []domain.FeeRule, log *zap.Logger) *adminhttp.Server {
	return adminhttp.New(repo, svc, agg, rules, log)
}

func newHTTPServer(api *adminhttp.Server) *http.Server {
	port := envOr("BILLING_HTTP_PORT", "8080")
	mux := http.NewServeMux()
	api.Mount(mux)
	mux.Handle("/metrics", promhttp.Handler())
	return &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

func startHTTPServer(lc fx.Lifecycle, srv *http.Server, log *zap.Logger) {
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			log.Info("billing-system listening", zap.String("addr", srv.Addr))
			go func() {
				if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					log.Error("http server failed", zap.Error(err))
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			shutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			return srv.Shutdown(shutCtx)
		},
	})
}

// startDailyAggregator 每分钟检查是否到 02:00, 到了跑前一天 daily statement;
// 月初 03:00 跑上月 monthly final.
func startDailyAggregator(lc fx.Lifecycle, agg *statement.Aggregator, log *zap.Logger) {
	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			go runDailyAggregator(ctx, agg, log)
			log.Info("daily aggregator cron started")
			return nil
		},
		OnStop: func(_ context.Context) error { cancel(); return nil },
	})
}

func runDailyAggregator(ctx context.Context, agg *statement.Aggregator, log *zap.Logger) {
	t := time.NewTicker(1 * time.Minute)
	defer t.Stop()
	lastRun := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := time.Now().UTC()
			date := now.Format("2006-01-02")
			if now.Hour() != 2 || lastRun == date {
				continue
			}
			yesterday := now.AddDate(0, 0, -1)
			from := time.Date(yesterday.Year(), yesterday.Month(), yesterday.Day(), 0, 0, 0, 0, time.UTC)
			to := from.AddDate(0, 0, 1)
			n, err := agg.RunPeriod(ctx, from, to, false)
			if err != nil {
				log.Warn("daily aggregator failed", zap.Error(err))
			} else {
				log.Info("daily aggregator done", zap.Int("statements", n),
					zap.String("date", yesterday.Format("2006-01-02")))
			}
			lastRun = date
			if now.Day() == 1 && now.Hour() == 3 {
				lastMonth := now.AddDate(0, -1, 0)
				mFrom := time.Date(lastMonth.Year(), lastMonth.Month(), 1, 0, 0, 0, 0, time.UTC)
				mTo := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
				n, err := agg.RunPeriod(ctx, mFrom, mTo, true)
				if err != nil {
					log.Warn("monthly aggregator failed", zap.Error(err))
				} else {
					log.Info("monthly aggregator done", zap.Int("statements", n))
				}
			}
		}
	}
}

func loadRules(path string) ([]domain.FeeRule, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Rules []domain.FeeRule `yaml:"rules"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	for i := range doc.Rules {
		if doc.Rules[i].ID == 0 {
			doc.Rules[i].ID = int64(i + 1)
		}
		if doc.Rules[i].CreatedAt.IsZero() {
			doc.Rules[i].CreatedAt = now
			doc.Rules[i].UpdatedAt = now
		}
		if doc.Rules[i].EffectiveFrom.IsZero() {
			doc.Rules[i].EffectiveFrom = now.AddDate(-1, 0, 0)
		}
	}
	return doc.Rules, nil
}

// nopFX 简化 FX provider — 暂时不接真实汇率源, 所有币种都按 1:1 算.
type nopFX struct{}

func (nopFX) Rate(_ context.Context, src, dst string, _ time.Time) (float64, error) {
	if src == dst {
		return 1.0, nil
	}
	pairs := map[string]float64{
		"PHP->USD": 0.018, "USD->PHP": 56.0,
		"SGD->USD": 0.74, "USD->SGD": 1.35,
		"TWD->USD": 0.031, "USD->TWD": 32.0,
		"EUR->USD": 1.08, "USD->EUR": 0.93,
	}
	if r, ok := pairs[src+"->"+dst]; ok {
		return r, nil
	}
	return 1.0, nil
}

func envOr(k, dflt string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return dflt
}
