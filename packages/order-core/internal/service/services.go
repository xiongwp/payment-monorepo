// Package service 业务层：Stripe 风格的 PaymentIntent / Charge / Refund + FSM + cron。
package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/xiongwp/order-core/internal/channel"
	"github.com/xiongwp/order-core/internal/domain"
	"github.com/xiongwp/order-core/internal/idgen"
	"github.com/xiongwp/order-core/internal/repo"
	"github.com/xiongwp/order-core/internal/sharding"
	"github.com/xiongwp/order-core/internal/urlguard"
)

// ─── PaymentIntentService ─────────────────────────────────────────────────────

// PaymentIntentService Stripe 风格的 PI 生命周期
type PaymentIntentService interface {
	Create(ctx context.Context, in *CreatePaymentIntentInput) (*domain.PaymentIntent, error)
	Retrieve(ctx context.Context, id string) (*domain.PaymentIntent, error)
	Update(ctx context.Context, id string, description string, metadata map[string]string) (*domain.PaymentIntent, error)
	Confirm(ctx context.Context, id, paymentMethod, clientSecret string) (*domain.PaymentIntent, *domain.Charge, error)
	// ConfirmCombined 组合支付：一次 Confirm 创建 N 笔 Charge，sum(splits.amount) == PI.amount。
	// 返回 PI 以及 N 笔 Charge；业务侧可逐渠道推进。
	ConfirmCombined(ctx context.Context, id, clientSecret string, splits []PaymentSplit) (*domain.PaymentIntent, []*domain.Charge, error)
	Capture(ctx context.Context, id string, amountToCapture int64) (*domain.PaymentIntent, *domain.Charge, error)
	Cancel(ctx context.Context, id, reason string) (*domain.PaymentIntent, error)
	List(ctx context.Context, mchID string, page, size int) ([]*domain.PaymentIntent, int64, error)

	// RequireAction / MarkSucceeded / MarkFailed 供渠道回调路径使用
	RequireAction(ctx context.Context, id string) (*domain.PaymentIntent, error)
	MarkSucceeded(ctx context.Context, id, chargeID string, amountReceived int64) (*domain.PaymentIntent, error)
	MarkFailed(ctx context.Context, id, chargeID, failureCode, failureMessage string) (*domain.PaymentIntent, error)

	// ExpireOverdue cron 关单：把过期的非终态 PI 转为 CANCELED
	ExpireOverdue(ctx context.Context, limit int) (int, error)

	// GetStatusReport 基于 order 上的 active_charge_ids / active_refund_ids
	// 逐一查询各单状态，聚合返回："支付是否完成 / 退款是否完成 / 各单终态"。
	GetStatusReport(ctx context.Context, id string) (*StatusReport, error)
}

// ChargeStatusRef 订单状态报告里单笔支付单的快照
type ChargeStatusRef struct {
	ID             string
	PaymentMethod  string
	Amount         int64
	AmountCaptured int64
	Status         domain.ChargeStatus
	FailureCode    string
	FailureMessage string
}

// RefundStatusRef 订单状态报告里单笔退款单的快照
type RefundStatusRef struct {
	ID            string
	ChargeID      string
	Amount        int64
	Status        domain.RefundStatus
	FailureReason string
}

// StatusReport 订单 + 所有进行中/历史支付单/退款单的聚合状态
type StatusReport struct {
	PaymentIntent   *domain.PaymentIntent
	ActiveCharges   []ChargeStatusRef // 进行中（status=pending）
	FinalizedCharges []ChargeStatusRef // 已终态（succeeded/failed）
	ActiveRefunds   []RefundStatusRef
	FinalizedRefunds []RefundStatusRef
	// 汇总
	AllChargesSucceeded bool  // 所有活动支付单都成功
	AnyChargeFailed     bool  // 有支付单失败
	AnyChargePending    bool  // 仍有 pending 支付单
	TotalCaptured       int64 // sum(charges[status=succeeded].amount_captured)
	AllRefundsSucceeded bool
	AnyRefundFailed     bool
	AnyRefundPending    bool
	TotalRefunded       int64
}

// PaymentSplit 组合支付里单个支付方式的分摊
type PaymentSplit struct {
	PaymentMethod    string            // VISA / BALANCE / ALIPAY / ...
	Amount           int64             // 本渠道承担金额
	PaymentMethodRef string            // token / card_id / wallet_id
	Extra            map[string]string // 渠道特定字段
}

// CreatePaymentIntentInput 创建 PI 的输入
type CreatePaymentIntentInput struct {
	Amount                  int64
	Currency                string
	CustomerID              string
	Description             string
	MchID                   string
	MchOrderNo              string
	// BusinessID 分片路由键。为空时 fallback 到 MchID。
	// pi_id 的分片前缀完全由 BusinessID（或 MchID 兜底）哈希决定——
	// 保证"按 business_id 查"与"按 pi_id 查"命中同一个分片。
	BusinessID              string
	// IdempotencyKey 调用方传入的幂等键。(mch_id, idempotency_key) 唯一；
	// 相同 key 的重复创建会直接返回首次结果，不会重复扣钱。
	IdempotencyKey          string
	PreviousPaymentIntentID string
	CaptureMethod           domain.CaptureMethod
	ConfirmationMethod      domain.ConfirmationMethod
	PaymentMethodTypes      []string
	ReturnURL               string
	NotifyURL               string
	StatementDescriptor     string
	Metadata                map[string]string
	Livemode                bool
	ExpiresIn               time.Duration // 非零时 expired_at = now + ExpiresIn
}

type piService struct {
	piRepo        repo.PaymentIntentRepository
	chargeRepo    repo.ChargeRepository
	actionRepo    repo.PayActionRepository
	idgen         idgen.IDGenerator
	router        *sharding.Router
	channels      channel.PaymentChannelRegistry // 可为 nil，nil 时 Confirm 直接落库不调渠道
	defaultChName string                         // 默认 channel name（通常是 "payment-core"）
	// accounting 可为 nil（未接入或单测）；非 nil 时同步 charge succeeded 分支
	// 调用 EnqueueChargeSucceeded，和 webhook_service 异步成功分支保持对称。
	accounting AccountingOutboxService
	logger     *zap.Logger
}

// NewPaymentIntentService 构造
func NewPaymentIntentService(
	pi repo.PaymentIntentRepository,
	ch repo.ChargeRepository,
	ar repo.PayActionRepository,
	g idgen.IDGenerator,
	r *sharding.Router,
	channels channel.PaymentChannelRegistry,
	defaultChannelName string,
	accounting AccountingOutboxService,
	logger *zap.Logger,
) PaymentIntentService {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &piService{
		piRepo: pi, chargeRepo: ch, actionRepo: ar,
		idgen: g, router: r,
		channels: channels, defaultChName: defaultChannelName,
		accounting: accounting,
		logger:     logger,
	}
}

