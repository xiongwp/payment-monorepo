// Package service 编排 adapter 调用与 acquirer_tx 持久化。
//
// 核心不变量：**save-first-then-call**。
//   1. 先在 acquirer_tx 落 pending 行（带 UNIQUE(adapter, idempotency_key) 兜底幂等）；
//   2. 再调 adapter；
//   3. 返回后 UPDATE 行（succeeded / failed）+ 写响应 snapshot。
//
// 如果第 1 步写 UNIQUE 冲突 → 说明这是同一 idempotency_key 的重试，读旧行
// 直接回放，**不向第三方重复调用**。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/xiongwp/payment-channel/internal/channel"
	"github.com/xiongwp/payment-channel/internal/domain"
)

// AcquirerTxRepo 被 internal/repo 实现。
type AcquirerTxRepo interface {
	Insert(ctx context.Context, tx *domain.AcquirerTx) error
	FindByIdem(ctx context.Context, adapter, idem string) (*domain.AcquirerTx, error)
	UpdateResult(ctx context.Context, id uint64, fields map[string]any) error
}

type WebhookRepo interface {
	Insert(ctx context.Context, w *domain.WebhookRaw) error // UNIQUE(dedupe_key) 兜底
	MarkForwarded(ctx context.Context, id uint64, err error) error
}

// Registry 按 adapter 名字查找。
type Registry interface {
	Get(name string) (channel.Adapter, bool)
}

type IDGen interface {
	Next(prefix string, piID string) (string, error) // aq_<db><tbl><seq>
}

type AcquirerService struct {
	reg      Registry
	txRepo   AcquirerTxRepo
	whRepo   WebhookRepo
	idgen    IDGen
	now      func() time.Time
}

func NewAcquirerService(reg Registry, txRepo AcquirerTxRepo, whRepo WebhookRepo, idgen IDGen) *AcquirerService {
	return &AcquirerService{reg: reg, txRepo: txRepo, whRepo: whRepo, idgen: idgen, now: time.Now}
}

// Charge / Refund / Capture / Void / Query 各自走相同的 orchestrate 流程，区别
// 仅在于调用的 adapter 方法。这里给出 Charge，其他 4 个完全同构 —— 见底部。

func (s *AcquirerService) Charge(ctx context.Context, adapterName string, req *channel.ChargeRequest) (*channel.ChargeResponse, error) {
	ad, ok := s.reg.Get(adapterName)
	if !ok {
		return nil, fmt.Errorf("adapter %s not registered", adapterName)
	}

	// 1. 幂等检查
	if prior, err := s.txRepo.FindByIdem(ctx, adapterName, req.IdempotencyKey); err == nil && prior != nil {
		return replayCharge(prior)
	}

	// 2. 先落 pending 行
	tx := &domain.AcquirerTx{
		PiID:           req.PiID,
		Adapter:        adapterName,
		Action:         domain.ActionCharge,
		IdempotencyKey: req.IdempotencyKey,
		State:          domain.AcquirerTxPending,
		Amount:         req.Amount,
		Currency:       req.Currency,
		RequestMethod:  http.MethodPost,
		RequestURL:     "<adapter handles>",
		RequestBody:    mustJSON(req),
		Attempt:        1,
		CreatedAt:      s.now(),
		UpdatedAt:      s.now(),
	}
	aqID, err := s.idgen.Next("aq", req.PiID)
	if err != nil {
		return nil, err
	}
	tx.AqID = aqID

	if err := s.txRepo.Insert(ctx, tx); err != nil {
		// 并发重试窗口：如果 UNIQUE(adapter, idempotency_key) 冲突，重新读旧行回放。
		if isUniqueViolation(err) {
			if prior, _ := s.txRepo.FindByIdem(ctx, adapterName, req.IdempotencyKey); prior != nil {
				return replayCharge(prior)
			}
		}
		return nil, err
	}

	// 3. 真实调用 adapter
	start := s.now()
	resp, callErr := ad.Charge(ctx, req)
	lat := int(s.now().Sub(start).Milliseconds())

	// 4. 落 UPDATE
	fields := map[string]any{
		"latency_ms":    lat,
		"updated_at":    s.now(),
		"response_body": mustJSON(resp),
	}
	if callErr != nil {
		fields["state"] = string(domain.AcquirerTxFailed)
		fields["failure_code"] = channel.FailChannelUnavailable
		fields["raw_failure_code"] = callErr.Error()
	} else {
		fields["external_ref_no"] = resp.ExternalRefNo
		fields["failure_code"] = resp.FailureCode
		fields["raw_failure_code"] = resp.RawFailureCode
		switch resp.Result {
		case channel.ResultSucceeded, channel.ResultAuthorized, channel.ResultRequiresAction, channel.ResultProcessing:
			fields["state"] = string(domain.AcquirerTxSucceeded)
		case channel.ResultFailed:
			fields["state"] = string(domain.AcquirerTxFailed)
			fields["next_retry_at"] = s.now().Add(60 * time.Second) // 上层 worker 会读
		}
	}
	_ = s.txRepo.UpdateResult(ctx, tx.ID, fields)

	return resp, callErr
}

