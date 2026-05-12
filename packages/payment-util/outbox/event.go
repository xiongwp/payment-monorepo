package outbox

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

// Status outbox 条目状态
type Status string

const (
	StatusPending   Status = "pending"   // 待发布
	StatusPublished Status = "published" // 已发送到 Kafka
	StatusFailed    Status = "failed"    // 多次重试后放弃 (进 DLQ)
)

// Event 待发布的事件
type Event struct {
	EventID      string          `json:"event_id"`        // sha256 random; 同时是 Kafka message key 候选
	Aggregate    string          `json:"aggregate"`       // "order", "payout", "screen"
	AggregateID  string          `json:"aggregate_id"`    // 业务 ID, 给 Kafka 分区/排序
	EventType    string          `json:"event_type"`      // OrderCreated / PayoutSettled / ScreenBlocked
	Payload      json.RawMessage `json:"payload"`
	Topic        string          `json:"topic"`           // Kafka topic; 空时用 default
	Headers      map[string]string `json:"headers,omitempty"` // 透传 trace_id / correlation_id
	CreatedAt    time.Time       `json:"created_at"`
	Status       Status          `json:"status"`
	PublishedAt  time.Time       `json:"published_at,omitempty"`
	RetryCount   int             `json:"retry_count"`
	LastError    string          `json:"last_error,omitempty"`
}

// NewEvent 构造一个 pending 事件 (event_id 由内部生成).
func NewEvent(aggregate, aggregateID, eventType string, payload any) (Event, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return Event{}, err
	}
	return Event{
		EventID:     "evt_" + randHex(16),
		Aggregate:   aggregate,
		AggregateID: aggregateID,
		EventType:   eventType,
		Payload:     b,
		CreatedAt:   time.Now().UTC(),
		Status:      StatusPending,
		Headers:     map[string]string{},
	}, nil
}

// Validate 基本字段检查
func (e Event) Validate() error {
	if e.EventID == "" {
		return errors.New("outbox: event_id required")
	}
	if e.EventType == "" {
		return errors.New("outbox: event_type required")
	}
	if len(e.Payload) == 0 {
		return errors.New("outbox: payload required")
	}
	return nil
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
