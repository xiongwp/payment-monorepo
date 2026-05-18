// Package service 编排 adapter 调用与 acquirer_tx 持久化。
//
// 核心不变量：**save-first-then-call**。
//  1. 先在 acquirer_tx 落 pending 行（带 UNIQUE(adapter, idempotency_key) 兜底幂等）；
//  2. 再调 adapter；
//  3. 返回后 UPDATE 行（succeeded / failed）+ 写响应 snapshot。
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
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/payment-channel/internal/breaker" // GAP-3: per-(adapter, action) CB
	"github.com/xiongwp/payment-channel/internal/channel"
	"github.com/xiongwp/payment-channel/internal/domain"
	"github.com/xiongwp/payment-channel/internal/metrics"
	"github.com/xiongwp/payment-channel/internal/repo"
	"github.com/xiongwp/payment-util/shadow"
)

// idemCacheTTL 幂等查询结果在进程内的缓存 TTL。
//
// 命中场景：order-core reconcile worker 对同一笔 charge 反复轮询直到 final state；
// payment-core 的 retry-on-deadlock 也可能导致同一 (pi, idem) 多次到达。短 TTL
// 把"已知结果回放"的查询从 DB 摘掉。1 分钟够覆盖典型 retry 风暴，又不至于在
// state 真变（charge 从 pending → succeeded）时长时间错配——服务自己每次
// UpdateResult 都会清缓存。
const idemCacheTTL = 60 * time.Second

// idemCacheMaxEntries LRU 上限。idempotency_key 量级随 PI 数线性增长，
// 4096 在单实例 1000 qps × 60s TTL 下足够；超过即随机驱逐。
const idemCacheMaxEntries = 4096

type idemCacheKey struct{ piID, adapter, idem string }

type idemCacheEntry struct {
	tx      *domain.AcquirerTx
	expires time.Time
}

type idemCache struct {
	mu      sync.RWMutex
	entries map[idemCacheKey]idemCacheEntry
}

func newIdemCache() *idemCache {
	return &idemCache{entries: make(map[idemCacheKey]idemCacheEntry, 64)}
}

func (c *idemCache) get(k idemCacheKey) (*domain.AcquirerTx, bool) {
	c.mu.RLock()
	e, ok := c.entries[k]
	c.mu.RUnlock()
	if !ok || time.Now().After(e.expires) {
		return nil, false
	}
	return e.tx, true
}

func (c *idemCache) set(k idemCacheKey, tx *domain.AcquirerTx) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= idemCacheMaxEntries {
		// 随机驱逐一条；规模下 LRU 复杂度收益小（与 kms decrypt cache 同模式）。
		for k := range c.entries {
			delete(c.entries, k)
			break
		}
	}
	c.entries[k] = idemCacheEntry{tx: tx, expires: time.Now().Add(idemCacheTTL)}
}

func (c *idemCache) invalidate(k idemCacheKey) {
	c.mu.Lock()
	delete(c.entries, k)
	c.mu.Unlock()
}

type AcquirerService struct {
	reg    channel.Registry
	txRepo repo.AcquirerTxRepository
	idgen  repo.IDIssuer
	logger *zap.Logger
	now    func() time.Time
	idem   *idemCache
	// GAP-3: 每个 (adapter, action) 一组 breaker.
	// 5xx / network / timeout 超阈值 → 熔断, fast-fail 给上游让其降级 / 切备用通道,
	// 不再傻等 30s timeout 一个一个失败.
	breakers *breaker.Manager
}

func NewAcquirerService(reg channel.Registry, txRepo repo.AcquirerTxRepository, idgen repo.IDIssuer, logger *zap.Logger) *AcquirerService {
	return &AcquirerService{
		reg:      reg,
		txRepo:   txRepo,
		idgen:    idgen,
		logger:   logger,
		now:      time.Now,
		idem:     newIdemCache(),
		breakers: breaker.NewManager(breaker.DefaultConfig()),
	}
}

// SetBreakerManager 允许外部注入自定义 breaker 配置 (例如运维通过 config-center 调阈值).
// 默认 NewAcquirerService 用 breaker.DefaultConfig — 大部分场景够用.
func (s *AcquirerService) SetBreakerManager(m *breaker.Manager) { s.breakers = m }