func replayCharge(prior *domain.AcquirerTx) (*channel.ChargeResponse, error) {
	if prior.State == domain.AcquirerTxPending {
		// 上一次未返回，当下本次请求也转为 processing，让上层按 Query 兜底。
		return &channel.ChargeResponse{Result: channel.ResultProcessing, ExternalRefNo: prior.ExternalRefNo}, nil
	}
	var resp channel.ChargeResponse
	if prior.ResponseBody != "" {
		_ = json.Unmarshal([]byte(prior.ResponseBody), &resp)
	}
	if resp.ExternalRefNo == "" {
		resp.ExternalRefNo = prior.ExternalRefNo
	}
	return &resp, nil
}

// --- Webhook 入口 ---------------------------------------------------------

// Ingest：payment-channel 自己的 HTTP 入口（POST /wh/<adapter>）收到回调后调这个。
// 先入 webhook_raw（幂等），再调 adapter.ParseWebhook 拿规范事件，转发给
// order-core 的 WebhookService.Ingest。
func (s *AcquirerService) Ingest(
	ctx context.Context,
	adapterName string,
	headers map[string]string,
	body []byte,
	forward func(ctx context.Context, evt *channel.WebhookEvent) error,
) error {
	ad, ok := s.reg.Get(adapterName)
	if !ok {
		return fmt.Errorf("adapter %s not registered", adapterName)
	}
	evt, err := ad.ParseWebhook(headers, body)
	if err != nil {
		return err
	}
	wh := &domain.WebhookRaw{
		PiID:        evt.PiID,
		Adapter:     adapterName,
		EventID:     evt.EventID,
		EventType:   evt.EventType,
		DedupeKey:   fmt.Sprintf("%s:%s", adapterName, evt.EventID),
		SignatureOK: evt.SignatureOK,
		Headers:     headers,
		Body:        string(body),
		ReceivedAt:  s.now(),
	}
	if err := s.whRepo.Insert(ctx, wh); err != nil {
		if errors.Is(err, domain.ErrWebhookAlreadySeen) || isUniqueViolation(err) {
			return nil // dedupe hit
		}
		return err
	}
	if !evt.SignatureOK {
		return domain.ErrChannelSignatureFail
	}
	ferr := forward(ctx, evt)
	_ = s.whRepo.MarkForwarded(ctx, wh.ID, ferr)
	return ferr
}

// --- helpers ---------------------------------------------------------------

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// 由 internal/repo 根据实际 driver 错误判定（MySQL 的 1062 等），这里占位。
func isUniqueViolation(err error) bool {
	return err != nil && (errors.Is(err, domain.ErrIdempotentHit))
}
