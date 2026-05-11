// billing-system server 入口.
//
// 启动:
//   BILLING_HTTP_PORT=8080 ./billing-system
//
// 默认 in-memory repo + 一组示例 fee rules（从 configs/fee_rules.yaml 拉）。
// 生产需要换 MySQL repo + Kafka ingest（监听 order-core 事件流自动算 fee）。

package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"

	"reconcile-system/packages/billing-system/internal/adminhttp"
	"reconcile-system/packages/billing-system/internal/domain"
	"reconcile-system/packages/billing-system/internal/feecalc"
	"reconcile-system/packages/billing-system/internal/repository"
	"reconcile-system/packages/billing-system/internal/statement"
)

func main() {
	logger, _ := zap.NewProduction()
	defer logger.Sync()

	port := envOr("BILLING_HTTP_PORT", "8080")
	rulesPath := envOr("BILLING_RULES_YAML", "/app/configs/fee_rules.yaml")
	baseCurrency := envOr("BILLING_BASE_CURRENCY", "USD")

	// 1. 加载 fee rules
	rules, err := loadRules(rulesPath)
	if err != nil {
		logger.Warn("load rules failed; starting with empty",
			zap.String("path", rulesPath), zap.Error(err))
		rules = nil
	}
	logger.Info("rules loaded", zap.Int("count", len(rules)), zap.String("path", rulesPath))

	// 2. Repo / Service / Aggregator wire
	repo := repository.NewMemoryRepo()
	feecalcSvc := feecalc.New(rules, repo, nopFX{}, baseCurrency, logger)
	agg := statement.New(repo, logger)

	// 3. HTTP server
	mux := http.NewServeMux()
	api := adminhttp.New(repo, feecalcSvc, agg, rules, logger)
	api.Mount(mux)
	mux.Handle("/metrics", promhttp.Handler())

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// 4. 启动后台聚合 cron（每日凌晨 02:00 跑昨日 draft 账单）
	go runDailyAggregator(ctx, agg, logger)

	logger.Info("billing-system listening", zap.String("addr", srv.Addr))
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http server", zap.Error(err))
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	srv.Shutdown(shutCtx)
}

// runDailyAggregator 每分钟检查是否到 02:00，到了跑前一天的 daily statement。
//
// 生产应该用 robfig/cron，这里依赖最小化用 ticker + wall clock。
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
			// 跑昨天的 daily statement
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
			// 月初 03:00 跑上月 monthly final
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

// loadRules 从 YAML 读 rules。
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
	// 给 id / timestamp 兜底
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

// nopFX 简化 FX provider — 暂时不接真实汇率源，所有币种都按 1:1 算。
// 生产换 OANDAClient / XEClient 之类。
type nopFX struct{}

func (nopFX) Rate(_ context.Context, src, dst string, _ time.Time) (float64, error) {
	if src == dst {
		return 1.0, nil
	}
	// MVP: 硬编码几个常见对（生产换实时 API）
	pairs := map[string]float64{
		"PHP->USD": 0.018, "USD->PHP": 56.0,
		"SGD->USD": 0.74, "USD->SGD": 1.35,
		"TWD->USD": 0.031, "USD->TWD": 32.0,
		"EUR->USD": 1.08, "USD->EUR": 0.93,
	}
	if r, ok := pairs[src+"->"+dst]; ok {
		return r, nil
	}
	return 1.0, nil // 兜底；ops 看到大量同币种 fx_event 应警惕
}

func envOr(k, dflt string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return dflt
}
