// Package metrics — oauth2-server Prometheus 指标。
//
// 暴露在 /metrics endpoint (由 promhttp.Handler() 接管)。
//
// 指标分 4 类:
//   1. token: 颁发量 / 时延 / 失败原因分布
//   2. introspect: 量 / 时延 / active 占比
//   3. jwks: 拉取量 / cache hit
//   4. admin: 创建 / rotate / suspend 量
//   5. key: rotation 事件 + active key age

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// Token 颁发 (按 grant_type, owner_type, outcome)
	TokenIssueTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "oauth2_token_issue_total",
		Help: "Number of /oauth2/token requests",
	}, []string{"grant_type", "owner_type", "outcome"})

	TokenIssueDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "oauth2_token_issue_duration_seconds",
		Help:    "Latency of /oauth2/token requests",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0},
	}, []string{"outcome"})

	// Introspect
	IntrospectTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "oauth2_introspect_total",
		Help: "Number of /oauth2/introspect requests",
	}, []string{"active"})

	IntrospectDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "oauth2_introspect_duration_seconds",
		Help:    "Latency of /oauth2/introspect requests",
		Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5},
	})

	// Revoke
	RevokeTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "oauth2_revoke_total",
		Help: "Number of tokens revoked",
	})

	// JWKS
	JWKSFetchTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "oauth2_jwks_fetch_total",
		Help: "Number of /.well-known/jwks.json requests served",
	})

	// Admin actions
	AdminActionTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "oauth2_admin_action_total",
		Help: "Admin API calls by action",
	}, []string{"action", "outcome"})

	// Key
	KeyRotationTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "oauth2_key_rotation_total",
		Help: "RSA key rotation events",
	})

	ActiveKeyAgeSeconds = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "oauth2_active_key_age_seconds",
		Help: "Age of currently active RSA key in seconds",
	})

	RetiredKeysCount = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "oauth2_retired_keys_count",
		Help: "Number of retired (still verify-able) keys",
	})

	// Client store size
	ClientsRegistered = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "oauth2_clients_registered_total",
		Help: "Number of registered OAuth clients",
	})

	RevokedTokens = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "oauth2_revoked_tokens_count",
		Help: "Active revocation entries (not yet GC'd)",
	})

	// Rate limit hits
	RateLimitHits = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "oauth2_rate_limit_hits_total",
		Help: "Requests rejected by rate limiter",
	}, []string{"endpoint", "limit_key"})
)
