// cached_payment_intent.go CACHE-3: PaymentIntent 仓储装饰器, 加 read-through 缓存.
//
// 设计:
//   - 装饰器模式: WrapWithCache(inner, cache) 返回同样实现 PaymentIntentRepository
//     的对象. 写路径 (Create/Update*) 主动失效缓存; 读路径 (Get) 走 read-through.
//   - GetByIdempotencyKey 不缓存 — 业务侧只在 PI 创建时短暂查一次, 缓存收益低.
//   - List / ListExpired 不缓存 — 分页 + 排序变化太频繁, 缓存命中率低.
//   - cache 为 nil → 透传 inner, 老路径不受影响.

package repo

import (
	"context"
	"time"

	"github.com/xiongwp/payment-util/cachelib"

	"github.com/xiongwp/order-core/internal/domain"
)

// PICacheTTL PaymentIntent 缓存默认 TTL.
//
// 短一些防止 stale: PI 状态频繁变化 (created → processing → succeeded), TTL
// 长会让接口返过期状态. UpdateStatus / UpdateFields 主动失效兜底, TTL 只是漂移
// 防护. 5min 在 sweet spot.
const PICacheTTL = 5 * time.Minute

// cachedPaymentIntentRepo 装饰器.
type cachedPaymentIntentRepo struct {
	inner PaymentIntentRepository
	cache cachelib.Cache
}

// WrapPaymentIntentWithCache 把 PaymentIntent 仓储包一层 read-through 缓存.
//
// cache nil → 透传 inner 不做任何包裹 (向后兼容).
func WrapPaymentIntentWithCache(inner PaymentIntentRepository, cache cachelib.Cache) PaymentIntentRepository {
	if cache == nil {
		return inner
	}
	return &cachedPaymentIntentRepo{inner: inner, cache: cache}
}

func piCacheKey(id string) string { return "order-core:pi:" + id }

// Create 写路径 — 不预热 cache (避免热点冷启动惩罚); 由后续 Get 自然回填.
func (r *cachedPaymentIntentRepo) Create(ctx context.Context, pi *domain.PaymentIntent) error {
	return r.inner.Create(ctx, pi)
}

// Get read-through: cache miss → DB → 回填.
//
// 用 cachelib.ReadThrough 做 singleflight 防雪崩.
func (r *cachedPaymentIntentRepo) Get(ctx context.Context, id string) (*domain.PaymentIntent, error) {
	return cachelib.ReadThrough[*domain.PaymentIntent](ctx, r.cache, piCacheKey(id),
		func(ctx context.Context) (*domain.PaymentIntent, error) {
			return r.inner.Get(ctx, id)
		},
		cachelib.ReadThroughOptions{TTL: PICacheTTL},
	)
}

// GetByIdempotencyKey 不缓存 — 调用频次低 + key 命名复杂(三元组), 透传.
func (r *cachedPaymentIntentRepo) GetByIdempotencyKey(ctx context.Context, mchID, businessID, key string) (*domain.PaymentIntent, error) {
	return r.inner.GetByIdempotencyKey(ctx, mchID, businessID, key)
}

// UpdateStatus 写路径 — 主动失效 cache, 让下次 Get 拿最新行.
func (r *cachedPaymentIntentRepo) UpdateStatus(ctx context.Context, id string, from, to domain.PaymentIntentStatus, mutate func(*domain.PaymentIntent)) (*domain.PaymentIntent, error) {
	pi, err := r.inner.UpdateStatus(ctx, id, from, to, mutate)
	// CAS 失败也 invalidate — stale entry 可能造成下次幂等检查走旧值.
	_ = cachelib.Invalidate(ctx, r.cache, piCacheKey(id))
	return pi, err
}

// UpdateFields 写路径同上.
func (r *cachedPaymentIntentRepo) UpdateFields(ctx context.Context, id string, fields map[string]any) (*domain.PaymentIntent, error) {
	pi, err := r.inner.UpdateFields(ctx, id, fields)
	_ = cachelib.Invalidate(ctx, r.cache, piCacheKey(id))
	return pi, err
}

// List 不缓存 — 列表场景多变.
func (r *cachedPaymentIntentRepo) List(ctx context.Context, mchID string, page, size int) ([]*domain.PaymentIntent, int64, error) {
	return r.inner.List(ctx, mchID, page, size)
}

// ListExpired 不缓存 — 仅 cron worker 调, 一次性扫.
func (r *cachedPaymentIntentRepo) ListExpired(ctx context.Context, now time.Time, limit int) ([]*domain.PaymentIntent, error) {
	return r.inner.ListExpired(ctx, now, limit)
}
