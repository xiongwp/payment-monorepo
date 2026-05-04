package dbx

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"gorm.io/gorm"
)

// PoolCollector 把 Manager 所有底层 *sql.DB 的池统计导成 Prometheus gauges。
// 每次抓取时直接调 (*sql.DB).Stats()，不需要额外后台 goroutine。
type PoolCollector struct {
	mgr *Manager

	openConns     *prometheus.Desc
	inUse         *prometheus.Desc
	idle          *prometheus.Desc
	waitCount     *prometheus.Desc
	waitDuration  *prometheus.Desc
	maxIdleClosed *prometheus.Desc
	maxLifeClosed *prometheus.Desc
}

// NewPoolCollector 构造
func NewPoolCollector(mgr *Manager) *PoolCollector {
	label := []string{"db"}
	d := func(name, help string) *prometheus.Desc {
		return prometheus.NewDesc("user_merchant_db_pool_"+name, help, label, nil)
	}
	return &PoolCollector{
		mgr:           mgr,
		openConns:     d("open", "Number of established connections both in use and idle."),
		inUse:         d("in_use", "Number of connections currently in use."),
		idle:          d("idle", "Number of idle connections."),
		waitCount:     d("wait_count_total", "Total number of connections waited for."),
		waitDuration:  d("wait_seconds_total", "Total time blocked waiting for a new connection."),
		maxIdleClosed: d("max_idle_closed_total", "Total connections closed due to SetMaxIdleConns."),
		maxLifeClosed: d("max_lifetime_closed_total", "Total connections closed due to SetConnMaxLifetime."),
	}
}

// Describe impl prometheus.Collector
func (c *PoolCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.openConns
	ch <- c.inUse
	ch <- c.idle
	ch <- c.waitCount
	ch <- c.waitDuration
	ch <- c.maxIdleClosed
	ch <- c.maxLifeClosed
}

// Collect impl prometheus.Collector：meta + 每个 replica + 每个 shard 一组 series。
func (c *PoolCollector) Collect(ch chan<- prometheus.Metric) {
	c.emit(ch, "meta", c.mgr.meta)
	for i, db := range c.mgr.replicas {
		c.emit(ch, "replica-"+strconv.Itoa(i), db)
	}
	for i, db := range c.mgr.shards {
		c.emit(ch, "shard-"+strconv.Itoa(i), db)
	}
}

func (c *PoolCollector) emit(ch chan<- prometheus.Metric, label string, gdb *gorm.DB) {
	if gdb == nil {
		return
	}
	sqlDB, err := gdb.DB()
	if err != nil {
		return
	}
	s := sqlDB.Stats()
	ch <- prometheus.MustNewConstMetric(c.openConns, prometheus.GaugeValue, float64(s.OpenConnections), label)
	ch <- prometheus.MustNewConstMetric(c.inUse, prometheus.GaugeValue, float64(s.InUse), label)
	ch <- prometheus.MustNewConstMetric(c.idle, prometheus.GaugeValue, float64(s.Idle), label)
	ch <- prometheus.MustNewConstMetric(c.waitCount, prometheus.CounterValue, float64(s.WaitCount), label)
	ch <- prometheus.MustNewConstMetric(c.waitDuration, prometheus.CounterValue, s.WaitDuration.Seconds(), label)
	ch <- prometheus.MustNewConstMetric(c.maxIdleClosed, prometheus.CounterValue, float64(s.MaxIdleClosed), label)
	ch <- prometheus.MustNewConstMetric(c.maxLifeClosed, prometheus.CounterValue, float64(s.MaxLifetimeClosed), label)
}
