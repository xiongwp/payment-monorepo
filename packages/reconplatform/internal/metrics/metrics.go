package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// Exception counter: total exceptions detected, labeled by type and severity
	// type: "amount_mismatch", "state_mismatch", etc.
	// severity: "critical", "warning", "info"
	ExceptionTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "recon_exception_total",
			Help: "Total exceptions detected in reconciliation",
		},
		[]string{"type", "severity"},
	)

	// Exception pending gauge: current count of unprocessed exceptions
	ExceptionPending = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "recon_exception_pending",
			Help: "Current count of pending unprocessed exceptions",
		},
	)

	// Diff amount counter: total amount differences by type
	// type: "amount_mismatch", "state_mismatch", etc.
	DiffAmountMinorTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "recon_diff_amount_minor_total",
			Help: "Total amount differences detected (in minor units, e.g. centavos)",
		},
		[]string{"type"},
	)

	// Run duration histogram: reconciliation run duration in seconds
	RunDurationSeconds = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "recon_run_duration_seconds",
			Help:    "Reconciliation run duration in seconds",
			Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1, 2, 5, 10},
		},
	)

	// Run error counter: reconciliation run failures
	RunErrorsTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "recon_run_errors_total",
			Help: "Total reconciliation run errors",
		},
	)

	// Run total counter: total reconciliation runs
	RunTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "recon_run_total",
			Help: "Total reconciliation runs attempted",
		},
	)
)
