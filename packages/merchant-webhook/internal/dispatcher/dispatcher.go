// Package dispatcher — 商户 webhook 投递引擎。
//
// 工作流：
//
//   1. Enqueue(event) — 业务侧（payment-core / order-core / dispute-service）发事件
//                       → 找到 merchant 订阅了这个 event_type 的 endpoints
//                       → 每个 endpoint 创建一条 Delivery (status=pending)
//
//   2. Worker pool 拉 pending delivery → POST 到 endpoint URL
//                       → 2xx → status=delivered, endpoint.last_delivery_at
//                       → 非 2xx / 网络错 → status=failed, next_retry_at=now+backoff
//
//   3. Retry scheduler 周期扫 next_retry_at < now 的 failed → 重排队
//
//   4. 达 max_retries → status=DLQ + ops 告警
//
// 顺序保证（per-endpoint）：用 endpoint_id 作 worker key，单线程串行处理。
// (高吞吐场景换 Redis Streams + consumer group)

package dispatcher

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"

	"reconcile-system/packages/merchant-webhook/internal/domain"
	"reconcile-system/packages/merchant-webhook/internal/sign"
)

// Repository 抽象。
type Repository interface {
	// Endpoints
	GetEndpoint(ctx context.Context, id int64) (*domain.Endpoint, error)
	GetSecretPlain(ctx context.Context, endpointID int64) (string, error) // 内部调 KMS 解
	ListEndpointsForEvent(ctx context.Context, merchantID, eventType string) ([]*domain.Endpoint, error)
	UpdateLastDelivery(ctx context.Context, endpointID int64, t time.Time) error

	// Events
	SaveEvent(ctx context.Context, e *domain.Event) (int64, error)
	GetEvent(id int64) *domain.Event

	// Deliveries
	CreateDelivery(ctx context.Context, d *domain.Delivery) (int64, error)
	UpdateDelivery(ctx context.Context, id int64, status domain.DeliveryStatus, fields map[string]any) error
	ListReadyToDeliver(ctx context.Context, now time.Time, limit int) ([]*domain.Delivery, error)
	GetDelivery(ctx context.Context, id int64) (*domain.Delivery, error)
}

// Dispatcher 主对外接口。
type Dispatcher struct {
	repo      Repository
	hc        *http.Client
	log       *zap.Logger
	apiVer    string // 写到 X-API-Version header + payload.api_version
}

// New 构造。
func New(repo Repository, apiVer string, log *zap.Logger) *Dispatcher {
	if log == nil {
		log = zap.NewNop()
	}
	if apiVer == "" {
		apiVer = "2026-05-01"
	}
	return &Dispatcher{
		repo:   repo,
		hc:     &http.Client{Timeout: 30 * time.Second},
		log:    log,
		apiVer: apiVer,
	}
}

// Enqueue 业务侧发事件入口。
//
// 路径：保存 Event → 找订阅了 event_type 的 endpoints → 给每个建一条 pending Delivery。
func (d *Dispatcher) Enqueue(ctx context.Context, ev *domain.Event) error {
	if ev.MerchantID == "" || ev.EventType == "" || ev.EventID == "" {
		return fmt.Errorf("merchant_id, event_type, event_id required")
	}
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = time.Now().UTC()
	}
	if ev.APIVersion == "" {
		ev.APIVersion = d.apiVer
	}
	id, err := d.repo.SaveEvent(ctx, ev)
	if err != nil {
		return fmt.Errorf("save event: %w", err)
	}
	ev.ID = id
	endpoints, err := d.repo.ListEndpointsForEvent(ctx, ev.MerchantID, ev.EventType)
	if err != nil {
		return fmt.Errorf("list endpoints: %w", err)
	}
	if len(endpoints) == 0 {
		d.log.Info("no endpoint subscribed",
			zap.String("merchant_id", ev.MerchantID),
			zap.String("event_type", ev.EventType))
		return nil
	}
	now := time.Now().UTC()
	for _, ep := range endpoints {
		_, err := d.repo.CreateDelivery(ctx, &domain.Delivery{
			EventID:     id,
			EndpointID:  ep.ID,
			Attempt:     1,
			Status:      domain.StatusPending,
			NextRetryAt: &now, // 立即可投递
			CreatedAt:   now,
		})
		if err != nil {
			d.log.Warn("create delivery failed",
				zap.Int64("endpoint_id", ep.ID),
				zap.Error(err))
		}
	}
	return nil
}

// DeliverPending worker 循环：每秒扫 ready_to_deliver 的 deliveries 串行处理。
//
// 顺序保证：单 worker 单 endpoint 串行（按 endpoint_id 排序后逐条处理）。
// 高吞吐时可拆多 worker，但同 endpoint 必须只一个 worker（一致性 hash）。
func (d *Dispatcher) DeliverPending(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 50
	}
	now := time.Now().UTC()
	deliveries, err := d.repo.ListReadyToDeliver(ctx, now, limit)
	if err != nil {
		return 0, err
	}
	delivered := 0
	for _, del := range deliveries {
		if err := d.deliverOne(ctx, del); err != nil {
			d.log.Warn("deliver failed",
				zap.Int64("delivery_id", del.ID), zap.Error(err))
			continue
		}
		delivered++
	}
	return delivered, nil
}

