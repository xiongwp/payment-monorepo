// Package audit 把 service.AuditEvent 异步发到 Kafka，同时本地 audit_log 表
// 兜底（Kafka 故障期保证不丢）。
//
// **分片设计**（10K TPS 后从 meta 移到 shard）：
//   - 100 张表：audit_log_00..audit_log_99，分布在 10 个 shard DB
//   - 路由 key：UserID（同 card_stored_token 一致），同 user 的所有 audit
//     都在同一 shard，取证 / 客服查全 OK
//   - UserID==0（系统级 op）走 trace_id hash 兜底；trace_id 也空走 op+timestamp
//
// **Per-shard 链式签名**：
//   - 不再单全局链。每 (db_idx, table_idx) 独立 prev_hash 链
//   - 启动期对 100 个 (db,table) 各 SELECT 1 行最大 row_hash 当 head
//   - 各 shard 的 mu 独立，避免 100 个 inserter 串行化
//   - 跨 shard 全局完整性靠 Kafka append-only canonical store + 数据湖归档
package audit

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sync"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/xiongwp/card-center/internal/service"
	"github.com/xiongwp/card-center/internal/sharding"
)

// KafkaProducer 抽象 Kafka 客户端的最小接口
type KafkaProducer interface {
	Send(topic string, key, value []byte) error
}

// ShardManager 抽象 repo.Manager 的最小接口（避免循环 import）。
type ShardManager interface {
	Shard(idx int) *gorm.DB
	Router() *sharding.Router
}

// chainKey 100 张表的 prev_hash 缓存 key。
type chainKey struct{ Db, Tbl int }

// Emitter 实现 service.AuditEmitter
type Emitter struct {
	producer KafkaProducer
	topic    string
	mgr      ShardManager
	logger   *zap.Logger

	// per-shard prev_hash：每个 (db,table) 独立链；首次 insert 前从 DB load 最大 row_hash
	mu        sync.Mutex
	chainHead map[chainKey]string
	loaded    map[chainKey]bool
}

// New 构造。mgr 为 nil → DB 落盘 disabled（dev 路径）。
func New(producer KafkaProducer, topic string, mgr ShardManager, logger *zap.Logger) *Emitter {
	return &Emitter{
		producer:  producer,
		topic:     topic,
		mgr:       mgr,
		logger:    logger,
		chainHead: make(map[chainKey]string),
		loaded:    make(map[chainKey]bool),
	}
}

// auditRow 与 audit_log_NN 表对齐。
type auditRow struct {
	ID        int64
	Op        string
	Caller    string
	CallerIP  string
	UserID    *int64
	PIID      string
	TokenHash string
	KMSKid    string
	MaskedPAN string
	Network   string
	Result    string
	Reason    string
	TraceID   string
	PrevHash  string
	RowHash   string
	CreatedAt time.Time
}

// Emit 实现 service.AuditEmitter
func (e *Emitter) Emit(ctx context.Context, ev service.AuditEvent) {
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = time.Now()
	}

	row := &auditRow{
		Op:        ev.Op,
		Caller:    ev.Caller,
		CallerIP:  ev.CallerIP,
		PIID:      ev.PIID,
		TokenHash: ev.TokenHash,
		KMSKid:    ev.KMSKid,
		MaskedPAN: ev.MaskedPAN,
		Network:   ev.Network,
		Result:    ev.Result,
		Reason:    ev.Reason,
		TraceID:   ev.TraceID,
		CreatedAt: ev.CreatedAt,
	}
	if ev.UserID != 0 {
		uid := ev.UserID
		row.UserID = &uid
	}

	// 1) 路由到 (db_idx, table_idx)
	dbIdx, tblIdx := e.route(&ev)

	// 2) 取该 shard 当前链 head（首次访问时从 DB load 最大 row_hash）
	prev := e.chainHeadFor(ctx, dbIdx, tblIdx)
	row.PrevHash = prev
	row.RowHash = computeRowHash(row)

	// 3) DB 同步插入
	if e.mgr != nil {
		db := e.mgr.Shard(dbIdx)
		tbl := e.mgr.Router().TableName(ctx, "audit_log", tblIdx)
		if db != nil {
			if err := db.WithContext(ctx).Table(tbl).Create(row).Error; err != nil {
				e.logger.Error("audit DB insert failed",
					zap.String("op", ev.Op),
					zap.String("caller", ev.Caller),
					zap.Int("db", dbIdx),
					zap.Int("tbl", tblIdx),
					zap.Error(err))
				// 链不推进（让下条接续相同 prev），保持完整性
				return
			}
		}
	}

	// 4) 推进链 head
	e.advanceChain(dbIdx, tblIdx, row.RowHash)

	// 5) Kafka 异步发（best-effort canonical store；DB 故障也不丢）
	if e.producer != nil {
		go func(r *auditRow, db, tbl int) {
			payload, err := json.Marshal(map[string]any{
				"shard_db":    db,
				"shard_table": tbl,
				"row":         r,
			})
			if err != nil {
				return
			}
			// kafka key = op + ":" + db.tbl 让同 shard 行落同 partition
			key := []byte(fmt.Sprintf("%s:%d.%d", r.Op, db, tbl))
			if err := e.producer.Send(e.topic, key, payload); err != nil {
				e.logger.Warn("audit Kafka send failed (DB has the record)", zap.Error(err))
			}
		}(row, dbIdx, tblIdx)
	}
}

