package repo

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/xiongwp/payment-channel/internal/domain"
	"github.com/xiongwp/payment-channel/internal/sharding"
)

// WebhookRawRepository 原始 webhook 幂等表。
type WebhookRawRepository interface {
	Insert(ctx context.Context, w *domain.WebhookRaw) error
	MarkForwarded(ctx context.Context, piID string, id uint64, err error) error
	ListUnforwarded(ctx context.Context, limit int) ([]*domain.WebhookRaw, error)
}

type webhookRawRepo struct {
	mgr    *Manager
	router *sharding.Router
}

func NewWebhookRawRepository(mgr *Manager, r *sharding.Router) WebhookRawRepository {
	return &webhookRawRepo{mgr: mgr, router: r}
}

type webhookRawRow struct {
	ID          uint64 `gorm:"column:id;primaryKey"`
	PiID        string `gorm:"column:pi_id"`
	Adapter     string `gorm:"column:adapter"`
	EventID     string `gorm:"column:event_id"`
	EventType   string `gorm:"column:event_type"`
	DedupeKey   string `gorm:"column:dedupe_key"`
	SignatureOK bool   `gorm:"column:signature_ok"`
	Forwarded   bool   `gorm:"column:forwarded"`
	ForwardErr  string `gorm:"column:forward_err"`
	Headers     string `gorm:"column:headers"`
	Body        string `gorm:"column:body"`
	ReceivedAt  string `gorm:"column:received_at"`
	ForwardedAt *string `gorm:"column:forwarded_at"`
}

func (r *webhookRawRepo) table(ctx context.Context, piID string) (*gorm.DB, string, error) {
	db, tblIdx := r.router.RouteByPrefixedID(piID)
	shard, err := r.mgr.GetShard(db)
	if err != nil {
		return nil, "", err
	}
	return shard, r.router.TableName(ctx, "webhook_raw", tblIdx), nil
}

func (r *webhookRawRepo) Insert(ctx context.Context, w *domain.WebhookRaw) error {
	shard, tbl, err := r.table(ctx, w.PiID)
	if err != nil {
		return err
	}
	hdr, _ := json.Marshal(w.Headers)
	row := webhookRawRow{
		PiID:        w.PiID,
		Adapter:     w.Adapter,
		EventID:     w.EventID,
		EventType:   w.EventType,
		DedupeKey:   w.DedupeKey,
		SignatureOK: w.SignatureOK,
		Headers:     string(hdr),
		Body:        w.Body,
		ReceivedAt:  w.ReceivedAt.UTC().Format("2006-01-02 15:04:05"),
	}
	res := shard.WithContext(ctx).Table(tbl).Create(&row)
	if res.Error != nil {
		if isDup(res.Error) {
			return domain.ErrWebhookAlreadySeen
		}
		return res.Error
	}
	w.ID = row.ID
	return nil
}

func (r *webhookRawRepo) MarkForwarded(ctx context.Context, piID string, id uint64, ferr error) error {
	shard, tbl, err := r.table(ctx, piID)
	if err != nil {
		return err
	}
	fields := map[string]any{
		"forwarded":    ferr == nil,
		"forwarded_at": time.Now().UTC().Format("2006-01-02 15:04:05"),
	}
	if ferr != nil {
		msg := ferr.Error()
		if len(msg) > 240 {
			msg = msg[:240]
		}
		fields["forward_err"] = msg
	}
	return shard.WithContext(ctx).Table(tbl).Where("id = ?", id).Updates(fields).Error
}

// ListUnforwarded 扫全分片。
func (r *webhookRawRepo) ListUnforwarded(ctx context.Context, limit int) ([]*domain.WebhookRaw, error) {
	if limit <= 0 {
		limit = 200
	}
	var out []*domain.WebhookRaw
	for _, pair := range r.router.AllShards() {
		dbIdx, tblIdx := pair[0], pair[1]
		shard, err := r.mgr.GetShard(dbIdx)
		if err != nil {
			continue
		}
		tbl := r.router.TableName(ctx, "webhook_raw", tblIdx)
		var rows []webhookRawRow
		if err := shard.WithContext(ctx).Table(tbl).
			Where("forwarded = ? AND signature_ok = ?", false, true).
			Order("received_at ASC").Limit(limit).Find(&rows).Error; err != nil {
			return nil, err
		}
		for i := range rows {
			out = append(out, whFromRow(&rows[i]))
			if len(out) >= limit {
				return out, nil
			}
		}
	}
	return out, nil
}

func whFromRow(row *webhookRawRow) *domain.WebhookRaw {
	w := &domain.WebhookRaw{
		ID:          row.ID,
		PiID:        row.PiID,
		Adapter:     row.Adapter,
		EventID:     row.EventID,
		EventType:   row.EventType,
		DedupeKey:   row.DedupeKey,
		SignatureOK: row.SignatureOK,
		Forwarded:   row.Forwarded,
		ForwardErr:  row.ForwardErr,
		Body:        row.Body,
		RawHeaders:  row.Headers, // wave L: defer JSON decode to domain.Headers() accessor
	}
	if t, err := time.Parse("2006-01-02 15:04:05", row.ReceivedAt); err == nil {
		w.ReceivedAt = t
	}
	_ = errors.New // silence
	return w
}
