package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/time/rate"

	"github.com/xiongwp/payment-channel/internal/channel"
	"github.com/xiongwp/payment-channel/internal/domain"
	"github.com/xiongwp/payment-channel/internal/metrics"
	"github.com/xiongwp/payment-channel/internal/repo"
)

// Forwarder 把规范化 WebhookEvent 转发给 order-core（或其它下游）。
type Forwarder interface {
	Forward(ctx context.Context, adapter string, evt *channel.WebhookEvent) error
}

// NoopForwarder 开发态占位：只打日志。
type NoopForwarder struct{ Logger *zap.Logger }

func (n *NoopForwarder) Forward(_ context.Context, adapter string, evt *channel.WebhookEvent) error {
	n.Logger.Info("noop forward webhook",
		zap.String("adapter", adapter),
		zap.String("pi_id", evt.PiID),
		zap.String("event_type", evt.EventType))
	return nil
}

// WebhookOptions 控制 WebhookService 的安全参数。零值 = 安全默认（开 replay
// 检查 + per-adapter 限流不开）。
type WebhookOptions struct {
	// ReplayWindow 允许 webhook timestamp 与服务器现在时间偏差的最大值。
	// 超出 window → 拒绝（防 replay attack 重放老 webhook payload）。
	// 0 → 默认 15min；负数 → 关闭检查（不推荐）。
	ReplayWindow time.Duration

	// AdapterRPS / AdapterBurst per-adapter 入口限流。攻击者用合法签名洪水攻击
	// 时挡住 DB amplification + downstream forward 风暴。
	// AdapterRPS <= 0 → 不限流。
	AdapterRPS   float64
	AdapterBurst int
}

const defaultReplayWindow = 15 * time.Minute

type WebhookService struct {
	reg       channel.Registry
	whRepo    repo.WebhookRawRepository
	forwarder Forwarder
	logger    *zap.Logger
	now       func() time.Time

	replayWindow time.Duration

	// adapterLimitMu 保护 adapterLimits（写锁仅在新 adapter 首次出现）。
	adapterLimitMu sync.RWMutex
	adapterRPS     rate.Limit
	adapterBurst   int
	adapterLimits  map[string]*rate.Limiter
}

// NewWebhookService 用安全默认值构造（replay window=15min，无 per-adapter 限流）。
// 生产部署建议改用 NewWebhookServiceWithOptions。
func NewWebhookService(reg channel.Registry, whRepo repo.WebhookRawRepository, f Forwarder, logger *zap.Logger) *WebhookService {
	return NewWebhookServiceWithOptions(reg, whRepo, f, logger, WebhookOptions{})
}

// NewWebhookServiceWithOptions 完整构造。opts.ReplayWindow=0 → 用默认 15min；
// 负值禁用 replay 检查（不推荐，仅 e2e 用）。
func NewWebhookServiceWithOptions(
	reg channel.Registry,
	whRepo repo.WebhookRawRepository,
	f Forwarder,
	logger *zap.Logger,
	opts WebhookOptions,
) *WebhookService {
	rw := opts.ReplayWindow
	if rw == 0 {
		rw = defaultReplayWindow
	}
	burst := opts.AdapterBurst
	if burst <= 0 && opts.AdapterRPS > 0 {
		burst = int(opts.AdapterRPS)
		if burst < 1 {
			burst = 1
		}
	}
	return &WebhookService{
		reg:           reg,
		whRepo:        whRepo,
		forwarder:     f,
		logger:        logger,
		now:           time.Now,
		replayWindow:  rw,
		adapterRPS:    rate.Limit(opts.AdapterRPS),
		adapterBurst:  burst,
		adapterLimits: make(map[string]*rate.Limiter),
	}
}

// allowAdapter per-adapter token bucket。adapterRPS<=0 → 直接允许。
func (s *WebhookService) allowAdapter(adapterName string) bool {
	if s.adapterRPS <= 0 {
		return true
	}
	s.adapterLimitMu.RLock()
	lim, ok := s.adapterLimits[adapterName]
	s.adapterLimitMu.RUnlock()
	if !ok {
		s.adapterLimitMu.Lock()
		if lim, ok = s.adapterLimits[adapterName]; !ok {
			lim = rate.NewLimiter(s.adapterRPS, s.adapterBurst)
			s.adapterLimits[adapterName] = lim
		}
		s.adapterLimitMu.Unlock()
	}
	return lim.Allow()
}