// Breakers 暴露给运维 admin HTTP (e.g. /ops/circuit/states + /ops/circuit/reset).
func (s *AcquirerService) Breakers() *breaker.Manager { return s.breakers }

// lookupIdem 缓存优先查 acquirer_tx；命中即返回，未命中落 DB。
// 仅缓存 NON-NIL 命中（首次请求/未存在不缓存——意义不大且会延迟首次写入的可见性）。
func (s *AcquirerService) lookupIdem(ctx context.Context, adapter, action, piID, idem string) (*domain.AcquirerTx, error) {
	k := idemCacheKey{piID: piID, adapter: adapter, idem: idem}
	if tx, ok := s.idem.get(k); ok {
		metrics.IdempotentLookupTotal.WithLabelValues(adapter, action, "cache", "hit").Inc()
		return tx, nil
	}
	tx, err := s.txRepo.FindByIdem(ctx, piID, adapter, idem)
	if err != nil {
		return nil, err
	}
	if tx == nil {
		metrics.IdempotentLookupTotal.WithLabelValues(adapter, action, "db", "miss").Inc()
		return nil, nil
	}
	metrics.IdempotentLookupTotal.WithLabelValues(adapter, action, "db", "hit").Inc()
	s.idem.set(k, tx)
	return tx, nil
}

// ─── Charge ──────────────────────────────────────────────────────────────

func (s *AcquirerService) Charge(ctx context.Context, adapterName string, req *channel.ChargeRequest) (*channel.ChargeResponse, error) {
	// Shadow 短路：压测流量绝不真打到外部渠道。返回 deterministic mock；
	// 不查 / 写 acquirer_tx 表，不刷 idem cache，不走 adapter。
	if shadow.IsShadow(ctx) {
		return shadowChargeResponse(adapterName, req), nil
	}

	ad, ok := s.reg.Get(adapterName)
	if !ok {
		return nil, fmt.Errorf("adapter %s not registered", adapterName)
	}

	// 1. 幂等检查：存在且已完成则直接回放（cache → DB）。
	if prior, err := s.lookupIdem(ctx, adapterName, string(domain.ActionCharge), req.PiID, req.IdempotencyKey); err == nil && prior != nil {
		metrics.IdempotentReplayTotal.WithLabelValues(adapterName, string(domain.ActionCharge)).Inc()
		return replayCharge(prior), nil
	}

	// 2. 先落 pending 行。
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
	id, err := s.idgen.Next("aq", req.PiID)
	if err != nil {
		return nil, err
	}
	tx.AqID = id

	if err := s.txRepo.Insert(ctx, tx); err != nil {
		if errors.Is(err, domain.ErrIdempotentHit) {
			// 竞争 race：cache miss 但落库瞬间被另一并发 Insert 抢先。直接走未缓存
			// FindByIdem（不经 cache，因为 prior 状态是「另一进程刚写的 pending」，
			// 想要立即可见而非 cache 60s 后）。
			if prior, _ := s.txRepo.FindByIdem(ctx, req.PiID, adapterName, req.IdempotencyKey); prior != nil {
				metrics.IdempotentReplayTotal.WithLabelValues(adapterName, string(domain.ActionCharge)).Inc()
				return replayCharge(prior), nil
			}
		}
		return nil, err
	}

	// 3. 真实调用 adapter (GAP-3: CB 包一层).
	//    Allow → 拒绝时直接 fast-fail (state=unknown, 让 PendingQueryWorker 走 Query 推进),
	//    避免 adapter 故障时 N 笔 charge 全部卡 30s 超时.
	brk := s.breakers.Get(adapterName, string(domain.ActionCharge))
	start := s.now()
	var resp *channel.ChargeResponse
	var callErr error
	if brkErr := brk.Allow(); brkErr != nil {
		callErr = brkErr // ErrCircuitOpen — 当作"上游不可用 / 结果未知"处理
		metrics.AcquirerCallTotal.WithLabelValues(adapterName, string(domain.ActionCharge), "circuit_open").Inc()
	} else {
		resp, callErr = ad.Charge(ctx, req)
		// Record success=true if NOT upstream infra failure (5xx/timeout/network).
		// 业务级 denied / 4xx 不算 breaker failure.
		brk.Record(!isUpstreamInfraFailure(callErr))
	}
	lat := int(s.now().Sub(start).Milliseconds())
	metrics.AcquirerCallDuration.WithLabelValues(adapterName, string(domain.ActionCharge)).Observe(time.Since(start).Seconds())

	// 4. 写回 UPDATE。
	fields := map[string]any{
		"latency_ms":    lat,
		"updated_at":    s.now(),
		"response_body": mustJSON(resp),
	}
	result := "failed"
	if callErr != nil {
		// callErr 视作「结果未知」：网络超时 / ctx 超时 / 5xx 都可能是
		// 「下游已落账但响应没回来」。**绝不**重发原请求（双扣 / 双退）；
		// 也**绝不**当 failed 关单。落 unknown，由 PendingQueryWorker 调 Query 推进。
		fields["state"] = string(domain.AcquirerTxUnknown)
		fields["failure_code"] = channel.FailChannelUnavailable
		fields["raw_failure_code"] = truncate(callErr.Error(), 60)
		// 不设 next_retry_at —— CallRetryWorker 只挑 state=failed 的行重发。
		result = "unknown"
	} else {
		fields["external_ref_no"] = resp.ExternalRefNo
		fields["failure_code"] = resp.FailureCode
		fields["raw_failure_code"] = resp.RawFailureCode
		switch resp.Result {
		case channel.ResultSucceeded, channel.ResultAuthorized, channel.ResultRequiresAction, channel.ResultProcessing:
			fields["state"] = string(domain.AcquirerTxSucceeded)
			result = string(resp.Result)
		case channel.ResultUnknown:
			// 渠道明示「未知 / 走 Query 推进」（部分钱包同步只回 ack）。
			fields["state"] = string(domain.AcquirerTxUnknown)
			result = "unknown"
		case channel.ResultFailed:
			fields["state"] = string(domain.AcquirerTxFailed)
			fields["next_retry_at"] = s.now().Add(60 * time.Second)
			result = string(resp.Result)
		}
	}
	_ = s.txRepo.UpdateResult(ctx, req.PiID, tx.ID, fields)
	// 状态发生变化（pending → succeeded/failed）：清进程内 cache，下次查询走 DB
	// 拿到最新行。多副本部署下，每个实例自己的 cache 由本地 Update 触发清除；
	// 跨实例的 stale 仅在 cache TTL 内（≤60s），对幂等回放语义可接受
	// （即便 race 拿到旧 pending 行，replay 也只是返回上一次的 response，调用方依旧
	//  得到一致的「这笔 charge 是 pending」语义）。
	s.idem.invalidate(idemCacheKey{piID: req.PiID, adapter: adapterName, idem: req.IdempotencyKey})
	metrics.AcquirerCallTotal.WithLabelValues(adapterName, string(domain.ActionCharge), result).Inc()

	return resp, callErr
}

