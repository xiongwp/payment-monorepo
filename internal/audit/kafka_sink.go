// kafka_sink.go: audit.Sink 的 Kafka 参考实现 (含 Flink 消费 schema)。
//
// **未编译进默认 build**：避免给 risk-manage 强制加 segmentio/kafka-go 依赖。
// 接入时：
//
//  1. go.mod 加：
//     require github.com/segmentio/kafka-go v0.4.x
//  2. 删除文件顶部的 //go:build kafka 标签
//  3. main.go newAuditSink 把 KafkaSink 包到 AsyncBatchSink (强烈建议)：
//
//     w := &kafka.Writer{
//         Addr: kafka.TCP("kafka:9092"),
//         Topic: "risk.decision.v1",
//         Balancer: &kafka.Hash{},
//         BatchSize: 1000,
//         BatchTimeout: 100 * time.Millisecond,
//     }
//     ks := audit.NewKafkaSink(w, logger)
//     async := audit.NewAsyncBatchSink(ks, 8192, 1000, 1*time.Second, logger)
//     return audit.MultiSink{
//         &audit.LogSink{Logger: logger},
//         audit.NewMemSink(4096),
//         async,
//     }
//
// 下游 Flink 任务消费这个 topic 算实时聚合（per-merchant 5min fraud rate、
// rolling cohort metrics、device-customer 关联图等），结果再回写 risk-manage
// 的 feature store 形成闭环。
//
// Kafka 消息 schema（JSON，每条 record 一条 message，key=merchant_id 让
// 同商户路由到同 partition 让 Flink keyed-state 高效）：
//
//	{
//	  "decision_id":      "...",
//	  "occurred_at":      "RFC3339",
//	  "merchant_id":      "...",
//	  "customer_id":      "...",
//	  "payment_intent_id":"...",
//	  "amount":           1000,
//	  "currency":         "PHP",
//	  "country":          "PH",
//	  "ip":               "1.2.3.4",
//	  "device_id":        "...",
//	  "verdict":          "DENY",
//	  "risk_score":       85,
//	  "ml_score":         0.92,
//	  "ml_model_ver":     "logistic-v1",
//	  "hit_rule_ids":     ["r1", "r2"],
//	  "shadow_rule_ids":  [],
//	  "eval_duration_ms": 12.3
//	}
//
// 推荐 Flink 任务（参考 deploy/flink/README.md）：
//   1. fraud_rate_per_merchant_5min  → 滑动窗口 + outcome label join
//   2. device_velocity                → 同 device 5min 内 N 笔不同 merchant
//   3. ip_geo_anomaly                 → 同 IP 多个 country
//   4. cohort_vintage                 → 按 first_pi_month 拉链分析



package audit

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// KafkaSink push 每条 audit record 到指定 Kafka topic。Writer 由 caller
// 配置（含 batch size / linger / acks 等 kafka-go.Writer 选项）。
type KafkaSink struct {
	w      *kafka.Writer
	logger *zap.Logger
	mu     sync.Mutex // protect w.WriteMessages call (kafka-go 已经线程安全；mu 给 close 用)
}

// NewKafkaSink 包装 kafka.Writer。
func NewKafkaSink(w *kafka.Writer, logger *zap.Logger) *KafkaSink {
	return &KafkaSink{w: w, logger: logger}
}

// kafkaPayload Kafka topic 上每条 message 的 JSON shape。比 DecisionAudit
// 更扁平 + Flink-friendly (avoid nested 深 JSON path 解析)。
type kafkaPayload struct {
	DecisionID      string   `json:"decision_id"`
	OccurredAt      string   `json:"occurred_at"`
	MerchantID      string   `json:"merchant_id"`
	CustomerID      string   `json:"customer_id"`
	PaymentIntentID string   `json:"payment_intent_id"`
	Amount          int64    `json:"amount"`
	Currency        string   `json:"currency"`
	PaymentMethod   string   `json:"payment_method"`
	Country         string   `json:"country"`
	IPAddress       string   `json:"ip"`
	DeviceID        string   `json:"device_id"`
	Verdict         string   `json:"verdict"`
	RiskScore       int      `json:"risk_score"`
	RiskLevel       string   `json:"risk_level"`
	MLScore         float64  `json:"ml_score"`
	MLModelVer      string   `json:"ml_model_ver"`
	HitRuleIDs      []string `json:"hit_rule_ids"`
	ShadowRuleIDs   []string `json:"shadow_rule_ids"`
	EvalDurationMs  float64  `json:"eval_duration_ms"`
}

func toPayload(a *DecisionAudit) kafkaPayload {
	hits := make([]string, 0, len(a.Hits))
	for _, h := range a.Hits {
		hits = append(hits, h.RuleID)
	}
	shadows := make([]string, 0, len(a.ShadowHits))
	for _, h := range a.ShadowHits {
		shadows = append(shadows, h.RuleID)
	}
	return kafkaPayload{
		DecisionID:      a.DecisionID,
		OccurredAt:      a.OccurredAt.UTC().Format("2006-01-02T15:04:05.000000000Z"),
		MerchantID:      a.Input.MerchantID,
		CustomerID:      a.Input.CustomerID,
		PaymentIntentID: a.Input.PaymentIntentID,
		Amount:          a.Input.Amount,
		Currency:        a.Input.Currency,
		PaymentMethod:   a.Input.PaymentMethod,
		Country:         a.Input.Country,
		IPAddress:       a.Input.IPAddress,
		DeviceID:        a.Input.DeviceID,
		Verdict:         a.Verdict,
		RiskScore:       a.RiskScore,
		RiskLevel:       a.RiskLevel,
		MLScore:         a.MLScore,
		MLModelVer:      a.MLModelVer,
		HitRuleIDs:      hits,
		ShadowRuleIDs:   shadows,
		EvalDurationMs:  a.EvalDurationMs,
	}
}

// Write 单条入队（推荐用 AsyncBatchSink 包一层避免主路径阻塞 Kafka produce）。
func (s *KafkaSink) Write(ctx context.Context, a *DecisionAudit) {
	if s == nil || s.w == nil || a == nil {
		return
	}
	body, err := json.Marshal(toPayload(a))
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("kafka sink marshal failed", zap.Error(err))
		}
		return
	}
	msg := kafka.Message{
		Key:   []byte(a.Input.MerchantID), // 同商户路由到同 partition
		Value: body,
	}
	if err := s.w.WriteMessages(ctx, msg); err != nil {
		if s.logger != nil {
			s.logger.Warn("kafka sink write failed", zap.Error(err))
		}
	}
}

// WriteBatch 批量 produce，匹配 BatchSink 接口让 AsyncBatchSink 直接调。
func (s *KafkaSink) WriteBatch(ctx context.Context, batch []*DecisionAudit) {
	if s == nil || s.w == nil || len(batch) == 0 {
		return
	}
	msgs := make([]kafka.Message, 0, len(batch))
	for _, a := range batch {
		body, err := json.Marshal(toPayload(a))
		if err != nil {
			continue
		}
		msgs = append(msgs, kafka.Message{
			Key:   []byte(a.Input.MerchantID),
			Value: body,
		})
	}
	if err := s.w.WriteMessages(ctx, msgs...); err != nil && s.logger != nil {
		s.logger.Warn("kafka sink batch write failed", zap.Error(err))
	}
}

// Close 关 writer。生产 main.go shutdown 钩子里调，确保 Kafka 缓冲区清空。
func (s *KafkaSink) Close() error {
	if s == nil || s.w == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Close()
}
