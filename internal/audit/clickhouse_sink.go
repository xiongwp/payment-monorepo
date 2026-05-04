// clickhouse_sink.go: audit.Sink 的 ClickHouse 参考实现。
//
// **未编译进默认 build**：避免给 risk-manage 强制加 clickhouse-go 依赖。
// 接入时：
//
//  1. go.mod 加：
//     require github.com/ClickHouse/clickhouse-go/v2 v2.x.x
//  2. 删除文件顶部的 //go:build clickhouse 标签
//  3. main.go newAuditSink 把 ClickHouseSink 加到 MultiSink：
//
//     conn, _ := clickhouse.Open(&clickhouse.Options{Addr: []string{"ch:9000"}})
//     return audit.MultiSink{
//         &audit.LogSink{Logger: logger},
//         audit.NewMemSink(4096),
//         audit.NewClickHouseSink(conn, logger),
//     }
//
// Schema（ClickHouse，UTC）：
//
//	CREATE TABLE risk_decision_audit (
//	    decision_id        String,
//	    occurred_at        DateTime64(9, 'UTC'),
//	    rule_version       Int32,
//	    pi_id              String,
//	    merchant_id        LowCardinality(String),
//	    customer_id        String,
//	    amount             Int64,
//	    currency           LowCardinality(String),
//	    payment_method     LowCardinality(String),
//	    country            LowCardinality(String),
//	    ip_address         String,
//	    device_id          String,
//	    verdict            LowCardinality(String),
//	    risk_score         Int32,
//	    risk_level         LowCardinality(String),
//	    hit_rule_ids       Array(String),
//	    shadow_rule_ids    Array(String),
//	    ml_score           Float64,
//	    ml_model_ver       LowCardinality(String),
//	    eval_duration_ms   Float64,
//	    metadata_json      String,
//	    INDEX idx_pi (pi_id) TYPE bloom_filter GRANULARITY 1,
//	    INDEX idx_did (decision_id) TYPE bloom_filter GRANULARITY 1
//	) ENGINE = MergeTree
//	PARTITION BY toYYYYMM(occurred_at)
//	ORDER BY (merchant_id, occurred_at)
//	TTL occurred_at + INTERVAL 730 DAY;
//
// 性能：
//   - Write 是异步 batch（默认 1000 条 / 1 秒一刷）；fail-open
//   - 高峰可上 Kafka producer → ClickHouse Kafka engine 表，进一步解耦



package audit

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"go.uber.org/zap"
)

const (
	chBatchSize = 1000
	chBatchWait = time.Second
)

type ClickHouseSink struct {
	conn   clickhouse.Conn
	logger *zap.Logger

	mu      sync.Mutex
	buf     []*DecisionAudit
	stop    chan struct{}
	stopped bool
}

func NewClickHouseSink(conn clickhouse.Conn, logger *zap.Logger) *ClickHouseSink {
	s := &ClickHouseSink{
		conn:   conn,
		logger: logger,
		stop:   make(chan struct{}),
	}
	go s.flushLoop()
	return s
}

// Write 入 buffer；满 batch 立刻 flush。**不阻塞主路径**：插不进 buf 时 drop +
// log，让风控决策永远走得动。
func (s *ClickHouseSink) Write(ctx context.Context, a *DecisionAudit) {
	if a == nil {
		return
	}
	s.mu.Lock()
	s.buf = append(s.buf, a)
	full := len(s.buf) >= chBatchSize
	s.mu.Unlock()
	if full {
		s.flush()
	}
}

func (s *ClickHouseSink) flushLoop() {
	t := time.NewTicker(chBatchWait)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.flush()
		case <-s.stop:
			s.flush()
			return
		}
	}
}

func (s *ClickHouseSink) flush() {
	s.mu.Lock()
	batch := s.buf
	s.buf = nil
	s.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bch, err := s.conn.PrepareBatch(ctx, `
INSERT INTO risk_decision_audit (
    decision_id, occurred_at, rule_version,
    pi_id, merchant_id, customer_id, amount, currency, payment_method, country,
    ip_address, device_id,
    verdict, risk_score, risk_level, hit_rule_ids, shadow_rule_ids,
    ml_score, ml_model_ver, eval_duration_ms, metadata_json
)`)
	if err != nil {
		s.logger.Warn("clickhouse audit prepare failed", zap.Error(err))
		return
	}
	for _, a := range batch {
		hitIDs := make([]string, 0, len(a.Hits))
		for _, h := range a.Hits {
			hitIDs = append(hitIDs, h.RuleID)
		}
		shadowIDs := make([]string, 0, len(a.ShadowHits))
		for _, h := range a.ShadowHits {
			shadowIDs = append(shadowIDs, h.RuleID)
		}
		metaJSON, _ := json.Marshal(a.Input.Metadata)
		_ = bch.Append(
			a.DecisionID, a.OccurredAt, int32(a.RuleVersion),
			a.Input.PaymentIntentID, a.Input.MerchantID, a.Input.CustomerID,
			a.Input.Amount, a.Input.Currency, a.Input.PaymentMethod, a.Input.Country,
			a.Input.IPAddress, a.Input.DeviceID,
			a.Verdict, int32(a.RiskScore), a.RiskLevel, hitIDs, shadowIDs,
			a.MLScore, a.MLModelVer, a.EvalDurationMs, string(metaJSON),
		)
	}
	if err := bch.Send(); err != nil {
		s.logger.Warn("clickhouse audit send failed", zap.Error(err), zap.Int("batch", len(batch)))
	}
}

// Close 优雅停 — 业务层 lifecycle hook 调一下。
func (s *ClickHouseSink) Close() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	close(s.stop)
	s.mu.Unlock()
}
