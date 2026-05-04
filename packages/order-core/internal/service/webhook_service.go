package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/order-core/internal/channel"
	"github.com/xiongwp/order-core/internal/domain"
	"github.com/xiongwp/order-core/internal/idgen"
	"github.com/xiongwp/order-core/internal/repo"
	"github.com/xiongwp/payment-util/shadow"
)

// WebhookDirection 通知方向
type WebhookDirection string

const (
	WebhookDirectionChannel WebhookDirection = "WEBHOOK_DIRECTION_CHANNEL"
	WebhookDirectionClient  WebhookDirection = "WEBHOOK_DIRECTION_CLIENT"
)

// IngestWebhookInput 接收异步通知入参
type IngestWebhookInput struct {
	Direction   WebhookDirection
	ChannelName string
	Headers     map[string]string
	Body        []byte
}

// WebhookService 异步通知处理入口
//
// 架构：payment-core 把各渠道（Stripe / Alipay / GCash / ShopeePay / GrabPay ...）的原始
// 通知规范化后转发到 order-core 这里；order-core 按 event_type 推进 PI / Charge / Refund。
type WebhookService interface {
	Ingest(ctx context.Context, in *IngestWebhookInput) (*channel.WebhookEvent, error)
}

type webhookService struct {
	registry    channel.PaymentChannelRegistry
	piSvc       PaymentIntentService
	refundSvc   RefundService
	reconcile   *RefundReconcileService
	chargeRepo  repo.ChargeRepository
	piRepo      repo.PaymentIntentRepository
	refundRepo  repo.RefundRepository
	inboundRepo repo.InboundWebhookRepository
	idg         idgen.IDGenerator
	// accounting 可为 nil（accounting 接入关闭或单测）；非 nil 时在 charge/refund
	// 成功分支后 best-effort 向分片 outbox 写 HybridDoubleEntryBooking 请求。
	accounting AccountingOutboxService
	dedup      sync.Map // event_id → time.Time（插入时间），快速去重 + TTL 淘汰
	logger     *zap.Logger
}

// NewWebhookService 构造
//
// inboundRepo + idg 可为 nil（向后兼容，单元测试常用）；非 nil 时启用基于
// (channel_name, event_id) 的入站幂等去重。
//
// accounting 可为 nil；非 nil 时在 charge/refund 成功后把
// HybridDoubleEntryBooking 请求写到分片 outbox，由 accounting_outbox_worker
// 异步投递给 accounting-system。
func NewWebhookService(
	reg channel.PaymentChannelRegistry,
	piSvc PaymentIntentService,
	rfSvc RefundService,
	reconcile *RefundReconcileService,
	piRepo repo.PaymentIntentRepository,
	chargeRepo repo.ChargeRepository,
	refundRepo repo.RefundRepository,
	inboundRepo repo.InboundWebhookRepository,
	idg idgen.IDGenerator,
	accounting AccountingOutboxService,
	logger *zap.Logger,
) WebhookService {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &webhookService{
		registry: reg, piSvc: piSvc, refundSvc: rfSvc, reconcile: reconcile,
		chargeRepo: chargeRepo, piRepo: piRepo, refundRepo: refundRepo,
		inboundRepo: inboundRepo, idg: idg,
		accounting: accounting,
		logger:     logger,
	}
}

