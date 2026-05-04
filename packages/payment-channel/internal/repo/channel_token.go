package repo

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"github.com/xiongwp/payment-channel/internal/domain"
	"github.com/xiongwp/payment-channel/internal/sharding"
)

// ChannelTokenRepository 用户在某 adapter 的长期 token（落库前加密）。
type ChannelTokenRepository interface {
	Insert(ctx context.Context, t *domain.ChannelToken) error
	FindByCustomer(ctx context.Context, piID, customerRef, adapter string) (*domain.ChannelToken, error)
}

type channelTokenRepo struct {
	mgr    *Manager
	router *sharding.Router
}

func NewChannelTokenRepository(mgr *Manager, r *sharding.Router) ChannelTokenRepository {
	return &channelTokenRepo{mgr: mgr, router: r}
}

type channelTokenRow struct {
	ID          uint64 `gorm:"column:id;primaryKey"`
	PiID        string `gorm:"column:pi_id"`
	CustomerRef string `gorm:"column:customer_ref"`
	Adapter     string `gorm:"column:adapter"`
	Token       string `gorm:"column:token"`
	Brand       string `gorm:"column:brand"`
	Last4       string `gorm:"column:last4"`
	Status      string `gorm:"column:status"`
	ExpiresAt   *string `gorm:"column:expires_at"`
	CreatedAt   string `gorm:"column:created_at;default:CURRENT_TIMESTAMP;->"`
}

func (r *channelTokenRepo) table(ctx context.Context, piID string) (*gorm.DB, string, error) {
	db, tblIdx := r.router.RouteByPrefixedID(piID)
	shard, err := r.mgr.GetShard(db)
	if err != nil {
		return nil, "", err
	}
	return shard, r.router.TableName(ctx, "channel_token", tblIdx), nil
}

func (r *channelTokenRepo) Insert(ctx context.Context, t *domain.ChannelToken) error {
	shard, tbl, err := r.table(ctx, t.PiID)
	if err != nil {
		return err
	}
	row := channelTokenRow{
		PiID:        t.PiID,
		CustomerRef: t.CustomerRef,
		Adapter:     t.Adapter,
		Token:       t.Token,
		Brand:       t.Brand,
		Last4:       t.Last4,
		Status:      t.Status,
	}
	return shard.WithContext(ctx).Table(tbl).Create(&row).Error
}

func (r *channelTokenRepo) FindByCustomer(ctx context.Context, piID, cust, adapter string) (*domain.ChannelToken, error) {
	shard, tbl, err := r.table(ctx, piID)
	if err != nil {
		return nil, err
	}
	var row channelTokenRow
	err = shard.WithContext(ctx).Table(tbl).
		Where("customer_ref = ? AND adapter = ? AND status = ?", cust, adapter, "active").
		Order("id DESC").First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &domain.ChannelToken{
		ID:          row.ID,
		PiID:        row.PiID,
		CustomerRef: row.CustomerRef,
		Adapter:     row.Adapter,
		Token:       row.Token,
		Brand:       row.Brand,
		Last4:       row.Last4,
		Status:      row.Status,
	}, nil
}
