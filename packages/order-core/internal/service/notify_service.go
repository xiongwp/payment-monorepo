package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/order-core/internal/channel"
	"github.com/xiongwp/order-core/internal/domain"
	"github.com/xiongwp/order-core/internal/idgen"
	"github.com/xiongwp/order-core/internal/repo"
	"github.com/xiongwp/order-core/internal/sharding"
	"github.com/xiongwp/payment-util/trace"
)

// NotifyService 生成通知记录 + 按 ClientType 路由到合适的 Notifier 发送。
type NotifyService interface {
	// Enqueue 创建一条 NotifyLog，立即尝试发送；失败则安排下次重试
	Enqueue(ctx context.Context, in *NotifyInput) (*domain.NotifyLog, error)
	// Retry 重试单条（worker 用）
	Retry(ctx context.Context, n *domain.NotifyLog) error
}

// NotifyInput 构造通知的入参
type NotifyInput struct {
	PaymentIntentID string
	ChargeID        string
	RefundID        string
	EventID         string
	EventType       string
	ClientType      channel.ClientType
	Target          string // notify_url / device token / ws session_id
	Payload         any    // 任意 JSON 可序列化
	MaxRetries      int
}

type notifyService struct {
	notifRepo repo.NotifyLogRepository
	router    *sharding.Router
	idgen     idgen.IDGenerator
	notifiers map[channel.NotifyChannel]channel.Notifier
	route     channel.NotifyRouter
	logger    *zap.Logger
}

// NewNotifyService 构造
func NewNotifyService(
	nr repo.NotifyLogRepository,
	g idgen.IDGenerator,
	r *sharding.Router,
	notifiers []channel.Notifier,
	route channel.NotifyRouter,
	logger *zap.Logger,
) NotifyService {
	if logger == nil {
		logger = zap.NewNop()
	}
	m := map[channel.NotifyChannel]channel.Notifier{}
	for _, n := range notifiers {
		m[n.Channel()] = n
	}
	if route == nil {
		route = channel.NewDefaultNotifyRouter()
	}
	return &notifyService{notifRepo: nr, router: r, idgen: g, notifiers: m, route: route, logger: logger}
}

// Enqueue 新建通知日志并立刻尝试发送
func (s *notifyService) Enqueue(ctx context.Context, in *NotifyInput) (*domain.NotifyLog, error) {
	if in == nil || in.PaymentIntentID == "" || in.EventType == "" {
		return nil, fmt.Errorf("%w: pi_id + event_type required", domain.ErrValidation)
	}
	payloadBytes, _ := json.Marshal(in.Payload)

	// 按 ClientType 取首选 channel（fallback 链里的第一个）
	chList := s.route.Route(in.ClientType)
	if len(chList) == 0 {
		chList = []channel.NotifyChannel{channel.NotifyChannelHTTP}
	}

	dbIdx, tblIdx := s.router.RouteByPrefixedID(in.PaymentIntentID)
	seq, err := s.idgen.NextID(ctx, idgen.BizTagPaymentIntent)
	if err != nil {
		return nil, err
	}
	id := s.router.FormatID("nl", dbIdx, tblIdx, seq)
	_ = tblIdx

	now := time.Now().UTC()
	maxRetries := in.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 10
	}
	n := &domain.NotifyLog{
		ID:              id,
		PaymentIntentID: in.PaymentIntentID,
		ChargeID:        in.ChargeID,
		RefundID:        in.RefundID,
		EventID:         in.EventID,
		EventType:       in.EventType,
		ClientType:      string(in.ClientType),
		NotifyChannel:   string(chList[0]),
		Target:          in.Target,
		Payload:         payloadBytes,
		Status:          domain.NotifyLogStatusPending,
		MaxRetries:      maxRetries,
		Created:         now,
		Updated:         now,
	}
	if err := s.notifRepo.Create(ctx, n); err != nil {
		return nil, err
	}
	// 立刻发一次
	_ = s.Retry(ctx, n)
	return s.notifRepo.Get(ctx, n.PaymentIntentID, n.ID)
}

// Retry 推送单条通知；失败安排下次重试（指数退避）
func (s *notifyService) Retry(ctx context.Context, n *domain.NotifyLog) error {
	notifier := s.notifiers[channel.NotifyChannel(n.NotifyChannel)]
	if notifier == nil {
		return fmt.Errorf("no notifier for channel %s", n.NotifyChannel)
	}
	req := channel.NotifyRequest{
		EventID:         n.EventID,
		EventType:       n.EventType,
		PaymentIntentID: n.PaymentIntentID,
		ChargeID:        n.ChargeID,
		RefundID:        n.RefundID,
		Target:          n.Target,
		Payload:         n.Payload,
	}
	res, err := notifier.Send(ctx, req)
	now := time.Now().UTC()
	fields := map[string]any{
		"attempt_count": n.AttemptCount + 1,
		"updated":       now,
	}
	if err != nil {
		s.scheduleNextRetry(ctx, n, err.Error(), fields)
		return err
	}
	if res.Success {
		fields["status"] = domain.NotifyLogStatusSucceeded
		fields["completed_at"] = now
		fields["http_status"] = res.HTTPStatus
		_, _ = s.notifRepo.UpdateFields(ctx, n.PaymentIntentID, n.ID, fields)
		return nil
	}
	// 未成功：降级到下一个 channel（如有）或安排重试
	reason := res.ErrorMsg
	if res.ErrorCode != "" {
		reason = res.ErrorCode + ": " + reason
	}
	fields["http_status"] = res.HTTPStatus
	s.scheduleNextRetry(ctx, n, reason, fields)
	return fmt.Errorf("notify failed: %s", reason)
}

