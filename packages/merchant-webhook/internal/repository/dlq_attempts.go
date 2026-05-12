// dlq_attempts.go — DLQ 列表 + attempt 历史 (MemoryRepo 扩展).
//
// 真生产用 MySQL: delivery_attempts 表 (append-only) + DLQ 走 status='dlq' 查.

package repository

import (
	"context"
	"sort"
	"sync"
	"time"

	"reconcile-system/packages/merchant-webhook/internal/domain"
)

// ── DLQ list + replay ──

// ListDLQ 返回所有 status=dlq 的 deliveries (按 created_at desc).
func (r *MemoryRepo) ListDLQ(ctx context.Context, limit, offset int) ([]*domain.Delivery, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	all := make([]*domain.Delivery, 0)
	for _, d := range r.deliveries {
		if d.Status == domain.StatusDLQ {
			all = append(all, d)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].CreatedAt.After(all[j].CreatedAt) })
	if offset >= len(all) {
		return nil, nil
	}
	end := offset + limit
	if end > len(all) {
		end = len(all)
	}
	return all[offset:end], nil
}

// ReplayDLQ DLQ 行重置为 pending, 重新计 attempt 从当前 attempt 开始.
//
// 调用方一般在商户报修 / fix endpoint URL 后调.
// 注意: 不重置 attempt 计数 (商户能看到"重新 from N 次"); 也不重新 backoff.
func (r *MemoryRepo) ReplayDLQ(ctx context.Context, deliveryIDs []int64) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	now := time.Now().UTC()
	for _, id := range deliveryIDs {
		d := r.deliveries[id]
		if d == nil || d.Status != domain.StatusDLQ {
			continue
		}
		d.Status = domain.StatusPending
		d.NextRetryAt = &now
		// 错误清掉但 attempt 保留 (商户能看到"我们一共试了 N 次")
		d.ErrorMessage = ""
		n++
	}
	return n, nil
}

// ── Attempt 历史 (append-only) ──

// attemptsStore 单独 mutex, 减少跟主 repo 锁竞争.
type attemptsStore struct {
	mu       sync.RWMutex
	byDel    map[int64][]*domain.Attempt
	nextID   int64
}

var attempts = &attemptsStore{byDel: map[int64][]*domain.Attempt{}}

// AppendAttempt 追加一次投递尝试 — dispatcher 每次 deliverOne 完成后调.
func (r *MemoryRepo) AppendAttempt(ctx context.Context, a *domain.Attempt) error {
	attempts.mu.Lock()
	defer attempts.mu.Unlock()
	attempts.nextID++
	a.ID = attempts.nextID
	attempts.byDel[a.DeliveryID] = append(attempts.byDel[a.DeliveryID], a)
	return nil
}

// ListAttempts 给一个 delivery 看完整历史 — 商户 DX.
func (r *MemoryRepo) ListAttempts(ctx context.Context, deliveryID int64) ([]*domain.Attempt, error) {
	attempts.mu.RLock()
	defer attempts.mu.RUnlock()
	src := attempts.byDel[deliveryID]
	out := make([]*domain.Attempt, len(src))
	copy(out, src)
	sort.Slice(out, func(i, j int) bool {
		return out[i].AttemptNum < out[j].AttemptNum
	})
	return out, nil
}
