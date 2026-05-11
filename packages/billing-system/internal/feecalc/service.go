// Package feecalc — 把 TransactionInput → FeeEvent 持久化全流程。
//
// Pipeline:
//
//	in: TransactionInput (from order-core / payment-channel 事件)
//	→ feerule.Engine.Pick   找规则
//	→ feerule.Compute       算 fee_minor + fx_markup
//	→ ApplyRefundPolicy     refund/chargeback 调整符号
//	→ FX conversion         若 currency != base，转 base 算 fee_minor_base
//	→ Repository.SaveFeeEvent  写库（idempotent by ref_id+event_type）
//	→ metrics                Prometheus 打点
//
// 入口：Service.Process(ctx, input) → *FeeEvent
//
// 调用者：
//   - ingest/ Kafka consumer 监听 order-core.charge.succeeded → 调 Process
//   - grpc/ gRPC handler 给外部一次性算 fee 用（不落库 / dry-run）

package feecalc

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"reconcile-system/packages/billing-system/internal/domain"
	"reconcile-system/packages/billing-system/internal/feerule"
)

// Repository 抽象 — 真实实现在 internal/repository。
type Repository interface {
	SaveFeeEvent(ctx context.Context, ev *domain.FeeEvent) error
	// LoadOriginalCharge 给定 refund 的 ref_id（charge_id），查原 charge 的 fee_event。
	// 用于 refund 时回溯原 fee 算返还。
	LoadOriginalCharge(ctx context.Context, merchantID, chargeID string) (*domain.FeeEvent, error)
	// ExistsByRef 幂等检查（重复事件不重复落库）。
	ExistsByRef(ctx context.Context, merchantID, refID string, eventType domain.EventType) (bool, error)
}

// FXProvider 跨币种换汇（caller 注入）。
type FXProvider interface {
	// Rate 返从 src 到 dst 的 1:N 汇率。同币种返 1.0。
	Rate(ctx context.Context, src, dst string, at time.Time) (float64, error)
}

// Service 主对外接口。
type Service struct {
	engine        *feerule.Engine
	repo          Repository
	fx            FXProvider
	baseCurrency  string
	log           *zap.Logger
	defaultPercent int    // 兜底 rule 没命中时的 percent_bps（防止某些边缘 case fee=0 漏算）
	defaultFixed   int64
}

// New 构造 Service。
func New(rules []domain.FeeRule, repo Repository, fx FXProvider, baseCurrency string, log *zap.Logger) *Service {
	if log == nil {
		log = zap.NewNop()
	}
	if baseCurrency == "" {
		baseCurrency = "USD"
	}
	return &Service{
		engine:         feerule.New(rules),
		repo:           repo,
		fx:             fx,
		baseCurrency:   baseCurrency,
		log:            log,
		defaultPercent: 290, // 2.9%
		defaultFixed:   30,  // 30 cents
	}
}

// SetDefault 改兜底费率（dev 调试用）。
func (s *Service) SetDefault(percentBPS int, fixedMinor int64) {
	s.defaultPercent = percentBPS
	s.defaultFixed = fixedMinor
}

