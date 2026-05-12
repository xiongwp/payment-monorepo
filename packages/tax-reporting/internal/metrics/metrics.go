package metrics

import "github.com/prometheus/client_golang/prometheus"

var (
	PayoutsIngested = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tax_payouts_ingested_total",
		Help: "payout events ingested, by jurisdiction",
	}, []string{"jurisdiction", "source"})

	FormsGenerated = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tax_forms_generated_total",
		Help: "tax forms generated, by type",
	}, []string{"form_type"})

	FormsFiled = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tax_forms_filed_total",
		Help: "tax forms successfully e-filed",
	}, []string{"form_type", "result"})

	EligibleMerchants = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "tax_eligible_merchants",
		Help: "merchants eligible for filing in current cycle",
	}, []string{"form_type", "year"})
)

func MustRegister(reg prometheus.Registerer) {
	reg.MustRegister(PayoutsIngested, FormsGenerated, FormsFiled, EligibleMerchants)
}
