package server_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xiongwp/payment-channel/internal/channel"
	"github.com/xiongwp/payment-channel/internal/domain"
)

// server_test 包独有的内存仓储，与 service 包的 fakes_test.go 功能相同。
// _test.go 文件不能跨包导入，所以我们在这里复制一份最小可用的实现。

type inProcTxRepo struct {
	mu     sync.Mutex
	rows   []*domain.AcquirerTx
	nextID uint64
}

func newInProcTxRepo() *inProcTxRepo { return &inProcTxRepo{} }

func (r *inProcTxRepo) Insert(_ context.Context, tx *domain.AcquirerTx) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, x := range r.rows {
		if x.Adapter == tx.Adapter && x.IdempotencyKey == tx.IdempotencyKey {
			return domain.ErrIdempotentHit
		}
	}
	r.nextID++
	tx.ID = r.nextID
	cp := *tx
	r.rows = append(r.rows, &cp)
	return nil
}

func (r *inProcTxRepo) FindByIdem(_ context.Context, piID, adapter, idem string) (*domain.AcquirerTx, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, x := range r.rows {
		if x.PiID == piID && x.Adapter == adapter && x.IdempotencyKey == idem {
			cp := *x
			return &cp, nil
		}
	}
	return nil, nil
}

func (r *inProcTxRepo) FindByID(_ context.Context, piID, aqID string) (*domain.AcquirerTx, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, x := range r.rows {
		if x.PiID == piID && x.AqID == aqID {
			cp := *x
			return &cp, nil
		}
	}
	return nil, domain.ErrAcquirerTxNotFound
}

func (r *inProcTxRepo) UpdateResult(_ context.Context, piID string, id uint64, fields map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, x := range r.rows {
		if x.ID != id {
			continue
		}
		if v, ok := fields["state"].(string); ok {
			x.State = domain.AcquirerTxState(v)
		}
		if v, ok := fields["external_ref_no"].(string); ok {
			x.ExternalRefNo = v
		}
		if v, ok := fields["failure_code"].(string); ok {
			x.FailureCode = v
		}
		if v, ok := fields["response_body"].(string); ok {
			x.ResponseBody = v
		}
		return nil
	}
	return fmt.Errorf("row %d not found", id)
}

func (r *inProcTxRepo) ListPendingRetries(_ context.Context, limit int) ([]*domain.AcquirerTx, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*domain.AcquirerTx
	for _, x := range r.rows {
		if x.State == domain.AcquirerTxFailed {
			cp := *x
			out = append(out, &cp)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (r *inProcTxRepo) ListUnknownTxs(_ context.Context, limit int, _, _ time.Duration) ([]*domain.AcquirerTx, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*domain.AcquirerTx
	for _, x := range r.rows {
		if x.State == domain.AcquirerTxUnknown || x.State == domain.AcquirerTxPending {
			cp := *x
			out = append(out, &cp)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (r *inProcTxRepo) ListStuckPending(_ context.Context, limit int, _ time.Duration) ([]*domain.AcquirerTx, error) {
	return nil, nil
}

// 调用计数 adapter decorator：验证幂等回放时 adapter 真的只被调一次
type callCountingAdapter struct {
	inner channel.Adapter
	n     atomic.Int32
}

func (a *callCountingAdapter) Name() string { return a.inner.Name() }
func (a *callCountingAdapter) Charge(ctx context.Context, req *channel.ChargeRequest) (*channel.ChargeResponse, error) {
	a.n.Add(1)
	return a.inner.Charge(ctx, req)
}
func (a *callCountingAdapter) Capture(ctx context.Context, req *channel.CaptureRequest) (*channel.OpResponse, error) {
	return a.inner.Capture(ctx, req)
}
func (a *callCountingAdapter) Void(ctx context.Context, req *channel.VoidRequest) (*channel.OpResponse, error) {
	return a.inner.Void(ctx, req)
}
func (a *callCountingAdapter) Refund(ctx context.Context, req *channel.RefundRequest) (*channel.OpResponse, error) {
	return a.inner.Refund(ctx, req)
}
func (a *callCountingAdapter) Query(ctx context.Context, req *channel.QueryRequest) (*channel.QueryResponse, error) {
	return a.inner.Query(ctx, req)
}
func (a *callCountingAdapter) ParseWebhook(headers map[string]string, body []byte) (*channel.WebhookEvent, error) {
	return a.inner.ParseWebhook(headers, body)
}

func (a *callCountingAdapter) count() int32 { return a.n.Load() }