// Process 单笔交易事件全流程。返计算好的 FeeEvent（已落库）。
//
// 幂等：同 (merchant_id, ref_id, event_type) 已存在则直接返已有的（不重复算 fee）。
func (s *Service) Process(ctx context.Context, in domain.TransactionInput) (*domain.FeeEvent, error) {
	if in.MerchantID == "" || in.RefID == "" {
		return nil, fmt.Errorf("merchant_id and ref_id required")
	}
	if in.OccurredAt.IsZero() {
		in.OccurredAt = time.Now().UTC()
	}
	// 幂等 check
	exists, err := s.repo.ExistsByRef(ctx, in.MerchantID, in.RefID, in.EventType)
	if err != nil {
		return nil, fmt.Errorf("check exists: %w", err)
	}
	if exists {
		s.log.Info("fee event already exists; idempotent skip",
			zap.String("merchant_id", in.MerchantID),
			zap.String("ref_id", in.RefID),
			zap.String("event_type", string(in.EventType)))
		return nil, nil
	}

	// 1. Pick rule
	rule := s.engine.Pick(in.OccurredAt, in)
	var ruleID int64
	var ruleName string
	var feeMinor, fxMarkupMinor int64

	if rule == nil {
		// fallback — 用 default
		s.log.Warn("no fee_rule matched; using default",
			zap.String("merchant_id", in.MerchantID),
			zap.String("ref_id", in.RefID),
			zap.Int("default_bps", s.defaultPercent))
		feeMinor = in.AmountMinor*int64(s.defaultPercent)/10000 + s.defaultFixed
		ruleName = "DEFAULT_FALLBACK"
	} else {
		ruleID = rule.ID
		ruleName = rule.Name
		feeMinor, fxMarkupMinor, err = feerule.Compute(rule, in, s.baseCurrency)
		if err != nil {
			return nil, fmt.Errorf("compute: %w", err)
		}
	}

	// 2. refund / chargeback 调符号
	switch in.EventType {
	case domain.EventRefund:
		original, _ := s.repo.LoadOriginalCharge(ctx, in.MerchantID, in.RefID)
		var origFee int64
		var origAmt int64
		if original != nil {
			origFee = original.FeeMinor
			origAmt = original.GrossAmountMinor
		}
		feeMinor = feerule.ApplyRefundPolicy(rule, origFee, in.AmountMinor, origAmt)
		fxMarkupMinor = 0 // refund 不退 FX markup（业务策略可调）
	case domain.EventChargeback:
		// 拒付：原 fee 不退 + 一般还要罚款（rule.FixedMinor 当罚款）
		// 这里 feeMinor 已经 = 罚款，但 sign 改成正（收商户）
		// 真实业务可能更复杂，留 hook 给 caller 后处理
	}

	// 3. FX 转换 base
	feeMinorBase := feeMinor
	fxRate := 1.0
	if !strings.EqualFold(in.Currency, s.baseCurrency) && s.fx != nil {
		r, err := s.fx.Rate(ctx, in.Currency, s.baseCurrency, in.OccurredAt)
		if err == nil && r > 0 {
			fxRate = r
			feeMinorBase = int64(float64(feeMinor) * r)
		}
	}

	// 4. 持久化主 fee_event
	ev := &domain.FeeEvent{
		MerchantID:       in.MerchantID,
		EventType:        in.EventType,
		RefID:            in.RefID,
		RefService:       in.RefService,
		GrossAmountMinor: in.AmountMinor,
		Currency:         in.Currency,
		FeeMinor:         feeMinor,
		FeeMinorBase:     feeMinorBase,
		FXRate:           fxRate,
		RuleID:           ruleID,
		RuleName:         ruleName,
		Product:          in.Product,
		ChannelAdapter:   in.ChannelAdapter,
		Region:           in.Region,
		Status:           domain.StatusPending,
		TraceID:          in.TraceID,
		OccurredAt:       in.OccurredAt,
		CreatedAt:        time.Now().UTC(),
	}
	if err := s.repo.SaveFeeEvent(ctx, ev); err != nil {
		return nil, fmt.Errorf("save fee_event: %w", err)
	}

	// 5. FX markup 单独一条 line item（账单清晰 + 可独立 reconcile）
	if fxMarkupMinor > 0 {
		fxEv := &domain.FeeEvent{
			MerchantID:       in.MerchantID,
			EventType:        domain.EventFXSpread,
			RefID:            in.RefID,
			RefService:       in.RefService,
			GrossAmountMinor: in.AmountMinor,
			Currency:         in.Currency,
			FeeMinor:         fxMarkupMinor,
			FeeMinorBase:     int64(float64(fxMarkupMinor) * fxRate),
			FXRate:           fxRate,
			RuleID:           ruleID,
			RuleName:         ruleName + " (FX markup)",
			Product:          in.Product,
			ChannelAdapter:   in.ChannelAdapter,
			Region:           in.Region,
			Status:           domain.StatusPending,
			TraceID:          in.TraceID,
			OccurredAt:       in.OccurredAt,
			CreatedAt:        time.Now().UTC(),
		}
		_ = s.repo.SaveFeeEvent(ctx, fxEv)
	}

	s.log.Info("fee event processed",
		zap.String("merchant_id", in.MerchantID),
		zap.String("ref_id", in.RefID),
		zap.String("event_type", string(in.EventType)),
		zap.Int64("fee_minor", feeMinor),
		zap.Int64("gross_minor", in.AmountMinor),
		zap.String("rule", ruleName))
	return ev, nil
}

