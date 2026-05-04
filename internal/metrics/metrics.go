// Package metrics: Prometheus 指标 + 独立 /metrics 端口。形状参照 api-gateway。
package metrics

import (
	"fmt"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

var (
	// SettlementRunsTotal labels: status (started / completed / failed)。
	SettlementRunsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "clearing_settlement",
			Subsystem: "run",
			Name:      "total",
			Help:      "Settlement runs by status outcome.",
		},
		[]string{"status"},
	)

	// MerchantSettlementDuration 单商户结算耗时直方图。
	MerchantSettlementDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "clearing_settlement",
			Subsystem: "merchant",
			Name:      "duration_seconds",
			Help:      "Per-merchant settlement processing duration.",
			Buckets:   []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		},
		[]string{"status"},
	)
)

// Register 注册指标。重复调用会 panic（prometheus 默认行为）。
func Register() {
	prometheus.MustRegister(SettlementRunsTotal, MerchantSettlementDuration)
}

// Serve 启动独立 /metrics HTTP。
func Serve(port int, logger *zap.Logger) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
	}
	go func() {
		logger.Info("metrics listening", zap.Int("port", port))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("metrics http error", zap.Error(err))
		}
	}()
	return srv
}