// Ingest 规范化 + 分发
func (s *webhookService) Ingest(ctx context.Context, in *IngestWebhookInput) (*channel.WebhookEvent, error) {
	if in == nil || in.ChannelName == "" {
		return nil, fmt.Errorf("%w: channel_name required", domain.ErrValidation)
	}
	ch := s.registry.Get(in.ChannelName)
	if ch == nil {
		return nil, fmt.Errorf("%w: channel not registered: %s", domain.ErrValidation, in.ChannelName)
	}
	ev, err := ch.ParseWebhook(ctx, in.Headers, in.Body)
	if err != nil {
		return nil, fmt.Errorf("parse webhook: %w", err)
	}
	if ev == nil {
		return nil, fmt.Errorf("%w: channel returned nil event", domain.ErrValidation)
	}

	// 幂等去重：按 (channel_name, event_id) 唯一约束 INSERT。
	//
	// **关键修复**：旧逻辑只看"行是否存在"，没看 process_status —— 第一次收到
	// 后落库 PENDING，处理 PI 状态机或 outbox 入队过程中崩溃 → channel 重发
	// 第二次 → 看到 dup 直接 return → process_status 永远卡 PENDING →
	// PI 永远没推进 → 客户已支付但 accounting 没记账 → merchant 收不到钱。
	//
	// 修：dup 时根据 process_status 决定是否继续。
	//   - SUCCEEDED：上次处理完成，本次直接 skip（真正的幂等命中）
	//   - PENDING / FAILED：上次半截崩了，本次 retry 推进；本次成功后 MarkProcessed
	//   - DUPLICATE 标记：只为了审计，不影响判断
	// 没有 ev.EventID 的渠道事件跳过去重（应通过监控告警驱动渠道侧补 event_id）。
	var inboundRow *domain.InboundWebhook
	if s.inboundRepo != nil && s.idg != nil && ev.EventID != "" {
		row, dupErr := s.recordInbound(ctx, in, ev)
		switch {
		case errors.Is(dupErr, domain.ErrInboundWebhookDuplicate):
			if row != nil && row.ProcessStatus == domain.InboundWebhookSucceeded {
				s.logger.Info("webhook duplicate (already succeeded), skip",
					zap.String("channel", in.ChannelName),
					zap.String("event_id", ev.EventID),
					zap.String("event_type", ev.EventType),
					zap.String("first_seen", row.Created.Format("2006-01-02T15:04:05Z")))
				return ev, nil
			}
			// PENDING / FAILED → 上次未完成，本次推进。inboundRow 用现有的 row。
			s.logger.Warn("webhook duplicate but previous run unfinished, retrying processing",
				zap.String("channel", in.ChannelName),
				zap.String("event_id", ev.EventID),
				zap.String("prev_status", string(row.ProcessStatus)))
			inboundRow = row
		case dupErr != nil:
			// 落库失败不阻塞业务（事件本身可能正常推进），但要告警。
			s.logger.Error("inbound webhook persist failed",
				zap.Error(dupErr),
				zap.String("channel", in.ChannelName),
				zap.String("event_id", ev.EventID))
		default:
			inboundRow = row
		}
	}

	// 幂等去重 —— 用 in-memory set 快速拦截重复回调。
	// 真正的兜底是 piRepo.UpdateStatus 的状态机（Succeeded→Succeeded 会 reject），
	// 这里只是避免重复回调多余的 DB 查询和日志噪声。
	if ev.EventID != "" {
		now := time.Now()
		if _, loaded := s.dedup.LoadOrStore(ev.EventID, now); loaded {
			s.logger.Info("webhook duplicate, skip",
				zap.String("event_id", ev.EventID), zap.String("pi_id", ev.PaymentIntentID))
			return ev, nil
		}
		// P1-3: 异步清理过期条目（> 1h），防止 sync.Map 无限增长
		if now.UnixNano()%100 == 0 { // ~1% 概率触发清理（不依赖时钟分钟）
			go s.evictStaleDedup(now.Add(-1 * time.Hour))
		}
	}

	s.logger.Info("webhook event",
		zap.String("channel", in.ChannelName),
		zap.String("event_id", ev.EventID),
		zap.String("event_type", ev.EventType),
		zap.String("pi_id", ev.PaymentIntentID))

	// 按 event_type 分发：charge.succeeded / charge.failed /
	// payment_intent.requires_action / refund.succeeded / refund.failed / ...
	switch ev.EventType {
	case "charge.succeeded", "payment_intent.succeeded":
		// 把 PI 推进到 succeeded；若支付单/订单已过期 → 自动补偿退款
		if s.isLatePayment(ctx, ev.PaymentIntentID) {
			if s.reconcile != nil {
				_ = s.reconcile.OnLateChargeSuccess(ctx, ev.PaymentIntentID, ev.ChargeID, ev.Amount)
			}
		} else {
			pi, err := s.piSvc.MarkSucceeded(ctx, ev.PaymentIntentID, ev.ChargeID, ev.Amount)
			if err == nil && pi != nil && s.accounting != nil {
				if enqErr := s.accounting.EnqueueChargeSucceeded(ctx, pi, ev.ChargeID, ev.Amount); enqErr != nil {
					s.logger.Error("accounting outbox enqueue (charge) failed",
						zap.Error(enqErr),
						zap.String("pi_id", pi.ID),
						zap.String("charge_id", ev.ChargeID))
				}
			}
		}
	case "charge.failed", "payment_intent.failed":
		_, _ = s.piSvc.MarkFailed(ctx, ev.PaymentIntentID, ev.ChargeID, "", "channel reported failure")
	case "refund.succeeded":
		if ev.RefundID != "" {
			rf, err := s.refundSvc.MarkSucceeded(ctx, ev.PaymentIntentID, ev.RefundID)
			if err == nil && rf != nil && s.accounting != nil {
				if pi, piErr := s.piRepo.Get(ctx, ev.PaymentIntentID); piErr == nil && pi != nil {
					if enqErr := s.accounting.EnqueueRefundSucceeded(ctx, pi, rf); enqErr != nil {
						s.logger.Error("accounting outbox enqueue (refund) failed",
							zap.Error(enqErr),
							zap.String("pi_id", pi.ID),
							zap.String("refund_id", rf.ID))
					}
				}
			}
		}
	case "refund.failed":
		if ev.RefundID != "" {
			_, _ = s.refundSvc.MarkFailed(ctx, ev.PaymentIntentID, ev.RefundID, "channel reported refund failure")
		}
	case "payment_intent.requires_action":
		_, _ = s.piSvc.RequireAction(ctx, ev.PaymentIntentID)
	default:
		s.logger.Warn("unhandled webhook event_type", zap.String("event_type", ev.EventType))
	}
	if inboundRow != nil {
		_ = s.inboundRepo.MarkProcessed(ctx, inboundRow, domain.InboundWebhookSucceeded, "")
	}
	return ev, nil
}

