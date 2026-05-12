// Package metrics — Prometheus 指标.

package metrics

import "github.com/prometheus/client_golang/prometheus"

var (
	ScreenTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "aml_screen_total",
		Help: "screen requests, by action (pass/review/block)",
	}, []string{"action", "trigger"})

	ScreenLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "aml_screen_latency_seconds",
		Help:    "screen request latency",
		Buckets: prometheus.ExponentialBuckets(0.005, 2, 12),
	}, []string{"trigger"})

	HitsByAction = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "aml_hits_total",
		Help: "hit records, by source",
	}, []string{"source"})

	ListEntries = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "aml_list_entries",
		Help: "current entry count per source",
	}, []string{"source"})

	RefreshSuccess = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "aml_refresh_total",
		Help: "list refresh attempts, by source / result",
	}, []string{"source", "result"})
)

func MustRegister(reg prometheus.Registerer) {
	reg.MustRegister(ScreenTotal, ScreenLatency, HitsByAction, ListEntries, RefreshSuccess)
}
