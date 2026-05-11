package repository

import (
	"context"
	"sort"
	"sync"
	"time"

	"reconcile-system/packages/merchant-webhook/internal/domain"
)

// MemoryRepo dispatcher.Repository in-mem 实现。
type MemoryRepo struct {
	mu          sync.RWMutex
	endpoints   map[int64]*domain.Endpoint
	endpointBy  map[string]map[string][]*domain.Endpoint // merchantID → eventType → endpoints
	events      map[int64]*domain.Event
	deliveries  map[int64]*domain.Delivery
	nextEpID    int64
	nextEvID    int64
	nextDelID   int64
	// 内存模拟 secret 明文表（生产换 KMS Decrypt）
	secretsByEp map[int64]string
}

func NewMemoryRepo() *MemoryRepo {
	return &MemoryRepo{
		endpoints:   map[int64]*domain.Endpoint{},
		endpointBy:  map[string]map[string][]*domain.Endpoint{},
		events:      map[int64]*domain.Event{},
		deliveries:  map[int64]*domain.Delivery{},
		secretsByEp: map[int64]string{},
	}
}

// RegisterEndpoint 商户后台调，注册一个 webhook 订阅。
func (r *MemoryRepo) RegisterEndpoint(ep *domain.Endpoint, secret string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextEpID++
	ep.ID = r.nextEpID
	ep.SecretLast4 = ""
	if len(secret) >= 4 {
		ep.SecretLast4 = secret[len(secret)-4:]
	}
	r.endpoints[ep.ID] = ep
	r.secretsByEp[ep.ID] = secret
	// 建索引
	if r.endpointBy[ep.MerchantID] == nil {
		r.endpointBy[ep.MerchantID] = map[string][]*domain.Endpoint{}
	}
	for _, et := range splitCSV(ep.EventTypes) {
		r.endpointBy[ep.MerchantID][et] = append(r.endpointBy[ep.MerchantID][et], ep)
	}
	return ep.ID, nil
}

func splitCSV(s string) []string {
	out := []string{}
	cur := ""
	for _, c := range s {
		if c == ',' || c == ' ' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(c)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func (r *MemoryRepo) GetEndpoint(ctx context.Context, id int64) (*domain.Endpoint, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.endpoints[id], nil
}

func (r *MemoryRepo) GetSecretPlain(ctx context.Context, id int64) (string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.secretsByEp[id], nil
}

func (r *MemoryRepo) ListEndpointsForEvent(ctx context.Context, merchantID, eventType string) ([]*domain.Endpoint, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.endpointBy[merchantID] == nil {
		return nil, nil
	}
	return r.endpointBy[merchantID][eventType], nil
}

func (r *MemoryRepo) UpdateLastDelivery(ctx context.Context, id int64, t time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ep, ok := r.endpoints[id]; ok {
		ep.LastDeliveryAt = &t
	}
	return nil
}

func (r *MemoryRepo) SaveEvent(ctx context.Context, e *domain.Event) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextEvID++
	e.ID = r.nextEvID
	r.events[e.ID] = e
	return e.ID, nil
}

func (r *MemoryRepo) GetEvent(id int64) *domain.Event {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.events[id]
}

func (r *MemoryRepo) CreateDelivery(ctx context.Context, d *domain.Delivery) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextDelID++
	d.ID = r.nextDelID
	r.deliveries[d.ID] = d
	return d.ID, nil
}

func (r *MemoryRepo) UpdateDelivery(ctx context.Context, id int64, status domain.DeliveryStatus, fields map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.deliveries[id]
	if d == nil {
		return nil
	}
	d.Status = status
	if v, ok := fields["http_status"].(int); ok {
		d.HTTPStatus = v
	}
	if v, ok := fields["response_body"].(string); ok {
		d.ResponseBody = v
	}
	if v, ok := fields["error_message"].(string); ok {
		d.ErrorMessage = v
	}
	if v, ok := fields["duration_ms"].(int64); ok {
		d.DurationMS = int(v)
	}
	if v, ok := fields["next_retry_at"].(*time.Time); ok {
		d.NextRetryAt = v
	}
	if v, ok := fields["sent_at"].(*time.Time); ok {
		d.SentAt = v
	}
	if v, ok := fields["attempt"].(int); ok {
		d.Attempt = v
	}
	return nil
}

func (r *MemoryRepo) ListReadyToDeliver(ctx context.Context, now time.Time, limit int) ([]*domain.Delivery, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*domain.Delivery
	for _, d := range r.deliveries {
		if d.Status != domain.StatusPending && d.Status != domain.StatusFailed {
			continue
		}
		if d.NextRetryAt != nil && d.NextRetryAt.After(now) {
			continue
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *MemoryRepo) GetDelivery(ctx context.Context, id int64) (*domain.Delivery, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.deliveries[id], nil
}