// deliverOne 单次投递（POST + 看 response + 状态机迁）。
func (d *Dispatcher) deliverOne(ctx context.Context, del *domain.Delivery) error {
	endpoint, err := d.repo.GetEndpoint(ctx, del.EndpointID)
	if err != nil {
		return fmt.Errorf("get endpoint: %w", err)
	}
	if !endpoint.Active {
		_ = d.repo.UpdateDelivery(ctx, del.ID, domain.StatusFailed,
			map[string]any{"error_message": "endpoint disabled"})
		return nil
	}
	// 取 secret（内部调 KMS 解密）
	secret, err := d.repo.GetSecretPlain(ctx, endpoint.ID)
	if err != nil {
		return fmt.Errorf("decrypt secret: %w", err)
	}
	// 找 event 拿 payload
	ev, err := d.fetchEvent(ctx, del.EventID)
	if err != nil {
		return fmt.Errorf("fetch event: %w", err)
	}
	// 签名
	timestamp := time.Now().Unix()
	sig := sign.Compute(secret, timestamp, ev.Payload)

	// 构造请求
	timeout := time.Duration(endpoint.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "POST", endpoint.URL, bytes.NewReader(ev.Payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "payment-monorepo-webhook/1.0")
	req.Header.Set("X-Webhook-Signature", "t="+strconv.FormatInt(timestamp, 10)+",v1="+sig)
	req.Header.Set("X-API-Version", ev.APIVersion)
	req.Header.Set("X-Webhook-Event-Type", ev.EventType)
	req.Header.Set("X-Webhook-Event-ID", ev.EventID)
	req.Header.Set("X-Webhook-Attempt", strconv.Itoa(del.Attempt))
	if ev.TraceID != "" {
		req.Header.Set("X-Trace-ID", ev.TraceID)
	}

	// 标 inflight
	now := time.Now().UTC()
	_ = d.repo.UpdateDelivery(ctx, del.ID, domain.StatusInflight,
		map[string]any{"sent_at": &now})

	start := time.Now()
	resp, err := d.hc.Do(req)
	dur := time.Since(start)

	if err != nil {
		// 网络错 → 失败 + retry
		return d.markFailed(ctx, del, endpoint, fmt.Sprintf("net error: %v", err),
			0, "", dur)
	}
	defer resp.Body.Close()
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	body := string(bodyBytes)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// 成功
		_ = d.repo.UpdateDelivery(ctx, del.ID, domain.StatusDelivered,
			map[string]any{
				"http_status":  resp.StatusCode,
				"response_body": body,
				"duration_ms":  dur.Milliseconds(),
			})
		_ = d.repo.UpdateLastDelivery(ctx, endpoint.ID, now)
		d.log.Info("webhook delivered",
			zap.Int64("delivery_id", del.ID),
			zap.String("event_type", ev.EventType),
			zap.Int("http_status", resp.StatusCode),
			zap.Duration("dur", dur))
		return nil
	}

	// 非 2xx → 失败 + retry
	return d.markFailed(ctx, del, endpoint,
		fmt.Sprintf("HTTP %d: %s", resp.StatusCode, body),
		resp.StatusCode, body, dur)
}

// markFailed 失败 + 调度重试 / 终态进 DLQ。
func (d *Dispatcher) markFailed(ctx context.Context, del *domain.Delivery, ep *domain.Endpoint,
	msg string, httpStatus int, body string, dur time.Duration) error {
	maxRetries := ep.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 10
	}
	now := time.Now().UTC()
	fields := map[string]any{
		"http_status":   httpStatus,
		"response_body": body,
		"error_message": msg,
		"duration_ms":   dur.Milliseconds(),
	}
	if del.Attempt >= maxRetries {
		// 进 DLQ
		fields["status"] = string(domain.StatusDLQ)
		d.log.Warn("webhook DLQ (max retries exceeded)",
			zap.Int64("delivery_id", del.ID),
			zap.Int64("endpoint_id", ep.ID),
			zap.Int("attempts", del.Attempt))
		return d.repo.UpdateDelivery(ctx, del.ID, domain.StatusDLQ, fields)
	}
	// 安排下次 retry
	nextRetry := now.Add(domain.RetryBackoff(del.Attempt))
	fields["next_retry_at"] = &nextRetry
	fields["attempt"] = del.Attempt + 1
	fields["status"] = string(domain.StatusFailed)
	d.log.Info("webhook retry scheduled",
		zap.Int64("delivery_id", del.ID),
		zap.Int("attempt", del.Attempt),
		zap.Time("next_retry", nextRetry))
	return d.repo.UpdateDelivery(ctx, del.ID, domain.StatusFailed, fields)
}

// fetchEvent 取 event payload。
func (d *Dispatcher) fetchEvent(ctx context.Context, eventID int64) (*domain.Event, error) {
	ev := d.repo.GetEvent(eventID)
	if ev == nil {
		return nil, fmt.Errorf("event %d not found", eventID)
	}
	return ev, nil
}
