package metrics

import "github.com/prometheus/client_golang/prometheus"

var (
	ExchangeTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vault_exchange_total",
		Help: "exchange requests, by result (dedup/created/error)",
	}, []string{"result", "brand"})

	ProvisionTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vault_provision_total",
		Help: "provision attempts, by provider/result",
	}, []string{"provider", "result"})

	ChargeIntentTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vault_charge_intent_total",
		Help: "charge intent (cryptogram generated), by provider/recurring",
	}, []string{"provider", "recurring"})

	TokenSuspendTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vault_suspend_total",
		Help: "tokens suspended, by reason",
	}, []string{"reason"})

	ProvisionLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "vault_provision_latency_seconds",
		Help:    "provision latency",
		Buckets: prometheus.ExponentialBuckets(0.02, 2, 10),
	}, []string{"provider"})
)

func MustRegister(reg prometheus.Registerer) {
	reg.MustRegister(ExchangeTotal, ProvisionTotal, ChargeIntentTotal, TokenSuspendTotal, ProvisionLatency)
}
