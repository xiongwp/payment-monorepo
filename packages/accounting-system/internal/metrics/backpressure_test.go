// backpressure_test.go — outbox backpressure metric smoke
//
// 这些 metric 是 OPS dashboard / 告警 source；测试目的是防止后续 refactor
// 误删 metric 注册（删了 Prometheus scrape 拿到 missing label 报错，
// 但 go test 阶段就能发现）。
package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
)

func TestOutboxBackpressureEngaged_Smoke(t *testing.T) {
	// 重置初值
	OutboxBackpressureEngaged.Set(0)
	assert.Equal(t, 0.0, testutil.ToFloat64(OutboxBackpressureEngaged))

	OutboxBackpressureEngaged.Set(1)
	assert.Equal(t, 1.0, testutil.ToFloat64(OutboxBackpressureEngaged))

	OutboxBackpressureEngaged.Set(0)
	assert.Equal(t, 0.0, testutil.ToFloat64(OutboxBackpressureEngaged))
}

func TestOutboxBackpressureTransitions_Smoke(t *testing.T) {
	engage := OutboxBackpressureTransitionsTotal.WithLabelValues("engage")
	release := OutboxBackpressureTransitionsTotal.WithLabelValues("release")

	beforeE := testutil.ToFloat64(engage)
	beforeR := testutil.ToFloat64(release)

	engage.Inc()
	engage.Inc()
	release.Inc()

	assert.Equal(t, beforeE+2, testutil.ToFloat64(engage))
	assert.Equal(t, beforeR+1, testutil.ToFloat64(release))
}

// TestBookingTotalSingleDBLabel — 防止 tcc fast-path 标签被误删
func TestBookingTotal_SingleDBLabel_Smoke(t *testing.T) {
	c := BookingTotal.WithLabelValues("tcc_single_db")
	before := testutil.ToFloat64(c)
	c.Inc()
	assert.Equal(t, before+1, testutil.ToFloat64(c))
}