// ─── Capture / Void / Refund / Query（同构，简化版） ───────────────────

func (s *AcquirerService) Capture(ctx context.Context, adapterName string, req *channel.CaptureRequest) (*channel.OpResponse, error) {
	if shadow.IsShadow(ctx) {
		return shadowOpResponse(adapterName, domain.ActionCapture, req.IdempotencyKey), nil
	}
	ad, ok := s.reg.Get(adapterName)
	if !ok {
		return nil, fmt.Errorf("adapter %s not registered", adapterName)
	}
	tx, err := s.recordPending(ctx, adapterName, req.PiID, domain.ActionCapture, req.IdempotencyKey, req.Amount, "", req)
	if err != nil {
		if replay, ok := err.(*capturedReplay); ok {
			return replay.resp.(*channel.OpResponse), nil
		}
		return nil, err
	}
	start := s.now()
	resp, callErr := ad.Capture(ctx, req)
	s.finalizeOp(ctx, req.PiID, tx, adapterName, domain.ActionCapture, start, resp, callErr)
	return resp, callErr
}

func (s *AcquirerService) Void(ctx context.Context, adapterName string, req *channel.VoidRequest) (*channel.OpResponse, error) {
	if shadow.IsShadow(ctx) {
		return shadowOpResponse(adapterName, domain.ActionVoid, req.IdempotencyKey), nil
	}
	ad, ok := s.reg.Get(adapterName)
	if !ok {
		return nil, fmt.Errorf("adapter %s not registered", adapterName)
	}
	tx, err := s.recordPending(ctx, adapterName, req.PiID, domain.ActionVoid, req.IdempotencyKey, 0, "", req)
	if err != nil {
		if replay, ok := err.(*capturedReplay); ok {
			return replay.resp.(*channel.OpResponse), nil
		}
		return nil, err
	}
	start := s.now()
	resp, callErr := ad.Void(ctx, req)
	s.finalizeOp(ctx, req.PiID, tx, adapterName, domain.ActionVoid, start, resp, callErr)
	return resp, callErr
}