// Ingest HTTP 入口 POST /wh/{adapter} 收到回调后调本方法。
//
// 检查顺序（按成本由低到高排）：
//  1. adapter 注册存在性（map lookup）
//  2. per-adapter rate limit（in-memory token bucket）—— 抵御洪水攻击的最便宜
//     防线；放在 parse 之前，攻击者用 RSA 签名也烧不动 CPU。
//  3. parse + signature 验证（adapter 内部）
//  4. replay window 检查（时间戳与现在偏差 ≤ replayWindow）
//  5. INSERT webhook_raw（保留 audit；含 SignatureOK 字段，便于事后取证）
//  6. forward → order-core（仅签名 OK 时；否则记 audit + 返回 sig fail）
func (s *WebhookService) Ingest(ctx context.Context, adapterName string, headers map[string]string, body []byte) error {
	ad, ok := s.reg.Get(adapterName)
	if !ok {
		return fmt.Errorf("adapter %s not registered", adapterName)
	}
	if !s.allowAdapter(adapterName) {
		metrics.WebhookReceivedTotal.WithLabelValues(adapterName, "n/a", "rate_limited").Inc()
		// 不调 ParseWebhook（昂贵签名验证）；不写 DB；返回特定错误让上层回 429。
		return domain.ErrWebhookRateLimited
	}
	evt, err := ad.ParseWebhook(headers, body)
	if err != nil {
		metrics.WebhookReceivedTotal.WithLabelValues(adapterName, "false", "parse_error").Inc()
		return err
	}
	// Replay window 检查：拒绝过老 / 过新（时钟漂移）的 payload。
	// adapter 未填 Timestamp（零值）→ 跳过检查 + 打一次 Warn（提示该 adapter 升级）。
	if s.replayWindow > 0 && !evt.Timestamp.IsZero() {
		drift := s.now().Sub(evt.Timestamp)
		if drift < 0 {
			drift = -drift
		}
		if drift > s.replayWindow {
			metrics.WebhookReceivedTotal.WithLabelValues(adapterName, fmt.Sprintf("%t", evt.SignatureOK), "replay_rejected").Inc()
			s.logger.Warn("webhook replay window exceeded",
				zap.String("adapter", adapterName),
				zap.String("pi_id", evt.PiID),
				zap.Duration("drift", drift),
				zap.Duration("window", s.replayWindow))
			return domain.ErrWebhookReplay
		}
	} else if evt.Timestamp.IsZero() {
		// 仅一次性提示（在每条请求上 Warn 噪音太大；保留 Debug 级降级）。
		s.logger.Debug("webhook event has zero Timestamp; replay window check skipped",
			zap.String("adapter", adapterName))
	}
	if evt.EventID == "" {
		// 渠道未给 event_id 时用 body hash 兜底。
		sum := sha256.Sum256(body)
		evt.EventID = hex.EncodeToString(sum[:])
	}
	dedup := fmt.Sprintf("%s:%s", adapterName, evt.EventID)
	wh := &domain.WebhookRaw{
		PiID:        evt.PiID,
		Adapter:     adapterName,
		EventID:     evt.EventID,
		EventType:   evt.EventType,
		DedupeKey:   dedup,
		SignatureOK: evt.SignatureOK,
		Headers:     headers,
		Body:        string(body),
		ReceivedAt:  s.now(),
	}
	sigLabel := fmt.Sprintf("%t", evt.SignatureOK)
	if err := s.whRepo.Insert(ctx, wh); err != nil {
		if errors.Is(err, domain.ErrWebhookAlreadySeen) {
			metrics.WebhookReceivedTotal.WithLabelValues(adapterName, sigLabel, "dedup").Inc()
			return nil
		}
		metrics.WebhookReceivedTotal.WithLabelValues(adapterName, sigLabel, "insert_error").Inc()
		return err
	}
	metrics.WebhookReceivedTotal.WithLabelValues(adapterName, sigLabel, "new").Inc()

	if !evt.SignatureOK {
		// 恶意或配置错误：记一行 signature_fail 日志，不转发。
		s.logger.Warn("webhook signature check failed",
			zap.String("adapter", adapterName),
			zap.String("pi_id", evt.PiID))
		return domain.ErrChannelSignatureFail
	}

	ferr := s.forwarder.Forward(ctx, adapterName, evt)
	_ = s.whRepo.MarkForwarded(ctx, evt.PiID, wh.ID, ferr)
	if ferr != nil {
		metrics.WebhookForwardTotal.WithLabelValues(adapterName, "failed").Inc()
	} else {
		metrics.WebhookForwardTotal.WithLabelValues(adapterName, "succeeded").Inc()
	}
	return ferr
}
