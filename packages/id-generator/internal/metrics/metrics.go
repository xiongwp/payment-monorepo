package metrics

import "github.com/prometheus/client_golang/prometheus"

var (
	IDGenCounter = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "id_generated_total",
			Help: "Total generated IDs",
		},
	)

	IDGenLatency = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "id_gen_latency_ms",
			Buckets: prometheus.DefBuckets,
		},
	)

	ClockRollback = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "clock_rollback_total",
		},
	)

	SegmentExhaust = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "segment_exhaust_total",
		},
	)
)

func Init() {
	prometheus.MustRegister(IDGenCounter, IDGenLatency, ClockRollback, SegmentExhaust)
}