func (s *AcquirerService) Refund(ctx context.Context, adapterName string, req *channel.RefundRequest) (*channel.OpResponse, error) {
	if shadow.IsShadow(ctx) {
		return shadowOpResponse(adapterName, domain.ActionRefund, req.IdempotencyKey), nil
	}
	ad, ok := s.reg.Get(adapterName)
	if !ok {
		return nil, fmt.Errorf("adapter %s not registered", adapterName)
	}
	tx, err := s.recordPending(ctx, adapterName, req.PiID, domain.ActionRefund, req.IdempotencyKey, req.Amount, "", req)
	if err != nil {
		if replay, ok := err.(*capturedReplay); ok {
			return replay.resp.(*channel.OpResponse), nil
		}
		return nil, err
	}
	start := s.now()
	resp, callErr := ad.Refund(ctx, req)
	s.finalizeOp(ctx, req.PiID, tx, adapterName, domain.ActionRefund, start, resp, callErr)
	return resp, callErr
}

func (s *AcquirerService) Query(ctx context.Context, adapterName string, req *channel.QueryRequest) (*channel.QueryResponse, error) {
	if shadow.IsShadow(ctx) {
		return shadowQueryResponse(adapterName, req.ExternalRefNo), nil
	}
	ad, ok := s.reg.Get(adapterName)
	if !ok {
		return nil, fmt.Errorf("adapter %s not registered", adapterName)
	}
	start := s.now()
	resp, err := ad.Query(ctx, req)
	metrics.AcquirerCallDuration.WithLabelValues(adapterName, string(domain.ActionQuery)).Observe(time.Since(start).Seconds())
	result := "succeeded"
	if err != nil {
		result = "failed"
	} else if resp != nil {
		result = string(resp.Result)
	}
	metrics.AcquirerCallTotal.WithLabelValues(adapterName, string(domain.ActionQuery), result).Inc()
	return resp, err
}

// ─── internal helpers ─────────────────────────────────────────────────

// capturedReplay 通过 error 语义向外层传递「幂等命中回放」的结果。
type capturedReplay struct {
	resp any
}

func (c *capturedReplay) Error() string { return "idempotent replay" }

func (s *AcquirerService) recordPending(
	ctx context.Context,
	adapterName, piID string,
	action domain.AcquirerAction,
	idem string, amount int64, currency string,
	req any,
) (*domain.AcquirerTx, error) {
	if prior, err := s.lookupIdem(ctx, adapterName, string(action), piID, idem); err == nil && prior != nil {
		metrics.IdempotentReplayTotal.WithLabelValues(adapterName, string(action)).Inc()
		return nil, &capturedReplay{resp: replayOp(prior)}
	}
	tx := &domain.AcquirerTx{
		PiID:           piID,
		Adapter:        adapterName,
		Action:         action,
		IdempotencyKey: idem,
		State:          domain.AcquirerTxPending,
		Amount:         amount,
		Currency:       currency,
		RequestMethod:  http.MethodPost,
		RequestURL:     "<adapter handles>",
		RequestBody:    mustJSON(req),
		Attempt:        1,
		CreatedAt:      s.now(),
		UpdatedAt:      s.now(),
	}
	id, err := s.idgen.Next("aq", piID)
	if err != nil {
		return nil, err
	}
	tx.AqID = id
	if err := s.txRepo.Insert(ctx, tx); err != nil {
		if errors.Is(err, domain.ErrIdempotentHit) {
			if prior, _ := s.txRepo.FindByIdem(ctx, piID, adapterName, idem); prior != nil {
				metrics.IdempotentReplayTotal.WithLabelValues(adapterName, string(action)).Inc()
				return nil, &capturedReplay{resp: replayOp(prior)}
			}
		}
		return nil, err
	}
	return tx, nil
}