// route 选 (db_idx, table_idx)。优先级：UserID > TraceID hash > Op+timestamp。
func (e *Emitter) route(ev *service.AuditEvent) (int, int) {
	r := e.mgr.Router()
	if ev.UserID != 0 {
		return r.RouteByUserID(ev.UserID)
	}
	// 系统级 op（无 user）：用 trace_id hash 保持稳定路由（同 trace 落同 shard）
	if ev.TraceID != "" {
		h := fnv.New64a()
		h.Write([]byte(ev.TraceID))
		return r.RouteByID(int64(h.Sum64() & 0x7fffffffffffffff))
	}
	// 兜底：op+timestamp 防 hot shard
	h := fnv.New64a()
	h.Write([]byte(ev.Op))
	binary.Write(h, binary.LittleEndian, ev.CreatedAt.UnixNano())
	return r.RouteByID(int64(h.Sum64() & 0x7fffffffffffffff))
}

// chainHeadFor 取 (db,tbl) 当前链 head；首次访问时从 DB load 最大 row_hash 接续。
func (e *Emitter) chainHeadFor(ctx context.Context, db, tbl int) string {
	k := chainKey{Db: db, Tbl: tbl}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.loaded[k] && e.mgr != nil {
		shard := e.mgr.Shard(db)
		tbName := e.mgr.Router().TableName(ctx, "audit_log", tbl)
		if shard != nil {
			var last struct {
				RowHash string `gorm:"column:row_hash"`
			}
			err := shard.WithContext(ctx).Table(tbName).Select("row_hash").
				Order("id DESC").Limit(1).Take(&last).Error
			if err == nil {
				e.chainHead[k] = last.RowHash
			}
		}
		e.loaded[k] = true
	}
	return e.chainHead[k]
}

// advanceChain 上一行 insert 成功后把 head 推到新 row_hash。
func (e *Emitter) advanceChain(db, tbl int, h string) {
	k := chainKey{Db: db, Tbl: tbl}
	e.mu.Lock()
	e.chainHead[k] = h
	e.loaded[k] = true
	e.mu.Unlock()
}

// computeRowHash sha256 over canonical fields；包含 prev_hash 形成链。
//
// **链的形状**：
//
//	row_N.row_hash = sha256(canonical(row_N) || row_{N-1}.row_hash)
//
// per-shard 各自独立，验证时 SELECT * FROM audit_log_NN ORDER BY id 顺序走完即可。
func computeRowHash(r *auditRow) string {
	h := sha256.New()
	type canonical struct {
		Op      string
		Caller  string
		UserID  *int64
		PIID    string
		Token   string
		KID     string
		Masked  string
		Network string
		Result  string
		Reason  string
		Trace   string
		Prev    string
		Created string
	}
	c := canonical{
		Op: r.Op, Caller: r.Caller, UserID: r.UserID,
		PIID: r.PIID, Token: r.TokenHash, KID: r.KMSKid,
		Masked: r.MaskedPAN, Network: r.Network,
		Result: r.Result, Reason: r.Reason, Trace: r.TraceID,
		Prev: r.PrevHash, Created: r.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	b, _ := json.Marshal(&c)
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}