func (s *notifyService) scheduleNextRetry(ctx context.Context, n *domain.NotifyLog, reason string, fields map[string]any) {
	attempts := n.AttemptCount + 1
	if attempts >= n.MaxRetries {
		fields["status"] = domain.NotifyLogStatusFailed
		fields["error_msg"] = reason
		_, _ = s.notifRepo.UpdateFields(ctx, n.PaymentIntentID, n.ID, fields)
		return
	}
	// 指数退避：60 * 2^attempts，上限 1h
	backoff := int64(math.Min(float64(60*int64(math.Pow(2, float64(attempts)))), 3600))
	next := time.Now().UTC().Add(time.Duration(backoff) * time.Second)
	fields["status"] = domain.NotifyLogStatusRetrying
	fields["next_retry_at"] = next
	fields["error_msg"] = reason
	_, _ = s.notifRepo.UpdateFields(ctx, n.PaymentIntentID, n.ID, fields)
}

// ─── HTTP Notifier（默认实现，用于 server 端商户回调）─────────────────────────

// HTTPNotifier 用 POST + JSON body 向商户 notify_url 发回调
type HTTPNotifier struct {
	client  *http.Client
	logger  *zap.Logger
	timeout time.Duration
}

// NewHTTPNotifier 构造
func NewHTTPNotifier(timeout time.Duration, logger *zap.Logger) *HTTPNotifier {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &HTTPNotifier{
		client:  &http.Client{Timeout: timeout},
		logger:  logger,
		timeout: timeout,
	}
}

// Channel 返回 http
func (h *HTTPNotifier) Channel() channel.NotifyChannel { return channel.NotifyChannelHTTP }

// Send 发送 HTTP POST
func (h *HTTPNotifier) Send(ctx context.Context, req channel.NotifyRequest) (*channel.NotifyResult, error) {
	if req.Target == "" {
		return &channel.NotifyResult{ErrorCode: "empty_target"}, fmt.Errorf("notify target empty")
	}
	start := time.Now()
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, req.Target, bytes.NewReader(req.Payload))
	if err != nil {
		return &channel.NotifyResult{ErrorMsg: err.Error()}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("X-Event-Id", req.EventID)
	hreq.Header.Set("X-Event-Type", req.EventType)
	for k, v := range req.Headers {
		hreq.Header.Set(k, v)
	}
	resp, err := h.client.Do(hreq)
	elapsed := time.Since(start)
	if err != nil {
		return &channel.NotifyResult{
			ErrorMsg:  err.Error(),
			LatencyMs: elapsed.Milliseconds(),
		}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	raw := map[string]string{"body": string(body)}
	success := resp.StatusCode >= 200 && resp.StatusCode < 300
	return &channel.NotifyResult{
		Success:    success,
		HTTPStatus: resp.StatusCode,
		LatencyMs:  elapsed.Milliseconds(),
		Raw:        raw,
	}, nil
}

// ─── NotifyRetryWorker ───────────────────────────────────────────────────────

// NotifyRetryWorker 扫 NotifyLog.ListDue，重发未完成的通知
type NotifyRetryWorker struct {
	svc      NotifyService
	repo     repo.NotifyLogRepository
	interval time.Duration
	limit    int
	logger   *zap.Logger
}

// NewNotifyRetryWorker 构造
func NewNotifyRetryWorker(svc NotifyService, rp repo.NotifyLogRepository, interval time.Duration, limit int, logger *zap.Logger) *NotifyRetryWorker {
	if interval <= 0 {
		interval = time.Minute
	}
	if limit <= 0 {
		limit = 200
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &NotifyRetryWorker{svc: svc, repo: rp, interval: interval, limit: limit, logger: logger}
}

// Start 阻塞运行
func (w *NotifyRetryWorker) Start(ctx context.Context) {
	w.logger.Info("notify retry worker started", zap.Duration("interval", w.interval))
	t := time.NewTicker(w.interval)
	defer t.Stop()
	run := func() {
		// 每 tick 起 background ctx：trace_id 新生成 + shadow=false。
		// notify retry 出站给商户 / 内部 service，不能漏带 shadow。
		bgCtx, cancel := trace.NewBackground(ctx, "notify-retry-worker", w.logger, w.interval)
		defer cancel()
		list, err := w.repo.ListDue(bgCtx, time.Now().UTC(), w.limit)
		if err != nil {
			trace.Logger(bgCtx, w.logger).Warn("notify list due failed", zap.Error(err))
			return
		}
		if len(list) == 0 {
			return
		}
		// wave L: cap concurrent retries. Unbounded goroutine spawn at e.g.
		// 200 batch × 3 workers = 600 concurrent HTTP requests competes for
		// NIC/CPU and can saturate merchant endpoints during retry storms.
		const maxConcurrent = 32
		sem := make(chan struct{}, maxConcurrent)
		var wg sync.WaitGroup
		for _, n := range list {
			wg.Add(1)
			sem <- struct{}{}
			go func(n *domain.NotifyLog) {
				defer wg.Done()
				defer func() { <-sem }()
				if err := w.svc.Retry(bgCtx, n); err != nil {
					trace.Logger(bgCtx, w.logger).Debug("notify retry result", zap.String("id", n.ID), zap.Error(err))
				}
			}(n)
		}
		wg.Wait()
	}
	run()
	for {
		select {
		case <-ctx.Done():
			w.logger.Info("notify retry worker stopped")
			return
		case <-t.C:
			run()
		}
	}
}
