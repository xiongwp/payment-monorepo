package service

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xiongwp/payment-channel/internal/channel"
	"github.com/xiongwp/payment-channel/internal/domain"
)

// 本文件提供内存版仓储 + adapter，仅在 _test.go 文件中使用。
// e2e 测试不依赖 MySQL；生产代码不变。

// ─── in-memory AcquirerTxRepository ────────────────────────────────

type memTxRepo struct {
	mu     sync.Mutex
	rows   []*domain.AcquirerTx
	nextID uint64
}

func newMemTxRepo() *memTxRepo { return &memTxRepo{} }

func (r *memTxRepo) Insert(_ context.Context, tx *domain.AcquirerTx) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, x := range r.rows {
		if x.Adapter == tx.Adapter && x.IdempotencyKey == tx.IdempotencyKey {
			return domain.ErrIdempotentHit
		}
	}
	r.nextID++
	tx.ID = r.nextID
	// 存副本，避免外层继续修改后污染
	cp := *tx
	r.rows = append(r.rows, &cp)
	return nil
}

func (r *memTxRepo) FindByIdem(_ context.Context, piID, adapter, idem string) (*domain.AcquirerTx, error) {
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

func (r *memTxRepo) FindByID(_ context.Context, piID, aqID string) (*domain.AcquirerTx, error) {
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

func (r *memTxRepo) UpdateResult(_ context.Context, piID string, id uint64, fields map[string]any) error {
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
		if v, ok := fields["raw_failure_code"].(string); ok {
			x.RawFailureCode = v
		}
		if v, ok := fields["response_body"].(string); ok {
			x.ResponseBody = v
		}
		if v, ok := fields["latency_ms"].(int); ok {
			x.LatencyMs = v
		}
		if v, ok := fields["query_count"].(int); ok {
			x.QueryCount = v
		}
		if v, ok := fields["last_query_at"].(time.Time); ok {
			t := v
			x.LastQueryAt = &t
		}
		return nil
	}
	return fmt.Errorf("row %d not found", id)
}

func (r *memTxRepo) ListPendingRetries(_ context.Context, limit int) ([]*domain.AcquirerTx, error) {
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

func (r *memTxRepo) ListUnknownTxs(_ context.Context, limit int, _, _ time.Duration) ([]*domain.AcquirerTx, error) {
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

func (r *memTxRepo) ListStuckPending(_ context.Context, limit int, _ time.Duration) ([]*domain.AcquirerTx, error) {
	return nil, nil
}

func (r *memTxRepo) rowsByAdapter(adapter string) []*domain.AcquirerTx {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*domain.AcquirerTx
	for _, x := range r.rows {
		if x.Adapter == adapter {
			cp := *x
			out = append(out, &cp)
		}
	}
	return out
}

// ─── in-memory WebhookRawRepository ────────────────────────────────

type memWebhookRepo struct {
	mu    sync.Mutex
	rows  []*domain.WebhookRaw
	seen  map[string]bool
	next  uint64
}

func newMemWebhookRepo() *memWebhookRepo { return &memWebhookRepo{seen: map[string]bool{}} }

func (r *memWebhookRepo) Insert(_ context.Context, w *domain.WebhookRaw) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen[w.DedupeKey] {
		return domain.ErrWebhookAlreadySeen
	}
	r.seen[w.DedupeKey] = true
	r.next++
	w.ID = r.next
	cp := *w
	r.rows = append(r.rows, &cp)
	return nil
}

func (r *memWebhookRepo) MarkForwarded(_ context.Context, piID string, id uint64, ferr error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, x := range r.rows {
		if x.ID == id {
			x.Forwarded = ferr == nil
			if ferr != nil {
				x.ForwardErr = ferr.Error()
			}
			return nil
		}
	}
	return fmt.Errorf("wh %d not found", id)
}

func (r *memWebhookRepo) ListUnforwarded(_ context.Context, limit int) ([]*domain.WebhookRaw, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*domain.WebhookRaw
	for _, x := range r.rows {
		if !x.Forwarded && x.SignatureOK {
			cp := *x
			out = append(out, &cp)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (r *memWebhookRepo) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.rows)
}

// ─── in-memory IDIssuer ────────────────────────────────────────────

type memIDIssuer struct{ n atomic.Int64 }

func (m *memIDIssuer) Next(prefix, piID string) (string, error) {
	return fmt.Sprintf("%s_%s_%d", prefix, piID, m.n.Add(1)), nil
}

// ─── fake channel.Adapter ───────────────────────────────────────────
//
// 通过 scripted response + 可注入错误，测出 AcquirerService 的编排细节：
// 落盘 → 调用 → 写回 → 幂等回放 → 重试入队。

type fakeAdapter struct {
	name string

	mu          sync.Mutex
	charges     int
	captures    int
	voids       int
	refunds     int
	queries     int
	webhookSigOK bool
	parseErr    error

	// 可由测试设置：下一次 Charge 返回内容；nil 表示成功
	nextChargeResp *channel.ChargeResponse
	nextChargeErr  error

	nextOpResp *channel.OpResponse
	nextOpErr  error

	nextQueryResp *channel.QueryResponse

	webhookEvent *channel.WebhookEvent
}

func newFakeAdapter(name string) *fakeAdapter {
	return &fakeAdapter{name: name, webhookSigOK: true}
}

func (a *fakeAdapter) Name() string { return a.name }

func (a *fakeAdapter) Charge(_ context.Context, req *channel.ChargeRequest) (*channel.ChargeResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.charges++
	if a.nextChargeErr != nil {
		err := a.nextChargeErr
		a.nextChargeErr = nil
		return nil, err
	}
	if a.nextChargeResp != nil {
		r := a.nextChargeResp
		a.nextChargeResp = nil
		return r, nil
	}
	return &channel.ChargeResponse{
		Result:        channel.ResultSucceeded,
		ExternalRefNo: "ref_" + req.PiID,
	}, nil
}

func (a *fakeAdapter) Capture(_ context.Context, req *channel.CaptureRequest) (*channel.OpResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.captures++
	return a.nextOpOrDefault(req.ExternalRefNo)
}

func (a *fakeAdapter) Void(_ context.Context, req *channel.VoidRequest) (*channel.OpResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.voids++
	return a.nextOpOrDefault(req.ExternalRefNo)
}

func (a *fakeAdapter) Refund(_ context.Context, req *channel.RefundRequest) (*channel.OpResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refunds++
	return a.nextOpOrDefault("rfd_" + req.PiID)
}

func (a *fakeAdapter) Query(_ context.Context, req *channel.QueryRequest) (*channel.QueryResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.queries++
	if a.nextQueryResp != nil {
		r := a.nextQueryResp
		a.nextQueryResp = nil
		return r, nil
	}
	return &channel.QueryResponse{Result: channel.ResultSucceeded, ExternalRefNo: req.ExternalRefNo}, nil
}

func (a *fakeAdapter) ParseWebhook(_ map[string]string, _ []byte) (*channel.WebhookEvent, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.parseErr != nil {
		return nil, a.parseErr
	}
	if a.webhookEvent != nil {
		evt := *a.webhookEvent
		evt.SignatureOK = a.webhookSigOK
		return &evt, nil
	}
	return &channel.WebhookEvent{
		EventID:     "evt_1",
		EventType:   "charge.succeeded",
		PiID:        "pi_1",
		SignatureOK: a.webhookSigOK,
	}, nil
}

func (a *fakeAdapter) nextOpOrDefault(ref string) (*channel.OpResponse, error) {
	if a.nextOpErr != nil {
		err := a.nextOpErr
		a.nextOpErr = nil
		return nil, err
	}
	if a.nextOpResp != nil {
		r := a.nextOpResp
		a.nextOpResp = nil
		return r, nil
	}
	return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: ref}, nil
}

func (a *fakeAdapter) counts() (charges, captures, voids, refunds, queries int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.charges, a.captures, a.voids, a.refunds, a.queries
}

// ─── tiny helpers ────────────────────────────────────────────────

func newFakeRegistry(adapters ...channel.Adapter) channel.Registry {
	reg := channel.NewRegistry()
	for _, a := range adapters {
		reg.Register(a)
	}
	return reg
}
