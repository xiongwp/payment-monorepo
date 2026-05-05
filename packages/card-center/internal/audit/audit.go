// Package audit 把 service.AuditEvent 异步发到 Kafka，同时本地落 audit_log 表
// 兜底（Kafka 故障期保证不丢）。
package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/xiongwp/card-center/internal/service"
)

// KafkaProducer 抽象 Kafka 客户端的最小接口
type KafkaProducer interface {
	Send(topic string, key, value []byte) error
}

// Emitter 实现 service.AuditEmitter
type Emitter struct {
	producer KafkaProducer
	topic    string
	metaDB   *gorm.DB
	logger   *zap.Logger

	// 链式签名：每行 prev_hash = 前一行 row_hash
	mu       sync.Mutex
	lastHash string
}

// New 构造
func New(producer KafkaProducer, topic string, metaDB *gorm.DB, logger *zap.Logger) *Emitter {
	e := &Emitter{
		producer: producer,
		topic:    topic,
		metaDB:   metaDB,
		logger:   logger,
	}
	// 启动期取 audit_log 最后一行 row_hash 接续链
	if metaDB != nil {
		var last struct {
			RowHash string
		}
		if err := metaDB.Table("audit_log").Order("id DESC").Limit(1).Take(&last).Error; err == nil {
			e.lastHash = last.RowHash
		}
	}
	return e
}

// Emit 实现 service.AuditEmitter。
//
// 双路径：
//  1. Kafka producer 异步发（best-effort）
//  2. metaDB.audit_log 同步插（强一致，PCI-DSS 10.7 7 年留存）
type auditRow struct {
	ID        int64
	Op        string
	Caller    string
	CallerIP  string
	UserID    *int64
	PIID      string
	TokenHash string
	KMSKid    string
	Result    string
	Reason    string
	TraceID   string
	PrevHash  string
	RowHash   string
	CreatedAt time.Time
}

func (auditRow) TableName() string { return "audit_log" }

// Emit 实现 service.AuditEmitter
func (e *Emitter) Emit(ctx context.Context, ev service.AuditEvent) {
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = time.Now()
	}
	e.mu.Lock()
	prev := e.lastHash
	row := &auditRow{
		Op:        ev.Op,
		Caller:    ev.Caller,
		CallerIP:  ev.CallerIP,
		PIID:      ev.PIID,
		TokenHash: ev.TokenHash,
		KMSKid:    ev.KMSKid,
		Result:    ev.Result,
		Reason:    ev.Reason,
		TraceID:   ev.TraceID,
		PrevHash:  prev,
		CreatedAt: ev.CreatedAt,
	}
	if ev.UserID != 0 {
		uid := ev.UserID
		row.UserID = &uid
	}
	row.RowHash = computeRowHash(row)
	e.lastHash = row.RowHash
	e.mu.Unlock()

	// 1) DB 同步插（不能丢）
	if e.metaDB != nil {
		if err := e.metaDB.WithContext(ctx).Create(row).Error; err != nil {
			e.logger.Error("audit DB insert failed",
				zap.String("op", ev.Op),
				zap.String("caller", ev.Caller),
				zap.Error(err))
		}
	}

	// 2) Kafka 异步发（best-effort）
	if e.producer != nil {
		go func(r *auditRow) {
			payload, err := json.Marshal(r)
			if err != nil {
				return
			}
			if err := e.producer.Send(e.topic, []byte(r.Op), payload); err != nil {
				e.logger.Warn("audit Kafka send failed (DB has the record)", zap.Error(err))
			}
		}(row)
	}
}

// computeRowHash sha256 over canonical fields；包含 prev_hash 形成链。
func computeRowHash(r *auditRow) string {
	h := sha256.New()
	type canonical struct {
		Op       string
		Caller   string
		UserID   *int64
		PIID     string
		Token    string
		KID      string
		Result   string
		Reason   string
		Trace    string
		Prev     string
		Created  string
	}
	c := canonical{
		Op: r.Op, Caller: r.Caller, UserID: r.UserID,
		PIID: r.PIID, Token: r.TokenHash, KID: r.KMSKid,
		Result: r.Result, Reason: r.Reason, Trace: r.TraceID,
		Prev: r.PrevHash, Created: r.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	b, _ := json.Marshal(&c)
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}
