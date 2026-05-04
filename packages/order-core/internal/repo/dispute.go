package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/xiongwp/order-core/internal/domain"
	"github.com/xiongwp/order-core/internal/sharding"
)

// DisputeRepository dispute_XX + dispute_event_XX 分片表，按 payment_intent_id 路由。
type DisputeRepository interface {
	Create(ctx context.Context, d *domain.Dispute) error
	Get(ctx context.Context, piID, id string) (*domain.Dispute, error)
	GetByChannelDisputeID(ctx context.Context, piID, channel, channelDisputeID string) (*domain.Dispute, error)
	ListByPI(ctx context.Context, piID string) ([]*domain.Dispute, error)
	ListByMerchant(ctx context.Context, merchantID string, status domain.DisputeStatus, limit, offset int) ([]*domain.Dispute, int64, error)
	UpdateFields(ctx context.Context, piID, id string, fields map[string]any) (*domain.Dispute, error)

	// Transition + log event in a single transaction.
	Transition(ctx context.Context, piID, id string, from, to domain.DisputeStatus, evt *domain.DisputeEvent) (*domain.Dispute, error)

	// Events
	ListEvents(ctx context.Context, piID, disputeID string, limit int) ([]*domain.DisputeEvent, error)
}

type disputeRepo struct {
	mgr    *Manager
	router *sharding.Router
}

// NewDisputeRepository 构造
func NewDisputeRepository(mgr *Manager, r *sharding.Router) DisputeRepository {
	return &disputeRepo{mgr: mgr, router: r}
}

func (r *disputeRepo) shard(piID string) (*gorm.DB, string, string, error) {
	dbIdx, tblIdx := r.router.RouteByPrefixedID(piID)
	db, err := r.mgr.GetShard(dbIdx)
	if err != nil {
		return nil, "", "", err
	}
	return db, r.router.GetTableName("dispute", tblIdx), r.router.GetTableName("dispute_event", tblIdx), nil
}

// ─── CRUD ────────────────────────────────────────────────────────────────────

func (r *disputeRepo) Create(ctx context.Context, d *domain.Dispute) error {
	if d.PaymentIntentID == "" || d.ChargeID == "" || d.ID == "" {
		return fmt.Errorf("%w: id + payment_intent_id + charge_id required", domain.ErrValidation)
	}
	if d.Currency == "" {
		d.Currency = "PHP"
	}
	if d.Status == "" {
		d.Status = domain.DisputeNeedsResponse
	}
	db, tbl, _, err := r.shard(d.PaymentIntentID)
	if err != nil {
		return err
	}
	return db.WithContext(ctx).Table(tbl).Create(d).Error
}

