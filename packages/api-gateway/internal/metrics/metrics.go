// Package metrics: api-gateway 自身的 Prometheus 计数器 + 一个独立 HTTP server。
//
// 与公网 / admin HTTP 端口分离的目的：scrape 不受公网流量限流影响，且 metrics
// 端口可以只开内网（K8s NetworkPolicy / iptables）。
package metrics

import (
	"encoding/json"
	"fmt"
	"net/http"
	_ "net/http/pprof" // ROI-2c: 注册 /debug/pprof/* 到 http.DefaultServeMux

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	// HTTPRequestsTotal labels: method, path_prefix(只取一级路径), status_code。
	// path_prefix 而非 raw path：避免 cardinality 爆炸（每个 order_id 都是一个 label）。
	HTTPRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "api_gateway",
			Subsystem: "http",
			Name:      "requests_total",
			Help:      "HTTP requests received, partitioned by method/path-prefix/status.",
		},
		[]string{"method", "path_prefix", "status"},
	)

	// HTTPRequestDuration label 同上；buckets 偏低延迟（gateway 加 < 5ms 是目标）。
	HTTPRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "api_gateway",
			Subsystem: "http",
			Name:      "request_duration_seconds",
			Help:      "HTTP request latency in seconds.",
			Buckets:   []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		},
		[]string{"method", "path_prefix", "status"},
	)

	// RateLimitHits labels: dim(ip / merchant)。
	RateLimitHits = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "api_gateway",
			Subsystem: "ratelimit",
			Name:      "hits_total",
			Help:      "Requests rejected due to rate limiting.",
		},
		[]string{"dim"},
	)

	// AuthRejects API key 校验失败计数。无 label（攻击者可能扫多种 key，单维度即可）。
	AuthRejects = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: "api_gateway",
			Subsystem: "auth",
			Name:      "rejects_total",
			Help:      "Requests rejected due to auth failure.",
		},
	)

	// ShadowHeaderRejected 不可信源伪造 X-Shadow header 被 strip 的次数。
	// 这个数字非零意味着外部探测在试图把流量绕到影子环境，告警阈值很低（持续 > 0/min）。
	ShadowHeaderRejected = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "api_gateway",
			Subsystem: "shadow",
			Name:      "header_rejected_total",
			Help:      "X-Shadow header from untrusted source stripped at the edge.",
		},
		[]string{"path"},
	)
)

// Register 把所有指标注册到全局 registry。main 启动时调一次；重复注册会 panic。
func Register() {
	prometheus.MustRegister(HTTPRequestsTotal, HTTPRequestDuration, RateLimitHits, AuthRejects, ShadowHeaderRejected)
}

// Serve 单独的 metrics HTTP server。/metrics 总是 200 即使其余端口异常。
//
// ROI-2c: 兼容老 API; 内部调 ServeWithAdmin.
func Serve(port int, logger *zap.Logger) *http.Server {
	return ServeWithAdmin(port, logger, nil)
}

// ServeWithAdmin: 同 Serve 但加 /debug/pprof/* + /admin/log-level (level=nil 时 readonly).
//
// ROI-2c: 与 split-payment / payment-core / payment-channel 一致.
// /healthz + /readyz 由主 HTTP server 处理 (api-gateway 是 HTTP gateway 自身, livez 走主端口);
// 这里只补 metrics 端口的 ops surface.
func ServeWithAdmin(port int, logger *zap.Logger, level *zap.AtomicLevel) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	// ROI-2c: pprof.
	mux.Handle("/debug/pprof/", http.DefaultServeMux)
	// ROI-2c: dynamic log level.
	if level != nil {
		mux.HandleFunc("/admin/log-level", func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]string{"level": level.Level().String()})
			case http.MethodPut, http.MethodPost:
				var body struct{ Level string `json:"level"` }
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					http.Error(w, "bad json", http.StatusBadRequest)
					return
				}
				var l zapcore.Level
				if err := l.UnmarshalText([]byte(body.Level)); err != nil {
					http.Error(w, "bad level", http.StatusBadRequest)
					return
				}
				level.SetLevel(l)
				logger.Info("log level changed", zap.String("level", body.Level))
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]string{"level": l.String()})
			default:
				http.Error(w, "method", http.StatusMethodNotAllowed)
			}
		})
	}
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