func (s *AcquirerService) finalizeOp(
	ctx context.Context,
	piID string,
	tx *domain.AcquirerTx,
	adapterName string,
	action domain.AcquirerAction,
	start time.Time,
	resp *channel.OpResponse,
	callErr error,
) {
	lat := int(time.Since(start).Milliseconds())
	metrics.AcquirerCallDuration.WithLabelValues(adapterName, string(action)).Observe(time.Since(start).Seconds())
	fields := map[string]any{
		"latency_ms":    lat,
		"updated_at":    s.now(),
		"response_body": mustJSON(resp),
	}
	result := "failed"
	if callErr != nil {
		// 同 Charge：callErr 落 unknown，让 PendingQueryWorker 走 Query 推进；
		// 不设 next_retry_at —— Capture/Void/Refund 重发都可能造成资损。
		fields["state"] = string(domain.AcquirerTxUnknown)
		fields["failure_code"] = channel.FailChannelUnavailable
		fields["raw_failure_code"] = truncate(callErr.Error(), 60)
		result = "unknown"
	} else if resp != nil {
		fields["external_ref_no"] = resp.ExternalRefNo
		fields["failure_code"] = resp.FailureCode
		fields["raw_failure_code"] = resp.RawFailureCode
		switch resp.Result {
		case channel.ResultSucceeded, channel.ResultAuthorized, channel.ResultProcessing:
			fields["state"] = string(domain.AcquirerTxSucceeded)
			result = string(resp.Result)
		case channel.ResultUnknown:
			fields["state"] = string(domain.AcquirerTxUnknown)
			result = "unknown"
		case channel.ResultFailed:
			fields["state"] = string(domain.AcquirerTxFailed)
			fields["next_retry_at"] = s.now().Add(60 * time.Second)
			result = string(resp.Result)
		}
	}
	_ = s.txRepo.UpdateResult(ctx, piID, tx.ID, fields)
	// 同 Charge：state 更新后立即清进程内 cache，下次查询走 DB 拿最新结果
	s.idem.invalidate(idemCacheKey{piID: piID, adapter: adapterName, idem: tx.IdempotencyKey})
	metrics.AcquirerCallTotal.WithLabelValues(adapterName, string(action), result).Inc()
}

func replayCharge(prior *domain.AcquirerTx) *channel.ChargeResponse {
	if prior.State == domain.AcquirerTxPending {
		return &channel.ChargeResponse{Result: channel.ResultProcessing, ExternalRefNo: prior.ExternalRefNo}
	}
	var resp channel.ChargeResponse
	if prior.ResponseBody != "" {
		_ = json.Unmarshal([]byte(prior.ResponseBody), &resp)
	}
	if resp.ExternalRefNo == "" {
		resp.ExternalRefNo = prior.ExternalRefNo
	}
	return &resp
}

func replayOp(prior *domain.AcquirerTx) *channel.OpResponse {
	if prior.State == domain.AcquirerTxPending {
		return &channel.OpResponse{Result: channel.ResultProcessing, ExternalRefNo: prior.ExternalRefNo}
	}
	var resp channel.OpResponse
	if prior.ResponseBody != "" {
		_ = json.Unmarshal([]byte(prior.ResponseBody), &resp)
	}
	if resp.ExternalRefNo == "" {
		resp.ExternalRefNo = prior.ExternalRefNo
	}
	return &resp
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// isUpstreamInfraFailure GAP-3: 判断 adapter 调用结果是否算上游"基础设施失败" —
// 用于决定要不要给 CB 记 fail.
//
// 算 failure (Record(false)):
//   - callErr != nil (timeout / DNS / TCP / 5xx)
//   - 也含 ctx canceled / deadline exceeded — 当 unknown 处理, 上报为 fail 让 CB 跳闸保护
//
// 不算 failure (Record(true)):
//   - callErr == nil 即业务级 4xx (denied / invalid card / fraud) — 上游正常工作
//     只是这单业务上拒, 不该熔断整个 adapter.
func isUpstreamInfraFailure(callErr error) bool {
	return callErr != nil
}