func (r *disputeRepo) Get(ctx context.Context, piID, id string) (*domain.Dispute, error) {
	db, tbl, _, err := r.shard(piID)
	if err != nil {
		return nil, err
	}
	var d domain.Dispute
	err = db.WithContext(ctx).Table(tbl).Where("id = ?", id).First(&d).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrDisputeNotFound
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func (r *disputeRepo) GetByChannelDisputeID(ctx context.Context, piID, channel, channelDisputeID string) (*domain.Dispute, error) {
	db, tbl, _, err := r.shard(piID)
	if err != nil {
		return nil, err
	}
	var d domain.Dispute
	err = db.WithContext(ctx).Table(tbl).
		Where("channel = ? AND channel_dispute_id = ?", channel, channelDisputeID).
		First(&d).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrDisputeNotFound
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func (r *disputeRepo) ListByPI(ctx context.Context, piID string) ([]*domain.Dispute, error) {
	db, tbl, _, err := r.shard(piID)
	if err != nil {
		return nil, err
	}
	var out []*domain.Dispute
	err = db.WithContext(ctx).Table(tbl).
		Where("payment_intent_id = ?", piID).
		Order("created DESC").Find(&out).Error
	return out, err
}

// ListByMerchant fan-outs across all shards (limit is per-shard, effectively
// capped at 10 × limit). Admin-side tool; not a hot path.
func (r *disputeRepo) ListByMerchant(ctx context.Context, merchantID string, status domain.DisputeStatus, limit, offset int) ([]*domain.Dispute, int64, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	// Scan every shard and merge. Good enough for admin ops on the scale of
	// hundreds of disputes per merchant; a materialized view in meta could
	// replace this later if volume grows.
	var all []*domain.Dispute
	var total int64
	for i := 0; i < r.router.DBCount(); i++ {
		db, err := r.mgr.GetShard(i)
		if err != nil {
			return nil, 0, err
		}
		for t := 0; t < r.router.TablePerDB(); t++ {
			tbl := r.router.GetTableName("dispute", i*r.router.TablePerDB()+t)
			q := db.WithContext(ctx).Table(tbl).Where("merchant_id = ?", merchantID)
			if status != "" {
				q = q.Where("status = ?", status)
			}
			var shardCount int64
			if err := q.Count(&shardCount).Error; err != nil {
				return nil, 0, err
			}
			total += shardCount
			var rows []*domain.Dispute
			if err := q.Order("created DESC").Limit(limit).Find(&rows).Error; err != nil {
				return nil, 0, err
			}
			all = append(all, rows...)
		}
	}
	// Sort global by created desc; simple O(n log n) is fine here.
	sortDisputesByCreatedDesc(all)
	if offset >= len(all) {
		return nil, total, nil
	}
	end := offset + limit
	if end > len(all) {
		end = len(all)
	}
	return all[offset:end], total, nil
}

func (r *disputeRepo) UpdateFields(ctx context.Context, piID, id string, fields map[string]any) (*domain.Dispute, error) {
	if len(fields) == 0 {
		return r.Get(ctx, piID, id)
	}
	db, tbl, _, err := r.shard(piID)
	if err != nil {
		return nil, err
	}
	if err := db.WithContext(ctx).Table(tbl).Where("id = ?", id).Updates(fields).Error; err != nil {
		return nil, err
	}
	return r.Get(ctx, piID, id)
}

// Transition compare-and-swap status + append event row in a single tx.
func (r *disputeRepo) Transition(ctx context.Context, piID, id string, from, to domain.DisputeStatus, evt *domain.DisputeEvent) (*domain.Dispute, error) {
	if !from.CanTransition(to) {
		return nil, fmt.Errorf("%w: %s → %s", domain.ErrDisputeInvalidTransition, from, to)
	}
	db, tbl, evtTbl, err := r.shard(piID)
	if err != nil {
		return nil, err
	}
	var updated *domain.Dispute
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Table(tbl).Where("id = ? AND status = ?", id, from).
			Updates(map[string]any{"status": to})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			var cur domain.Dispute
			if err := tx.Table(tbl).Where("id = ?", id).First(&cur).Error; errors.Is(err, gorm.ErrRecordNotFound) {
				return domain.ErrDisputeNotFound
			} else if err != nil {
				return err
			}
			return fmt.Errorf("%w: current=%s, tried from=%s", domain.ErrDisputeInvalidTransition, cur.Status, from)
		}
		if evt == nil {
			evt = &domain.DisputeEvent{}
		}
		evt.DisputeID = id
		evt.PaymentIntentID = piID
		evt.FromStatus = string(from)
		evt.ToStatus = string(to)
		if evt.Source == "" {
			evt.Source = "system"
		}
		if err := tx.Table(evtTbl).Create(evt).Error; err != nil {
			return err
		}
		var d domain.Dispute
		if err := tx.Table(tbl).Where("id = ?", id).First(&d).Error; err != nil {
			return err
		}
		updated = &d
		return nil
	})
	return updated, err
}

func (r *disputeRepo) ListEvents(ctx context.Context, piID, disputeID string, limit int) ([]*domain.DisputeEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	db, _, evtTbl, err := r.shard(piID)
	if err != nil {
		return nil, err
	}
	var out []*domain.DisputeEvent
	err = db.WithContext(ctx).Table(evtTbl).
		Where("dispute_id = ?", disputeID).
		Order("created DESC").Limit(limit).Find(&out).Error
	return out, err
}

// sortDisputesByCreatedDesc in-place insertion sort (N is small for admin fan-out).
func sortDisputesByCreatedDesc(xs []*domain.Dispute) {
	for i := 1; i < len(xs); i++ {
		j := i
		for j > 0 && xs[j].Created.After(xs[j-1].Created) {
			xs[j], xs[j-1] = xs[j-1], xs[j]
			j--
		}
	}
	_ = time.Now // silence
}
