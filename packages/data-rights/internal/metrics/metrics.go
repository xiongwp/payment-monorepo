package metrics

import "github.com/prometheus/client_golang/prometheus"

var (
	RequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "data_rights_requests_total",
		Help: "DSAR/RTBF requests, by type/jurisdiction",
	}, []string{"type", "jurisdiction"})

	RequestsByState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "data_rights_requests_by_state",
		Help: "current request count by state",
	}, []string{"state"})

	OverdueGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "data_rights_overdue",
		Help: "requests past 30-day deadline (legal violation if >0)",
	})

	ServiceErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "data_rights_service_errors_total",
		Help: "downstream service errors during fan-out",
	}, []string{"service", "action"})
)

func MustRegister(reg prometheus.Registerer) {
	reg.MustRegister(RequestsTotal, RequestsByState, OverdueGauge, ServiceErrors)
}
