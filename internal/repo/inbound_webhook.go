package repo

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"github.com/xiongwp/order-core/internal/domain"
	"github.com/xiongwp/order-core/internal/sharding"
)

// InboundWebhookRepository 入站 webhook 去重日志仓储
type InboundWebhookRepository interface {
	// Insert 尝试落库；若 (channel_name, event_id) 已存在，返回 ErrInboundWebhookDuplicate
	// 并附带已有的记录以便调用方决定是否需要重新分发处理。
	Insert(ctx context.Context, w *domain.InboundWebhook) (*domain.InboundWebhook, error)
	// MarkProcessed 处理完成后回填状态 + processed_at + 可选 error_msg
	MarkProcessed(ctx context.Context, w *domain.InboundWebhook, status domain.InboundWebhookProcessStatus, errMsg string) error
	// GetByEvent 根据 (channel, event_id) 查找
	GetByEvent(ctx context.Context, channel, eventID, piHint string) (*domain.InboundWebhook, error)
}

type inboundWebhookRepo struct {
	mgr    *Manager
	router *sharding.Router
}

// NewInboundWebhookRepository 构造
func NewInboundWebhookRepository(mgr *Manager, r *sharding.Router) InboundWebhookRepository {
	return &inboundWebhookRepo{mgr: mgr, router: r}
}

// shardOf 路由：优先按 piID（与 PI 同分片），没有 piID 时按 (channel,event_id) 哈希。
// 同一事件必须永远落在同一分片（否则唯一约束失效），所以 channel/event_id 哈希在
// piID 缺失时是 deterministic 的兜底。
func (r *inboundWebhookRepo) shardOf(channel, eventID, piID string) (*gorm.DB, string, error) {
	var dbIdx, tblIdx int
	if piID != "" {
		dbIdx, tblIdx = r.router.RouteByPrefixedID(piID)
	} else {
		h := fnv.New64a()
		_, _ = h.Write([]byte(channel))
		_, _ = h.Write([]byte{'|'})
		_, _ = h.Write([]byte(eventID))
		sum := h.Sum64()
		dbIdx = int(sum % uint64(r.router.DBCount()))
		tblIdx = int((sum / uint64(r.router.DBCount())) % uint64(r.router.TablePerDB()))
	}
	db, err := r.mgr.GetShard(dbIdx)
	if err != nil {
		return nil, "", err
	}
	return db, r.router.GetTableName("inbound_webhook", tblIdx), nil
}

func (r *inboundWebhookRepo) Insert(ctx context.Context, w *domain.InboundWebhook) (*domain.InboundWebhook, error) {
	if w.ChannelName == "" || w.EventID == "" {
		return nil, fmt.Errorf("%w: channel_name and event_id required", domain.ErrValidation)
	}
	if w.ProcessStatus == "" {
		w.ProcessStatus = domain.InboundWebhookPending
	}
	db, tbl, err := r.shardOf(w.ChannelName, w.EventID, w.PaymentIntentID)
	if err != nil {
		return nil, err
	}
	err = db.WithContext(ctx).Table(tbl).Create(w).Error
	if err == nil {
		return w, nil
	}
	// MySQL duplicate-key (errno 1062) → look up existing row, return it tagged duplicate.
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
		existing, getErr := r.GetByEvent(ctx, w.ChannelName, w.EventID, w.PaymentIntentID)
		if getErr != nil {
			return nil, fmt.Errorf("dedupe lookup after dup: %w (orig: %v)", getErr, err)
		}
		return existing, domain.ErrInboundWebhookDuplicate
	}
	// gorm sometimes wraps it differently; fall back to substring sniff.
	if strings.Contains(err.Error(), "Duplicate entry") {
		existing, getErr := r.GetByEvent(ctx, w.ChannelName, w.EventID, w.PaymentIntentID)
		if getErr != nil {
			return nil, fmt.Errorf("dedupe lookup after dup: %w (orig: %v)", getErr, err)
		}
		return existing, domain.ErrInboundWebhookDuplicate
	}
	return nil, err
}

func (r *inboundWebhookRepo) GetByEvent(ctx context.Context, channel, eventID, piHint string) (*domain.InboundWebhook, error) {
	db, tbl, err := r.shardOf(channel, eventID, piHint)
	if err != nil {
		return nil, err
	}
	var out domain.InboundWebhook
	err = db.WithContext(ctx).Table(tbl).
		Where("channel_name = ? AND event_id = ?", channel, eventID).
		First(&out).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("inbound webhook not found: %s/%s", channel, eventID)
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (r *inboundWebhookRepo) MarkProcessed(ctx context.Context, w *domain.InboundWebhook, status domain.InboundWebhookProcessStatus, errMsg string) error {
	db, tbl, err := r.shardOf(w.ChannelName, w.EventID, w.PaymentIntentID)
	if err != nil {
		return err
	}
	updates := map[string]any{
		"process_status": status,
		"processed_at":   gorm.Expr("CURRENT_TIMESTAMP(3)"),
	}
	if errMsg != "" {
		updates["error_msg"] = errMsg
	}
	return db.WithContext(ctx).Table(tbl).
		Where("channel_name = ? AND event_id = ?", w.ChannelName, w.EventID).
		Updates(updates).Error
}
