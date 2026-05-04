// Package healthx: DBStats Prometheus 收集器。
//
// 用法：
//
//	collector := healthx.NewDBStatsCollector("accounting", db)
//	prometheus.MustRegister(collector)
//
// 暴露的指标（按 db_label 分维度，方便区分多 shard）：
//
//	{namespace}_db_open_connections{db}     当前打开连接（IDLE + InUse）
//	{namespace}_db_in_use{db}               正在被 query 使用的连接
//	{namespace}_db_idle{db}                 空闲连接
//	{namespace}_db_max_open{db}             配置的上限
//	{namespace}_db_wait_count_total{db}     累计等待次数
//	{namespace}_db_wait_seconds_total{db}   累计等待秒数（pool exhaustion 直接看这个 rate）
//
// 用 prometheus.Collector 接口而非 MustRegister 一堆 Gauge：每次 scrape 才
// 调用 db.Stats()，避免后台 goroutine + 数据竞态。
package healthx

import (
	"database/sql"

	"github.com/prometheus/client_golang/prometheus"
)

// DBStatsCollector 把一组 *sql.DB 的 stats 暴露成 Prometheus gauge / counter。
// 多个 shard 时可以传一个 map 进来，每个 shard 用不同 label 值。
type DBStatsCollector struct {
	namespace string
	dbs       map[string]*sql.DB

	descOpen     *prometheus.Desc
	descInUse    *prometheus.Desc
	descIdle     *prometheus.Desc
	descMaxOpen  *prometheus.Desc
	descWaitCnt  *prometheus.Desc
	descWaitSecs *prometheus.Desc
}

// NewDBStatsCollector 单 DB 简化版。namespace 形如 "accounting" / "order_core"。
func NewDBStatsCollector(namespace string, db *sql.DB) *DBStatsCollector {
	return NewMultiDBStatsCollector(namespace, map[string]*sql.DB{"primary": db})
}

// NewMultiDBStatsCollector 多 DB（多 shard）版。dbs 的 key 用作 db label 值。
func NewMultiDBStatsCollector(namespace string, dbs map[string]*sql.DB) *DBStatsCollector {
	c := &DBStatsCollector{
		namespace: namespace,
		dbs:       dbs,
	}
	labels := []string{"db"}
	c.descOpen = prometheus.NewDesc(namespace+"_db_open_connections", "Open connections (in-use + idle)", labels, nil)
	c.descInUse = prometheus.NewDesc(namespace+"_db_in_use", "Connections currently in use by a query", labels, nil)
	c.descIdle = prometheus.NewDesc(namespace+"_db_idle", "Idle connections in pool", labels, nil)
	c.descMaxOpen = prometheus.NewDesc(namespace+"_db_max_open", "Configured pool max", labels, nil)
	c.descWaitCnt = prometheus.NewDesc(namespace+"_db_wait_count_total", "Cumulative count of waits for a connection", labels, nil)
	c.descWaitSecs = prometheus.NewDesc(namespace+"_db_wait_seconds_total", "Cumulative time spent waiting for a connection", labels, nil)
	return c
}

// Describe 实现 prometheus.Collector。
func (c *DBStatsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.descOpen
	ch <- c.descInUse
	ch <- c.descIdle
	ch <- c.descMaxOpen
	ch <- c.descWaitCnt
	ch <- c.descWaitSecs
}

// Collect 实现 prometheus.Collector。每次 scrape 调一次 db.Stats()。
func (c *DBStatsCollector) Collect(ch chan<- prometheus.Metric) {
	for label, db := range c.dbs {
		if db == nil {
			continue
		}
		s := db.Stats()
		ch <- prometheus.MustNewConstMetric(c.descOpen, prometheus.GaugeValue, float64(s.OpenConnections), label)
		ch <- prometheus.MustNewConstMetric(c.descInUse, prometheus.GaugeValue, float64(s.InUse), label)
		ch <- prometheus.MustNewConstMetric(c.descIdle, prometheus.GaugeValue, float64(s.Idle), label)
		ch <- prometheus.MustNewConstMetric(c.descMaxOpen, prometheus.GaugeValue, float64(s.MaxOpenConnections), label)
		ch <- prometheus.MustNewConstMetric(c.descWaitCnt, prometheus.CounterValue, float64(s.WaitCount), label)
		ch <- prometheus.MustNewConstMetric(c.descWaitSecs, prometheus.CounterValue, s.WaitDuration.Seconds(), label)
	}
}