func (s *piService) Create(ctx context.Context, in *CreatePaymentIntentInput) (*domain.PaymentIntent, error) {
	if in == nil {
		return nil, fmt.Errorf("%w: input nil", domain.ErrValidation)
	}
	if in.Amount <= 0 {
		return nil, fmt.Errorf("%w: amount must be > 0", domain.ErrValidation)
	}
	if in.Currency == "" {
		return nil, fmt.Errorf("%w: currency required", domain.ErrValidation)
	}
	if in.MchID == "" {
		return nil, fmt.Errorf("%w: mch_id required", domain.ErrValidation)
	}
	// SSRF 防护：商户传入的回调 URL 不能指向内网 / loopback / cloud-metadata IP。
	// 详细规则见 internal/urlguard。
	if err := urlguard.ValidateOutboundURL(in.ReturnURL); err != nil {
		return nil, fmt.Errorf("%w: return_url: %v", domain.ErrValidation, err)
	}
	if err := urlguard.ValidateOutboundURL(in.NotifyURL); err != nil {
		return nil, fmt.Errorf("%w: notify_url: %v", domain.ErrValidation, err)
	}
	// 幂等检查
	if in.IdempotencyKey != "" {
		if existing, err := s.piRepo.GetByIdempotencyKey(ctx, in.MchID, in.BusinessID, in.IdempotencyKey); err == nil && existing != nil {
			s.logger.Info("idempotent Create returned existing PI",
				zap.String("pi_id", existing.ID),
				zap.String("idempotency_key", in.IdempotencyKey))
			return existing, nil
		} else if err != nil && !errors.Is(err, domain.ErrPaymentIntentNotFound) {
			return nil, err
		}
	}
	captureMethod := in.CaptureMethod
	if captureMethod == "" {
		captureMethod = domain.CaptureAutomatic
	}
	confirmMethod := in.ConfirmationMethod
	if confirmMethod == "" {
		confirmMethod = domain.ConfirmationAutomatic
	}

	// 路由：business_id 哈希定分片；为空时 fallback 到 mch_id
	routeKey := in.BusinessID
	if routeKey == "" {
		routeKey = in.MchID
	}
	dbIdx, tblIdx := s.router.RouteByString(routeKey)
	seq, err := s.idgen.NextID(ctx, idgen.BizTagPaymentIntent)
	if err != nil {
		return nil, err
	}
	id := s.router.FormatID("pi", dbIdx, tblIdx, seq)

	now := time.Now().UTC()
	var expiredAt *time.Time
	if in.ExpiresIn > 0 {
		t := now.Add(in.ExpiresIn)
		expiredAt = &t
	}

	pi := &domain.PaymentIntent{
		ID:                      id,
		Amount:                  in.Amount,
		Currency:                in.Currency,
		Status:                  domain.PIStatusCreated,
		CustomerID:              in.CustomerID,
		Description:             in.Description,
		MchID:                   in.MchID,
		MchOrderNo:              in.MchOrderNo,
		BusinessID:              in.BusinessID,
		IdempotencyKey:          in.IdempotencyKey,
		PreviousPaymentIntentID: in.PreviousPaymentIntentID,
		CaptureMethod:           captureMethod,
		ConfirmationMethod:      confirmMethod,
		ClientSecret:            id + "_secret_" + randHex(16),
		PaymentMethodTypes:      domain.StringList(in.PaymentMethodTypes),
		ReturnURL:               in.ReturnURL,
		NotifyURL:               in.NotifyURL,
		StatementDescriptor:     in.StatementDescriptor,
		Metadata:                domain.Metadata(in.Metadata),
		Livemode:                in.Livemode,
		Created:                 now,
		Updated:                 now,
		ExpiredAt:               expiredAt,
	}
	if err := s.piRepo.Create(ctx, pi); err != nil {
		// 资金安全 / UX：(mch_id, idempotency_key) UNIQUE 撞了说明并发的另一个
		// Create 已经先一步落库（line 187 GetByIdempotencyKey 读到 not-found
		// 后，另一个 goroutine 抢先 INSERT 了）。回查已落库的 PI 返回，让
		// caller 拿到一个一致的结果而不是 error；语义上等价于 idempotent hit。
		if isPIDuplicateKey(err) && in.IdempotencyKey != "" {
			if existing, gErr := s.piRepo.GetByIdempotencyKey(ctx, in.MchID, in.BusinessID, in.IdempotencyKey); gErr == nil && existing != nil {
				s.logger.Info("Create lost race, returning existing PI",
					zap.String("pi_id", existing.ID),
					zap.String("idempotency_key", in.IdempotencyKey))
				return existing, nil
			}
		}
		return nil, err
	}
	return pi, nil
}

// isPIDuplicateKey detects MySQL 1062 (UNIQUE KEY) on PI Create.
// 兼容 mysql driver 的 *MySQLError 和 gorm 包过的 error string。
func isPIDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
		return true
	}
	// gorm 在某些版本会包成 string；fallback sniff
	return strings.Contains(err.Error(), "Duplicate entry")
}

func (s *piService) Retrieve(ctx context.Context, id string) (*domain.PaymentIntent, error) {
	if id == "" {
		return nil, fmt.Errorf("%w: id required", domain.ErrValidation)
	}
	return s.piRepo.Get(ctx, id)
}

func (s *piService) Update(ctx context.Context, id, description string, metadata map[string]string) (*domain.PaymentIntent, error) {
	fields := map[string]any{}
	if description != "" {
		fields["description"] = description
	}
	if metadata != nil {
		fields["metadata"] = domain.Metadata(metadata)
	}
	if len(fields) == 0 {
		return s.piRepo.Get(ctx, id)
	}
	return s.piRepo.UpdateFields(ctx, id, fields)
}