// recordInbound 持久化入站 webhook 用于幂等去重 + 审计回溯。
// 命中 (channel_name, event_id) 唯一约束时返回 ErrInboundWebhookDuplicate +
// 已存在的行（让上层据此跳过状态推进）。
func (s *webhookService) recordInbound(ctx context.Context, in *IngestWebhookInput, ev *channel.WebhookEvent) (*domain.InboundWebhook, error) {
	seq, err := s.idg.NextID(ctx, idgen.BizTagInboundWebhook)
	if err != nil {
		return nil, fmt.Errorf("idgen for inbound webhook: %w", err)
	}
	headers := domain.Metadata{}
	for k, v := range in.Headers {
		headers[k] = v
	}
	// 体积保护：超过 64KB 的 body 截断保留前缀（足够审计排查，避免分片表撑爆）
	body := in.Body
	const maxBody = 64 * 1024
	if len(body) > maxBody {
		body = body[:maxBody]
	}
	inwhID, err := shadow.EncodeIDStr(ctx, shadow.IDTypeOrderInboundWebhook, 0, seq)
	if err != nil {
		return nil, fmt.Errorf("encode inbound webhook id: %w", err)
	}
	row := &domain.InboundWebhook{
		ID:              inwhID,
		ChannelName:     in.ChannelName,
		EventID:         ev.EventID,
		EventType:       ev.EventType,
		PaymentIntentID: ev.PaymentIntentID,
		ChargeID:        ev.ChargeID,
		RefundID:        ev.RefundID,
		// channel.WebhookEvent 当前未携带 signature 验证状态；payment-channel
		// 已经在自己的 webhook handler 内部完成验签，转发到这里时认定可信。
		SignatureOK:   true,
		Headers:       headers,
		Body:          body,
		ProcessStatus: domain.InboundWebhookPending,
	}
	return s.inboundRepo.Insert(ctx, row)
}

// _ keeps the json import alive for future webhook payload normalization.
var _ = json.RawMessage(nil)

// isLatePayment 判断 PI / Charge 是否已经过期 / 失败，若是则走差错补偿
func (s *webhookService) isLatePayment(ctx context.Context, piID string) bool {
	pi, err := s.piRepo.Get(ctx, piID)
	if err != nil {
		return false
	}
	return pi.Status == domain.PIStatusCanceled || pi.Status == domain.PIStatusFailed
}

// evictStaleDedup 清理 sync.Map 中超过 cutoff 的旧条目，防止无限增长
func (s *webhookService) evictStaleDedup(cutoff time.Time) {
	s.dedup.Range(func(key, value interface{}) bool {
		if ts, ok := value.(time.Time); ok && ts.Before(cutoff) {
			s.dedup.Delete(key)
		}
		return true
	})
}