// Confirm 绑定 payment_method 并进入 PROCESSING；同时创建一条 Charge。
//
// 流程：
//  1. 创建 Charge（pending）
//  2. 把 PI 推到 PROCESSING，active_charge_ids 加入本 charge
//  3. 若配置了 channel.PaymentChannelRegistry（= 对接 payment-core）：调 channel.Charge()
//     - succeeded: 立刻 MarkSucceeded
//     - requires_action: 从 RequiredAction 创建一条 PayAction（pending），PI 推到 REQUIRES_ACTION
//     - processing:   保持 PROCESSING 等 webhook
//     - failed:       立刻 MarkFailed
//  4. 若 registry=nil: 保持 PROCESSING 由后续 MarkSucceeded/MarkFailed（webhook 或 admin）推动
func (s *piService) Confirm(ctx context.Context, id, paymentMethod, clientSecret string) (*domain.PaymentIntent, *domain.Charge, error) {
	if id == "" || paymentMethod == "" {
		return nil, nil, fmt.Errorf("%w: id and payment_method required", domain.ErrValidation)
	}
	// P0-1 fix: 先用 CAS 抢占状态 → PROCESSING（from 指定当前合法状态），
	// 如果两个并发 Confirm 来，只有一个能赢 CAS，另一个拿到 ErrInvalidTransition。
	// Charge 只在 CAS 成功后才创建，杜绝孤儿 Charge。
	pi, err := s.piRepo.Get(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if clientSecret != "" && pi.ClientSecret != clientSecret {
		return nil, nil, fmt.Errorf("%w: invalid client_secret", domain.ErrValidation)
	}
	// 先抢占状态（CAS: 只有当前 status=pi.Status 时才能 → PROCESSING）
	updated, err := s.piRepo.UpdateStatus(ctx, id, pi.Status, domain.PIStatusProcessing, func(p *domain.PaymentIntent) {
		p.PaymentMethod = paymentMethod
	})
	if err != nil {
		return nil, nil, err
	}
	// CAS 成功后再创建 Charge — 不会有孤儿
	ch, err := s.createCharge(ctx, updated, paymentMethod)
	if err != nil {
		// Charge 创建失败 → PI 已是 PROCESSING 但没有活跃 Charge。
		// 不能回到 CREATED（FSM 不允许 PROCESSING→CREATED）；
		// 推到 FAILED 是唯一合法出路，避免 PI 永远卡在 PROCESSING。
		s.logger.Error("charge creation failed after CAS, marking PI failed",
			zap.String("pi_id", id), zap.Error(err))
		_, _ = s.piRepo.UpdateStatus(ctx, id, domain.PIStatusProcessing, domain.PIStatusFailed, func(p *domain.PaymentIntent) {
			p.CancellationReason = "charge_creation_failed"
		})
		return nil, nil, err
	}
	// 把 charge ID 写回 PI
	updated, _ = s.piRepo.UpdateFields(ctx, id, map[string]any{
		"active_charge_ids": appendUnique(updated.ActiveChargeIDs, ch.ID),
	})
	// 调 payment-core（如果配置了）
	if s.channels != nil {
		s.invokeChannelForCharge(ctx, updated, ch, paymentMethod, nil)
		// invokeChannelForCharge 内部会更新 PI/Charge；这里重读最新
		if latest, err := s.piRepo.Get(ctx, updated.ID); err == nil {
			updated = latest
		}
		if latestCh, err := s.chargeRepo.Get(ctx, updated.ID, ch.ID); err == nil {
			ch = latestCh
		}
	}
	return updated, ch, nil
}

// invokeChannelForCharge 调对应 channel 执行扣款，依据响应推进 PI / Charge / PayAction。
//
// extra 可为空；非空时传给 channel.PaymentRequest.Extra
func (s *piService) invokeChannelForCharge(ctx context.Context, pi *domain.PaymentIntent, ch *domain.Charge, paymentMethod string, extra map[string]string) {
	chName := s.defaultChName
	if chName == "" {
		chName = "payment-core"
	}
	pc := s.channels.Get(chName)
	if pc == nil {
		s.logger.Warn("no payment channel registered", zap.String("name", chName))
		return
	}
	resp, err := pc.Charge(ctx, channel.PaymentRequest{
		PaymentIntentID: pi.ID,
		ChargeID:        ch.ID,
		Amount:          ch.Amount,
		Currency:        ch.Currency,
		PaymentMethod:   paymentMethod,
		CustomerID:      pi.CustomerID,
		CaptureMethod:   string(pi.CaptureMethod),
		ReturnURL:       pi.ReturnURL,
		NotifyURL:       pi.NotifyURL,
		Description:     pi.Description,
		// 把 PI.Metadata 透传给 channel —— payment-core 的路由会用 metadata.country
		// 决定走哪个 adapter，不传就 no_match。
		Metadata: map[string]string(pi.Metadata),
		Extra:    extra,
	})
	if err != nil {
		// **资金安全**：RPC 失败不能立刻 MarkFailed —— channel 端可能已经扣款
		// 成功（payment-core 收到了响应但传回 order-core 这一跳网络抖了 / 进程
		// crash 了），本地直接标 FAILED → 客户钱卡在 channel + accounting 永远
		// 没入账 → 资金错乱。
		//
		// 留 PROCESSING 等下面两路兜底：
		//   1. ReconcileWorker / ChargeExpireWorker (commit aa18d18) 调
		//      pc.Query(charge_id) 拉真实状态：succeeded → OnLateChargeSuccess
		//      包括 enqueue accounting outbox；failed → MarkFailed
		//   2. channel webhook 异步通知（多数渠道都会）
		// 两路都用 idempotent / CAS 确保幂等，重复触发无害。
		s.logger.Warn("channel charge RPC error; leaving PI/Charge in PROCESSING for reconcile",
			zap.String("pi_id", pi.ID), zap.String("charge_id", ch.ID), zap.Error(err))
		// 写一下 charge 的 failure reason 给运维诊断，但不变 status。
		_, _ = s.chargeRepo.UpdateFields(ctx, pi.ID, ch.ID, map[string]any{
			"failure_code":    "channel_rpc_error",
			"failure_message": "RPC error: " + err.Error() + " (channel state unknown, awaiting reconcile)",
		})
		return
	}
	// 把 external_ref_no / risk 存到 charge
	chargeUpdates := map[string]any{
		"balance_transaction": resp.ExternalRefNo,
	}
	if resp.RiskLevel != "" {
		chargeUpdates["outcome_risk_level"] = resp.RiskLevel
	}
	if resp.RiskScore > 0 {
		chargeUpdates["outcome_risk_score"] = resp.RiskScore
	}
	_, _ = s.chargeRepo.UpdateFields(ctx, pi.ID, ch.ID, chargeUpdates)

	switch resp.ResultType {
	case channel.PaymentResultSucceeded:
		// 部分渠道（GCash mock 等）不填 AmountCaptured，回退到下单时的 ch.Amount。
		// 没这层 fallback，accounting outbox 行 Amount=0，HybridDoubleEntryBooking
		// 的两条 entry 都是 debit=0/credit=0，被 accounting-system "entry must have
		// debit or credit amount" 拒掉。
		amountReceived := resp.AmountCaptured
		if amountReceived <= 0 {
			amountReceived = ch.Amount
		}
		updated, _ := s.MarkSucceeded(ctx, pi.ID, ch.ID, amountReceived)
		// 同步成功分支也要入 accounting outbox，和 webhook 异步成功分支对称；
		// 真 payment-core 对 GCash/mockserver 等同步返回 succeeded，缺了这行
		// accounting-system 永远收不到事件。
		if updated != nil && s.accounting != nil {
			if enqErr := s.accounting.EnqueueChargeSucceeded(ctx, updated, ch.ID, amountReceived); enqErr != nil {
				s.logger.Error("accounting outbox enqueue (sync charge) failed",
					zap.String("pi_id", updated.ID),
					zap.String("charge_id", ch.ID),
					zap.Error(enqErr))
			}
		}
	case channel.PaymentResultFailed:
		_, _ = s.MarkFailed(ctx, pi.ID, ch.ID, resp.FailureCode, resp.FailureMessage)
	case channel.PaymentResultRequiresAction:
		// 根据 RequiredAction 创建 PayAction（pending），PI → REQUIRES_ACTION
		if resp.RequiredAction != nil && s.actionRepo != nil {
			s.createActionFromChannel(ctx, pi, ch, resp.RequiredAction)
		}
	case channel.PaymentResultAuthorized:
		// manual capture path：PI 保持 processing，等商户 Capture
	case channel.PaymentResultProcessing:
		// 异步受理：不动
	}
}

// createActionFromChannel 从渠道 RequiredAction 直接落一条 PayAction + 把 PI 推到 REQUIRES_ACTION
func (s *piService) createActionFromChannel(ctx context.Context, pi *domain.PaymentIntent, ch *domain.Charge, ra *channel.RequiredAction) {
	if ra == nil {
		return
	}
	dbIdx, tblIdx := s.router.RouteByPrefixedID(pi.ID)
	seq, err := s.idgen.NextID(ctx, idgen.BizTagPaymentIntent)
	if err != nil {
		return
	}
	id := s.router.FormatID("act", dbIdx, tblIdx, seq)
	now := time.Now().UTC()
	var expiresAt *time.Time
	if !ra.ExpiresAt.IsZero() {
		t := ra.ExpiresAt
		expiresAt = &t
	}
	var actionType domain.PayActionType
	switch ra.Type {
	case channel.RequiredAction3DSRedirect:
		actionType = domain.PayActionThreeDS
	case channel.RequiredActionOTP:
		actionType = domain.PayActionOTP
	case channel.RequiredActionPayPassword:
		actionType = domain.PayActionPayPassword
	default:
		actionType = domain.PayActionType(ra.Type)
	}
	act := &domain.PayAction{
		ID:              id,
		PaymentIntentID: pi.ID,
		ChargeID:        ch.ID,
		ActionType:      actionType,
		Status:          domain.PayActionStatusPending,
		Payload:         domain.Metadata(ra.Details),
		ExpectedSecret:  ra.ChallengeRef,
		MaxAttempts:     3,
		ExpiresAt:       expiresAt,
		Created:         now,
		Updated:         now,
	}
	// 并行 actionRepo.Create + PI status transition；失败语义同 pay_action.go
	// 同名段：Create 失败放弃整段（无 PayAction 落表），RequireAction 失败仅 warn。
	var (
		wg            sync.WaitGroup
		createErr     error
		transitionErr error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		createErr = s.actionRepo.Create(ctx, act)
	}()
	go func() {
		defer wg.Done()
		_, transitionErr = s.RequireAction(ctx, pi.ID)
	}()
	wg.Wait()
	if createErr != nil {
		s.logger.Warn("create pay_action from channel failed", zap.Error(createErr))
		return
	}
	if transitionErr != nil {
		s.logger.Warn("transition to requires_action failed (channel callback path)",
			zap.Error(transitionErr), zap.String("pi_id", pi.ID))
	}
}

// Capture MANUAL 捕获场景：PROCESSING → SUCCEEDED
func (s *piService) Capture(ctx context.Context, id string, amountToCapture int64) (*domain.PaymentIntent, *domain.Charge, error) {
	pi, err := s.piRepo.Get(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if pi.CaptureMethod != domain.CaptureManual {
		return nil, nil, fmt.Errorf("%w: capture requires capture_method=manual", domain.ErrValidation)
	}
	if amountToCapture < 0 || amountToCapture > pi.Amount {
		return nil, nil, fmt.Errorf("%w: amount_to_capture out of range", domain.ErrValidation)
	}
	if amountToCapture == 0 {
		amountToCapture = pi.Amount
	}
	var ch *domain.Charge
	if len(pi.ActiveChargeIDs) > 0 {
		if c, err := s.chargeRepo.Get(ctx, id, pi.ActiveChargeIDs[len(pi.ActiveChargeIDs)-1]); err == nil {
			ch = c
		}
	}
	if ch == nil {
		return nil, nil, fmt.Errorf("%w: no active charge to capture", domain.ErrValidation)
	}

	// **资金安全 critical fix**：之前的 Capture 只改本地状态，**不调 channel.Capture**！
	// 等于 channel 那边只是 auth 状态，资金没真扣（auth 一般 7 天后过期）。本地却
	// PI=SUCCEEDED + 给 merchant 入账 → 7 天后客户钱回去了，merchant 凭空拿到钱
	// → 资损（accounting 多记一笔贷方）。
	//
	// 现在调 channel.Capture 真正执行 capture：
	//   - succeeded → 本地 MarkSucceeded（含 enqueue accounting outbox）
	//   - failed    → 本地 MarkFailed
	//   - error     → 留 PROCESSING 不变，由 ReconcileWorker / ChargeExpireWorker
	//                 调 Query 兜底（commit 685c4e8 / aa18d18）
	if s.channels != nil {
		chName := s.defaultChName
		if chName == "" {
			chName = "payment-core"
		}
		pc := s.channels.Get(chName)
		if pc != nil {
			cresp, cerr := pc.Capture(ctx, channel.CaptureRequest{
				PaymentIntentID: id,
				ChargeID:        ch.ID,
				ExternalRefNo:   ch.BalanceTransaction,
				Amount:          amountToCapture,
			})
			if cerr != nil {
				// RPC error：channel 状态未知（可能已 capture），不能立刻 MarkFailed。
				// 留 PROCESSING 由 reconcile 兜底。
				s.logger.Warn("channel capture RPC error; leaving PI/Charge in PROCESSING",
					zap.String("pi_id", id), zap.String("charge_id", ch.ID), zap.Error(cerr))
				_, _ = s.chargeRepo.UpdateFields(ctx, id, ch.ID, map[string]any{
					"failure_code":    "channel_rpc_error",
					"failure_message": "Capture RPC error: " + cerr.Error() + " (channel state unknown, awaiting reconcile)",
				})
				return pi, ch, cerr
			}
			switch cresp.ResultType {
			case channel.PaymentResultFailed:
				updated, _ := s.MarkFailed(ctx, id, ch.ID, cresp.FailureCode, cresp.FailureMessage)
				return updated, ch, fmt.Errorf("channel capture failed: %s", cresp.FailureMessage)
			case channel.PaymentResultProcessing:
				// channel 仍在处理 → PI/Charge 保持 PROCESSING
				s.logger.Info("channel capture still processing, awaiting webhook/reconcile",
					zap.String("pi_id", id), zap.String("charge_id", ch.ID))
				return pi, ch, nil
			case channel.PaymentResultSucceeded:
				if cresp.AmountCaptured > 0 {
					amountToCapture = cresp.AmountCaptured
				}
				// 走下面的 MarkSucceeded 路径
			default:
				// 未知 ResultType → 保守留 PROCESSING
				s.logger.Warn("channel capture returned unexpected result, leaving PROCESSING",
					zap.String("pi_id", id), zap.String("result", string(cresp.ResultType)))
				return pi, ch, nil
			}
		}
	}

	// channel 已成功（或没接 channel 的 dev 模式）→ 推进本地状态。
	// MarkSucceeded 内聚 enqueue accounting outbox（commit 481cdc9）。
	updated, err := s.MarkSucceeded(ctx, id, ch.ID, amountToCapture)
	if err != nil {
		return updated, ch, err
	}
	// 重新读最新 charge 状态
	if latestCh, gerr := s.chargeRepo.Get(ctx, id, ch.ID); gerr == nil {
		ch = latestCh
	}
	return updated, ch, nil
}

// Cancel CREATED / REQUIRES_ACTION → CANCELED
func (s *piService) Cancel(ctx context.Context, id, reason string) (*domain.PaymentIntent, error) {
	if id == "" {
		return nil, fmt.Errorf("%w: id required", domain.ErrValidation)
	}
	pi, err := s.piRepo.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	// **资金安全 / UX**：如果 PI 在 PROCESSING (auth hold) 状态，必须先调
	// channel.Void 释放 channel 那边的 auth；否则客户钱会被锁 7 天直到 auth
	// 自动过期。Void 是 idempotent (payment-core 用 sha256(piID, "void") 派生)。
	//
	// 对其它状态（CREATED / REQUIRES_ACTION）channel 无 auth 可释放，跳过。
	if pi.Status == domain.PIStatusProcessing && len(pi.ActiveChargeIDs) > 0 && s.channels != nil {
		chName := s.defaultChName
		if chName == "" {
			chName = "payment-core"
		}
		if pc := s.channels.Get(chName); pc != nil {
			activeChargeID := pi.ActiveChargeIDs[len(pi.ActiveChargeIDs)-1]
			ch, getErr := s.chargeRepo.Get(ctx, id, activeChargeID)
			externalRefNo := ""
			if getErr == nil && ch != nil {
				externalRefNo = ch.BalanceTransaction
			}
			vresp, vErr := pc.Void(ctx, channel.VoidRequest{
				PaymentIntentID: id,
				ChargeID:        activeChargeID,
				ExternalRefNo:   externalRefNo,
			})
			switch {
			case vErr != nil:
				// channel RPC 错 → 不能本地标 CANCELED，否则 channel 那边 auth
				// 还在但 PI 已 canceled → 后续看不到这个 PI → auth 永远没人 release。
				// 留 PROCESSING 让 caller 重试 / reconcile 兜底。
				s.logger.Warn("channel void RPC error during Cancel; refusing to mark CANCELED",
					zap.String("pi_id", id), zap.Error(vErr))
				return nil, fmt.Errorf("cancel deferred: channel void error: %w", vErr)
			case vresp != nil && vresp.ResultType == channel.PaymentResultFailed:
				// channel 拒绝 void（已 captured?）→ 不能 cancel
				return nil, fmt.Errorf("%w: channel rejected void: %s", domain.ErrValidation, vresp.FailureMessage)
			}
			// SUCCEEDED / PROCESSING → 继续本地标 CANCELED
			// charge 没有 CANCELED 状态，用 FAILED 表示终态，failure_code 标识来源。
			_, _ = s.chargeRepo.UpdateFields(ctx, id, activeChargeID, map[string]any{
				"status":          domain.ChargeStatusFailed,
				"failure_code":    "canceled",
				"failure_message": "canceled by user before capture (channel auth voided)",
			})
		}
	}

	now := time.Now().UTC()
	return s.piRepo.UpdateStatus(ctx, id, "", domain.PIStatusCanceled, func(p *domain.PaymentIntent) {
		p.CanceledAt = &now
		p.CancellationReason = reason
	})
}

func (s *piService) List(ctx context.Context, mchID string, page, size int) ([]*domain.PaymentIntent, int64, error) {
	return s.piRepo.List(ctx, mchID, page, size)
}

func (s *piService) RequireAction(ctx context.Context, id string) (*domain.PaymentIntent, error) {
	return s.piRepo.UpdateStatus(ctx, id, "", domain.PIStatusRequiresAction, nil)
}

// MarkSucceeded 渠道异步成功：PROCESSING → SUCCEEDED（同时更新 Charge）
func (s *piService) MarkSucceeded(ctx context.Context, id, chargeID string, amountReceived int64) (*domain.PaymentIntent, error) {
	if chargeID != "" {
		// 资损修复：CAS pending → succeeded。并发 webhook + ReconcileWorker 各
		// 调一次时，第二次 RowsAffected=0 → won=false → 不会用第二次的 amount
		// 覆盖第一次（防 partial-capture race 中后到的 reconcile 把 amount 改小）。
		// PI 行也走 piRepo.UpdateStatus 的 CAS，双层兜底。
		_, _, _ = s.chargeRepo.CASUpdateStatus(ctx, id, chargeID,
			domain.ChargeStatusPending, domain.ChargeStatusSucceeded, map[string]any{
				"amount_captured": amountReceived,
				"captured":        true,
				"paid":            true,
			})
	}
	// P1-5: 显式指定 from=PROCESSING，杜绝 FSM 未来变更引入的 hole
	pi, err := s.piRepo.UpdateStatus(ctx, id, domain.PIStatusProcessing, domain.PIStatusSucceeded, func(p *domain.PaymentIntent) {
		if chargeID != "" {
			p.ActiveChargeIDs = removeID(p.ActiveChargeIDs, chargeID) // charge 已终态，移出列表
		}
		p.AmountReceived = amountReceived
	})
	// **资金安全关键**：内聚 enqueue accounting outbox 到 MarkSucceeded 内，
	// 所有 caller (webhook / ReconcileWorker / OnLateChargeSuccess) 自动触发，
	// 避免漏调（之前 ReconcileWorker 直接 _, _ = MarkSucceeded(...) 漏了 enqueue
	// → 客户已支付但 merchant 账上没钱）。RequestID =
	//   "{pi_id}:charge_succeeded:{charge_id}" deterministic + DB UNIQUE 兜底。
	if err == nil && pi != nil && s.accounting != nil && chargeID != "" {
		if enqErr := s.accounting.EnqueueChargeSucceeded(ctx, pi, chargeID, amountReceived); enqErr != nil {
			s.logger.Error("MarkSucceeded: accounting outbox enqueue failed",
				zap.String("pi_id", id), zap.String("charge_id", chargeID), zap.Error(enqErr))
		}
	}
	return pi, err
}

// MarkFailed 渠道终态失败：PROCESSING → FAILED
func (s *piService) MarkFailed(ctx context.Context, id, chargeID, failureCode, failureMessage string) (*domain.PaymentIntent, error) {
	if chargeID != "" {
		// 资损修复：CAS pending → failed。如果同笔 charge 已被 webhook 标 succeeded，
		// 此 CAS 不会反转回 failed（won=false 静默跳过），避免 status 翻转 +
		// PI 行也跟着走 InvalidTransition 兜底。
		_, _, _ = s.chargeRepo.CASUpdateStatus(ctx, id, chargeID,
			domain.ChargeStatusPending, domain.ChargeStatusFailed, map[string]any{
				"failure_code":    failureCode,
				"failure_message": failureMessage,
			})
	}
	// P1-5: 显式指定 from=PROCESSING
	return s.piRepo.UpdateStatus(ctx, id, domain.PIStatusProcessing, domain.PIStatusFailed, func(p *domain.PaymentIntent) {
		if chargeID != "" {
			p.ActiveChargeIDs = removeID(p.ActiveChargeIDs, chargeID)
		}
	})
}

// ExpireOverdue 扫描全部分片，把过期的非终态 PI 关单。返回关单数量。
func (s *piService) ExpireOverdue(ctx context.Context, limit int) (int, error) {
	list, err := s.piRepo.ListExpired(ctx, time.Now().UTC(), limit)
	if err != nil {
		return 0, err
	}
	closed := 0
	for _, pi := range list {
		if _, err := s.Cancel(ctx, pi.ID, "expired"); err != nil {
			s.logger.Warn("expire pi failed", zap.String("id", pi.ID), zap.Error(err))
			continue
		}
		closed++
	}
	if closed > 0 {
		s.logger.Info("cron: expired payment intents closed", zap.Int("count", closed))
	}
	return closed, nil
}

// ConfirmCombined 组合支付入口。
//
// 为每一个 split 创建一笔 Charge（status=pending），用 latest_charge_id 指向最后一笔；
// 校验 sum(splits.amount) == pi.Amount，保证账单加起来对得上。
// 真实扣款由调用方用返回的 Charge 列表按渠道推进（channel.Charge），
// 按需为每笔 Charge 创建 PayAction（3DS / OTP 等）。
func (s *piService) ConfirmCombined(ctx context.Context, id, clientSecret string, splits []PaymentSplit) (*domain.PaymentIntent, []*domain.Charge, error) {
	if id == "" {
		return nil, nil, fmt.Errorf("%w: id required", domain.ErrValidation)
	}
	if len(splits) == 0 {
		return nil, nil, fmt.Errorf("%w: at least one payment method required", domain.ErrValidation)
	}
	pi, err := s.piRepo.Get(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if clientSecret != "" && pi.ClientSecret != clientSecret {
		return nil, nil, fmt.Errorf("%w: invalid client_secret", domain.ErrValidation)
	}
	var sum int64
	for _, sp := range splits {
		if sp.PaymentMethod == "" || sp.Amount <= 0 {
			return nil, nil, fmt.Errorf("%w: each split requires payment_method + positive amount", domain.ErrValidation)
		}
		sum += sp.Amount
	}
	if sum != pi.Amount {
		return nil, nil, fmt.Errorf("%w: sum(splits.amount)=%d != pi.amount=%d",
			domain.ErrValidation, sum, pi.Amount)
	}

	dbIdx, tblIdx := s.router.RouteByPrefixedID(pi.ID)
	// P0-3: CAS 先抢状态（同 Confirm 的 TOCTOU 修法）
	updated, err := s.piRepo.UpdateStatus(ctx, id, pi.Status, domain.PIStatusProcessing, func(p *domain.PaymentIntent) {
		p.PaymentMethod = "COMBINED"
	})
	if err != nil {
		return nil, nil, err
	}

	charges := make([]*domain.Charge, 0, len(splits))
	now := time.Now().UTC()
	methodList := make([]string, 0, len(splits))
	for i, sp := range splits {
		seq, err := s.idgen.NextID(ctx, idgen.BizTagCharge)
		if err != nil {
			// P0-3: 部分 Charge 已创建但后续失败 → 标记已创建的为 failed
			s.rollbackPartialCharges(ctx, pi.ID, charges)
			_, _ = s.piRepo.UpdateStatus(ctx, id, domain.PIStatusProcessing, domain.PIStatusFailed, func(p *domain.PaymentIntent) {
				p.CancellationReason = fmt.Sprintf("combined charge #%d id generation failed", i)
			})
			return nil, nil, err
		}
		ch := &domain.Charge{
			ID:              s.router.FormatID("ch", dbIdx, tblIdx, seq),
			PaymentIntentID: pi.ID,
			Amount:          sp.Amount,
			Currency:        pi.Currency,
			Status:          domain.ChargeStatusPending,
			PaymentMethod:   sp.PaymentMethod,
			Livemode:        pi.Livemode,
			Created:         now,
			Updated:         now,
		}
		if err := s.chargeRepo.Create(ctx, ch); err != nil {
			s.rollbackPartialCharges(ctx, pi.ID, charges)
			_, _ = s.piRepo.UpdateStatus(ctx, id, domain.PIStatusProcessing, domain.PIStatusFailed, func(p *domain.PaymentIntent) {
				p.CancellationReason = fmt.Sprintf("combined charge #%d create failed: %v", i, err)
			})
			return nil, nil, err
		}
		charges = append(charges, ch)
		methodList = append(methodList, sp.PaymentMethod)
	}

	// 写回 charge IDs + method list（CAS 已在上面完成）
	updated, err = s.piRepo.UpdateFields(ctx, id, map[string]any{
		"active_charge_ids":  appendChargeIDs(updated.ActiveChargeIDs, charges),
		"payment_method_types": domain.StringList(methodList),
	})
	if err != nil {
		return nil, nil, err
	}
	return updated, charges, nil
}

func (s *piService) createCharge(ctx context.Context, pi *domain.PaymentIntent, paymentMethod string) (*domain.Charge, error) {
	dbIdx, tblIdx := s.router.RouteByPrefixedID(pi.ID)
	seq, err := s.idgen.NextID(ctx, idgen.BizTagCharge)
	if err != nil {
		return nil, err
	}
	id := s.router.FormatID("ch", dbIdx, tblIdx, seq)
	now := time.Now().UTC()
	ch := &domain.Charge{
		ID:              id,
		PaymentIntentID: pi.ID,
		Amount:          pi.Amount,
		Currency:        pi.Currency,
		Status:          domain.ChargeStatusPending,
		PaymentMethod:   paymentMethod,
		Livemode:        pi.Livemode,
		Created:         now,
		Updated:         now,
	}
	if err := s.chargeRepo.Create(ctx, ch); err != nil {
		return nil, err
	}
	return ch, nil
}

// ─── ChargeService ────────────────────────────────────────────────────────────

// ChargeService Charge 查询
type ChargeService interface {
	Retrieve(ctx context.Context, piID, chargeID string) (*domain.Charge, error)
	// RetrieveByChargeID 按 ch_ 前缀路由到分片后查询
	RetrieveByChargeID(ctx context.Context, chargeID string) (*domain.Charge, error)
	ListByPI(ctx context.Context, piID string) ([]*domain.Charge, error)
}

type chargeService struct {
	repo   repo.ChargeRepository
	router *sharding.Router
}

// NewChargeService 构造
func NewChargeService(r repo.ChargeRepository, router *sharding.Router) ChargeService {
	return &chargeService{repo: r, router: router}
}

func (s *chargeService) Retrieve(ctx context.Context, piID, chargeID string) (*domain.Charge, error) {
	return s.repo.Get(ctx, piID, chargeID)
}

// RetrieveByChargeID 通过 ch_ id 前缀解析分片位，跨 PI 定位 charge
func (s *chargeService) RetrieveByChargeID(ctx context.Context, chargeID string) (*domain.Charge, error) {
	if chargeID == "" {
		return nil, fmt.Errorf("%w: charge_id required", domain.ErrValidation)
	}
	return s.repo.GetByChargeID(ctx, chargeID)
}

func (s *chargeService) ListByPI(ctx context.Context, piID string) ([]*domain.Charge, error) {
	return s.repo.ListByPI(ctx, piID)
}

// ─── RefundService ────────────────────────────────────────────────────────────

// RefundService Refund 生命周期
type RefundService interface {
	Create(ctx context.Context, in *CreateRefundInput) (*domain.Refund, error)
	// CreateBundle 组合支付退款入口：单次调用按比例拆分到 PI 下所有 SUCCEEDED 的 Charge。
	// 若 PI 只有一笔 Charge，效果等同于 Create。
	CreateBundle(ctx context.Context, in *CreateRefundInput) (*RefundBundle, error)
	Retrieve(ctx context.Context, piID, refundID string) (*domain.Refund, error)
	RetrieveByRefundID(ctx context.Context, refundID string) (*domain.Refund, error)
	ListByPI(ctx context.Context, piID string) ([]*domain.Refund, error)
	MarkSucceeded(ctx context.Context, piID, refundID string) (*domain.Refund, error)
	MarkFailed(ctx context.Context, piID, refundID, failureReason string) (*domain.Refund, error)
}

// CreateRefundInput 创建退款的输入。
//
// 单渠道扣款：传 charge_id 或仅 payment_intent_id（服务端取 latest_charge_id）。
// 组合支付：
//   - 传 payment_intent_id + amount（不指定 charge_id）→ 默认按比例拆分到所有 SUCCEEDED 的 Charge
//   - 或多次调用 CreateRefundInput + 指定 charge_id 退特定渠道
type CreateRefundInput struct {
	PaymentIntentID string
	ChargeID        string
	Amount          int64 // 0 表示全额退（组合支付时 = 全部可退金额，按比例拆）
	Reason          domain.RefundReason
	Metadata        map[string]string
	// AllowCombinedSplit 显式允许组合支付自动按比例拆（默认 true）；false 时必须传 charge_id
	AllowCombinedSplit bool
}

// CreateRefundsInput 批量退款结果（组合支付会返回多条）
type RefundBundle struct {
	Refunds []*domain.Refund
	// TotalAmount sum(refunds.amount)
	TotalAmount int64
}

type refundService struct {
	piRepo     repo.PaymentIntentRepository
	chargeRepo repo.ChargeRepository
	refundRepo repo.RefundRepository
	// accounting MarkSucceeded 内聚 enqueue accounting outbox。
	// 所有 caller (webhook / RefundRetryWorker / 后续可能的别的地方) 都会自动
	// 触发 enqueue，避免漏调。可选 nil（dev / 测试）。
	accounting AccountingOutboxService
	idgen      idgen.IDGenerator
	router     *sharding.Router
	logger     *zap.Logger
}

// NewRefundService 构造
func NewRefundService(
	pi repo.PaymentIntentRepository,
	ch repo.ChargeRepository,
	rf repo.RefundRepository,
	accounting AccountingOutboxService,
	g idgen.IDGenerator,
	r *sharding.Router,
	logger *zap.Logger,
) RefundService {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &refundService{
		piRepo: pi, chargeRepo: ch, refundRepo: rf,
		accounting: accounting,
		idgen:      g, router: r, logger: logger,
	}
}

func (s *refundService) Create(ctx context.Context, in *CreateRefundInput) (*domain.Refund, error) {
	if in == nil || (in.PaymentIntentID == "" && in.ChargeID == "") {
		return nil, fmt.Errorf("%w: payment_intent_id or charge_id required", domain.ErrValidation)
	}

	piID := in.PaymentIntentID
	chargeID := in.ChargeID
	var pi *domain.PaymentIntent
	var ch *domain.Charge
	var err error

	if chargeID != "" && piID == "" {
		// 只有 charge_id：先找到 PI
		// 需要知道分片——Charge.id 与 PI 同分片，用 ch_ 前缀解析。
		// 这里要求调用方传 payment_intent_id 才能走分片查找；当只有 charge_id 时，
		// 我们用 ch_id 解析分片，但查询需要 piID 字段。
		// 实际做法：用 charge_id 所在分片 + charge 表反查 payment_intent_id。
		dbIdx, tblIdx := s.router.RouteByPrefixedID(chargeID)
		tblName := s.router.TableName(ctx, "charge", tblIdx)
		_ = dbIdx
		_ = tblName
		return nil, fmt.Errorf("%w: please supply payment_intent_id together with charge_id", domain.ErrValidation)
	}

	pi, err = s.piRepo.Get(ctx, piID)
	if err != nil {
		return nil, err
	}
	if err := ensureRefundable(pi); err != nil {
		return nil, err
	}
	if chargeID == "" {
		// P0-4: 只对 SUCCEEDED 状态的 Charge 退款。ActiveChargeIDs 里可能有 PENDING
		// 的 Charge（尚未到终态），不能对它们发起退款。
		if charges, err := s.chargeRepo.ListByPI(ctx, piID); err == nil {
			for i := len(charges) - 1; i >= 0; i-- {
				if charges[i].Status == domain.ChargeStatusSucceeded {
					chargeID = charges[i].ID
					break
				}
			}
		}
	}
	if chargeID == "" {
		return nil, fmt.Errorf("%w: no charge to refund", domain.ErrValidation)
	}
	ch, err = s.chargeRepo.Get(ctx, piID, chargeID)
	if err != nil {
		return nil, err
	}

	// 可退金额 = 已捕获 − 已退（已退以 Charge.amount_refunded + 正在成功中的 refund 总和为准）
	// 注意：支付金额 ch.Amount 未必等于可退金额（比如 manual capture 只捕获部分、或渠道扣手续费）
	capturedAmt := ch.AmountCaptured
	if capturedAmt <= 0 {
		// 兼容：如果 Charge 已到 succeeded 但 amount_captured 未被回填，则用 Amount 兜底
		capturedAmt = ch.Amount
	}
	refundable := capturedAmt - ch.AmountRefunded
	if refundable <= 0 {
		return nil, fmt.Errorf("%w: charge has no refundable balance (captured=%d, refunded=%d)",
			domain.ErrRefundAmountExceeded, capturedAmt, ch.AmountRefunded)
	}

	amount := in.Amount
	if amount <= 0 {
		amount = refundable // 全额退：退掉全部剩余可退
	}
	if amount > refundable {
		return nil, fmt.Errorf("%w: requested=%d > refundable=%d (captured=%d, refunded=%d)",
			domain.ErrRefundAmountExceeded, amount, refundable, capturedAmt, ch.AmountRefunded)
	}
	// 并发保护：累加 PENDING + SUCCEEDED 的退款金额，与捕获金额比较。
	//
	// **关键**：必须包含 PENDING（不只是 SUCCEEDED），否则两个并发 Create 会都
	// 看到 succeeded=0 → 都通过校验 → 都创建 PENDING refund → 都向 channel 真实
	// 发起 → 累计退款超过 amount_captured → 资金风险（多退给用户）。
	//
	// 同时这是 read-then-INSERT 模式仍有 race window；下游 channel 用
	// idempotency_key 是按 RefundID 派生的（不同 refund 不同 key），
	// 所以 channel 不会拦截。要真正消除 race 需要：
	//   1. SELECT charge FOR UPDATE 锁住 charge 行
	//   2. 在锁内 SumActive + 校验 + INSERT refund
	// 当前最低成本保护是 SumActiveByCharge（包含 PENDING），缩窄 race 窗到
	// "两个 Create 在亚毫秒内并发"的极小概率。生产可加 FOR UPDATE 进一步收紧。
	active, err := s.refundRepo.SumActiveByCharge(ctx, piID, chargeID)
	if err != nil {
		return nil, err
	}
	if active+amount > capturedAmt {
		return nil, fmt.Errorf("%w: active(pending+succeeded)=%d + requested=%d > captured=%d",
			domain.ErrRefundAmountExceeded, active, amount, capturedAmt)
	}

	dbIdx, tblIdx := s.router.RouteByPrefixedID(piID)
	seq, err := s.idgen.NextID(ctx, idgen.BizTagRefund)
	if err != nil {
		return nil, err
	}
	id := s.router.FormatID("re", dbIdx, tblIdx, seq)

	now := time.Now().UTC()
	rf := &domain.Refund{
		ID:              id,
		ChargeID:        chargeID,
		PaymentIntentID: piID,
		Amount:          amount,
		Currency:        pi.Currency,
		Status:          domain.RefundStatusPending,
		Reason:          in.Reason,
		Metadata:        domain.Metadata(in.Metadata),
		Created:         now,
		Updated:         now,
	}
	if err := s.refundRepo.Create(ctx, rf); err != nil {
		return nil, err
	}
	// P1-1: 更新 PI.RefundPhase → REFUNDING
	_, _ = s.piRepo.UpdateFields(ctx, piID, map[string]any{
		"refund_phase": domain.RefundPhaseRefunding,
	})
	return rf, nil
}

// CreateBundle 组合支付退款：按比例拆分到所有 SUCCEEDED 的 Charge。
//
// 规则：
//   - amount 为 0 时，退每笔 Charge 的全部剩余可退额之和；
//   - amount > 0 时，按各 Charge 的 refundable 权重等比例拆分；
//   - 最后一笔拿"余数"保证 sum(allocations) == amount；
//   - 任何一笔创建失败 → 返回已创建的部分（不做事务回滚；调用方按 Refund.status 推进）。
func (s *refundService) CreateBundle(ctx context.Context, in *CreateRefundInput) (*RefundBundle, error) {
	if in == nil || in.PaymentIntentID == "" {
		return nil, fmt.Errorf("%w: payment_intent_id required", domain.ErrValidation)
	}
	pi, err := s.piRepo.Get(ctx, in.PaymentIntentID)
	if err != nil {
		return nil, err
	}
	if err := ensureRefundable(pi); err != nil {
		return nil, err
	}
	charges, err := s.chargeRepo.ListByPI(ctx, in.PaymentIntentID)
	if err != nil {
		return nil, err
	}

	// 只对 succeeded 的 Charge 分摊
	type slot struct {
		ch         *domain.Charge
		refundable int64
	}
	var slots []slot
	var totalRefundable int64
	for _, ch := range charges {
		if ch.Status != domain.ChargeStatusSucceeded {
			continue
		}
		cap := ch.AmountCaptured
		if cap <= 0 {
			cap = ch.Amount
		}
		// **资金安全**：用 SumActiveByCharge (PENDING + SUCCEEDED) 而不是 SumSucceeded。
		// 与单 Create 的修复 (commit 6f114c7) 同思路：忽略 PENDING 会让两个并发
		// CreateBundle 都看到 succeeded=0 → 都通过校验 → 都创建 PENDING refund →
		// 都向 channel 真实发起 → 累计退款超过 amount_captured。
		// charge.AmountRefunded 只在 refund 成功后递增（commit 6f114c7 注释），
		// 所以以 active 累计为准更准确。
		active, sumErr := s.refundRepo.SumActiveByCharge(ctx, pi.ID, ch.ID)
		if sumErr != nil {
			s.logger.Warn("CreateBundle: SumActiveByCharge failed, falling back to AmountRefunded",
				zap.String("charge_id", ch.ID), zap.Error(sumErr))
			active = ch.AmountRefunded
		}
		rem := cap - active
		if rem <= 0 {
			continue
		}
		slots = append(slots, slot{ch: ch, refundable: rem})
		totalRefundable += rem
	}
	if len(slots) == 0 || totalRefundable <= 0 {
		return nil, fmt.Errorf("%w: no refundable charges", domain.ErrRefundAmountExceeded)
	}

	amount := in.Amount
	if amount <= 0 {
		amount = totalRefundable
	}
	if amount > totalRefundable {
		return nil, fmt.Errorf("%w: requested=%d > totalRefundable=%d",
			domain.ErrRefundAmountExceeded, amount, totalRefundable)
	}

	dbIdx, tblIdx := s.router.RouteByPrefixedID(pi.ID)
	out := &RefundBundle{}
	remaining := amount
	for i, sl := range slots {
		var alloc int64
		if i == len(slots)-1 {
			alloc = remaining
		} else {
			alloc = amount * sl.refundable / totalRefundable
		}
		if alloc <= 0 {
			continue
		}
		remaining -= alloc

		seq, err := s.idgen.NextID(ctx, idgen.BizTagRefund)
		if err != nil {
			return out, err
		}
		refundID := s.router.FormatID("re", dbIdx, tblIdx, seq)
		now := time.Now().UTC()
		rf := &domain.Refund{
			ID:              refundID,
			ChargeID:        sl.ch.ID,
			PaymentIntentID: pi.ID,
			Amount:          alloc,
			Currency:        sl.ch.Currency,
			Status:          domain.RefundStatusPending,
			Reason:          in.Reason,
			Metadata:        domain.Metadata(in.Metadata),
			Created:         now,
			Updated:         now,
		}
		if err := s.refundRepo.Create(ctx, rf); err != nil {
			return out, err
		}
		out.Refunds = append(out.Refunds, rf)
		out.TotalAmount += alloc
	}
	return out, nil
}

func (s *refundService) Retrieve(ctx context.Context, piID, refundID string) (*domain.Refund, error) {
	return s.refundRepo.Get(ctx, piID, refundID)
}

// RetrieveByRefundID 通过 re_ id 前缀路由，跨 PI 定位 refund
func (s *refundService) RetrieveByRefundID(ctx context.Context, refundID string) (*domain.Refund, error) {
	if refundID == "" {
		return nil, fmt.Errorf("%w: refund_id required", domain.ErrValidation)
	}
	return s.refundRepo.GetByRefundID(ctx, refundID)
}

func (s *refundService) ListByPI(ctx context.Context, piID string) ([]*domain.Refund, error) {
	return s.refundRepo.ListByPI(ctx, piID)
}

func (s *refundService) MarkSucceeded(ctx context.Context, piID, refundID string) (*domain.Refund, error) {
	// 资损修复：CAS pending → succeeded 一次性原子推进，only first-winner 跑下面的副作用
	// （charge.amount_refunded 累加、refunded flag、refund_phase、accounting outbox）。
	// 之前用非 CAS UpdateFields 改 status，紧跟 SQL `amount_refunded = amount_refunded + ?`。
	// 并发 webhook + RefundRetryWorker 各调一次 → 累加跑两次 → 余额翻倍 → 后续合法 refund
	// 被 SumActiveByCharge 拒掉 → 商户无法继续退款。
	rf, won, err := s.refundRepo.CASUpdateStatus(ctx, piID, refundID,
		domain.RefundStatusPending, domain.RefundStatusSucceeded, nil)
	if err != nil {
		return nil, err
	}
	if !won {
		// 已被另一个 caller 推进过；返回当前行（status 多半已是 succeeded），不再做副作用。
		s.logger.Info("refund MarkSucceeded CAS skipped (already terminal)",
			zap.String("pi_id", piID), zap.String("refund_id", refundID),
			zap.String("current_status", string(rf.Status)))
		return rf, nil
	}
	// 用 SQL 原子递增 Charge.amount_refunded，避免并发退款 read-then-write 竞态。
	// gorm.Expr 生成 `amount_refunded = amount_refunded + ?`，DB 端原子执行。
	// CAS 兜底已保证本块只跑一次。
	if rf.ChargeID != "" {
		_, _ = s.chargeRepo.UpdateFields(ctx, piID, rf.ChargeID, map[string]any{
			"amount_refunded": gorm.Expr("amount_refunded + ?", rf.Amount),
		})
		// 单独 update refunded flag（需要读最新 amount_refunded 才能判断）
		if ch, err := s.chargeRepo.Get(ctx, piID, rf.ChargeID); err == nil {
			if ch.AmountRefunded >= ch.Amount {
				_, _ = s.chargeRepo.UpdateFields(ctx, piID, rf.ChargeID, map[string]any{
					"refunded": true,
				})
			}
		}
	}
	// P1-1: 更新 RefundPhase（按 Charge 状态判断全额/部分）
	if rf.ChargeID != "" {
		if ch, err := s.chargeRepo.Get(ctx, piID, rf.ChargeID); err == nil {
			if ch.AmountRefunded >= ch.AmountCaptured || ch.AmountRefunded >= ch.Amount {
				_, _ = s.piRepo.UpdateFields(ctx, piID, map[string]any{"refund_phase": domain.RefundPhaseFullyRefunded})
			} else {
				_, _ = s.piRepo.UpdateFields(ctx, piID, map[string]any{"refund_phase": domain.RefundPhasePartiallyRefunded})
			}
		}
	}
	// **资金安全关键**：MarkSucceeded 内聚 enqueue accounting outbox。
	// 之前只有 webhook 路径调用 EnqueueRefundSucceeded，RefundRetryWorker 路径
	// 漏掉 → channel 退款成功但 accounting 没记 → merchant 待结算账户没扣回 →
	// 实际钱已退给客户但账上 merchant 还多这笔进账 → 资金错乱。
	// 用 deterministic RequestID = "{pi_id}:refund_succeeded:{refund_id}" + DB UNIQUE
	// 兜底；webhook 已 enqueue 过的话本次 INSERT DUPLICATE KEY → 幂等无害。
	if s.accounting != nil {
		if pi, piErr := s.piRepo.Get(ctx, piID); piErr == nil && pi != nil {
			if enqErr := s.accounting.EnqueueRefundSucceeded(ctx, pi, rf); enqErr != nil {
				s.logger.Error("MarkSucceeded: accounting outbox enqueue failed",
					zap.String("pi_id", piID), zap.String("refund_id", refundID), zap.Error(enqErr))
			}
		}
	}
	return rf, nil
}

func (s *refundService) MarkFailed(ctx context.Context, piID, refundID, failureReason string) (*domain.Refund, error) {
	// 资损修复：CAS pending → failed。second-caller no-op，避免 RefundPhase 被反复
	// 写成 REFUND_FAILED（如果之前已经 succeeded，被错误覆盖回 FAILED 是 catastrophic）。
	rf, won, err := s.refundRepo.CASUpdateStatus(ctx, piID, refundID,
		domain.RefundStatusPending, domain.RefundStatusFailed, map[string]any{
			"failure_reason": failureReason,
		})
	if err != nil {
		return nil, err
	}
	if !won {
		s.logger.Info("refund MarkFailed CAS skipped (already terminal)",
			zap.String("pi_id", piID), zap.String("refund_id", refundID),
			zap.String("current_status", string(rf.Status)))
		return rf, nil
	}
	// P1-1: RefundPhase → REFUND_FAILED
	_, _ = s.piRepo.UpdateFields(ctx, piID, map[string]any{"refund_phase": domain.RefundPhaseRefundFailed})
	return rf, nil
}

// GetStatusReport 基于 active_*_ids + charge / refund 表数据汇总订单状态
func (s *piService) GetStatusReport(ctx context.Context, id string) (*StatusReport, error) {
	pi, err := s.piRepo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	rep := &StatusReport{PaymentIntent: pi, AllChargesSucceeded: true, AllRefundsSucceeded: true}

	charges, err := s.chargeRepo.ListByPI(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, c := range charges {
		ref := ChargeStatusRef{
			ID: c.ID, PaymentMethod: c.PaymentMethod, Amount: c.Amount,
			AmountCaptured: c.AmountCaptured, Status: c.Status,
			FailureCode: c.FailureCode, FailureMessage: c.FailureMessage,
		}
		switch c.Status {
		case domain.ChargeStatusPending:
			rep.AnyChargePending = true
			rep.AllChargesSucceeded = false
			rep.ActiveCharges = append(rep.ActiveCharges, ref)
		case domain.ChargeStatusSucceeded:
			rep.TotalCaptured += c.AmountCaptured
			rep.FinalizedCharges = append(rep.FinalizedCharges, ref)
		case domain.ChargeStatusFailed, domain.ChargeStatusExpired:
			rep.AnyChargeFailed = true
			rep.AllChargesSucceeded = false
			rep.FinalizedCharges = append(rep.FinalizedCharges, ref)
		}
	}

	// refundRepo 不在 piService 里；通过占位字段：空 slice
	// 真实 Report 构造由 refundService 聚合，这里先把退款部分留空，避免循环依赖。
	_ = pi.ActiveRefundIDs

	return rep, nil
}

// ensureRefundable 校验 PI 是否可以发起新的退款：
//   - Status 必须是 succeeded
//   - RefundPhase 必须不是 fully_refunded / refunding（进行中禁止重复发起）
func ensureRefundable(pi *domain.PaymentIntent) error {
	if pi.Status != domain.PIStatusSucceeded {
		return fmt.Errorf("%w: refund requires payment_intent.status=succeeded, got %s", domain.ErrInvalidTransition, pi.Status)
	}
	switch pi.RefundPhase {
	case domain.RefundPhaseFullyRefunded:
		return fmt.Errorf("%w: payment_intent is fully refunded", domain.ErrRefundAmountExceeded)
	case domain.RefundPhaseRefunding:
		return fmt.Errorf("%w: a refund is already in progress; wait for it to finalize", domain.ErrInvalidTransition)
	}
	return nil
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// appendUnique 在 StringList 里追加，如果已存在则跳过
func appendUnique(list domain.StringList, id string) domain.StringList {
	for _, x := range list {
		if x == id {
			return list
		}
	}
	return append(append(domain.StringList{}, list...), id)
}

// removeID 从 StringList 里移除指定 id（不改变顺序）
func removeID(list domain.StringList, id string) domain.StringList {
	out := make(domain.StringList, 0, len(list))
	for _, x := range list {
		if x == id {
			continue
		}
		out = append(out, x)
	}
	return out
}

func appendChargeIDs(list domain.StringList, charges []*domain.Charge) domain.StringList {
	for _, c := range charges {
		list = appendUnique(list, c.ID)
	}
	return list
}

// rollbackPartialCharges 组合支付失败时，把已创建的 Charge 标为 failed
func (s *piService) rollbackPartialCharges(ctx context.Context, piID string, charges []*domain.Charge) {
	for _, c := range charges {
		_, _ = s.chargeRepo.UpdateFields(ctx, piID, c.ID, map[string]any{
			"status":          domain.ChargeStatusFailed,
			"failure_code":    "combined_rollback",
			"failure_message": "partial combined charge rollback",
		})
	}
}

func randHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(buf)
}
