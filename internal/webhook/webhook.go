// Package webhook 风控决策 → 商户的异步出站推送。
//
// Stripe Radar / Adyen Thorn 标准能力：商户配一个 URL，risk-manage 在以下
// 事件发生时推送 JSON：
//
//	risk.review.created    verdict=REVIEW（已落 review queue，待运营决议）
//	risk.review.decided    review queue 决议（approve / reject）
//	risk.decision.denied   verdict=DENY 直接拒（让商户客服知道为什么）
//
// 设计要点：
//   - 完全异步：Publish() 入 buffered channel + worker pool，不阻塞 Screen 路径
//   - 重试：失败按指数退避 + jitter 重投递最多 N 次（默认 5）；最终失败落
//     dead_letter 表（生产 PG），admin 后续可手工重发
//   - 签名：HMAC-SHA256(merchant_secret, body) → header X-Risk-Signature；
//     商户用 secret 验签防伪造（同 Stripe 风格）
//   - 幂等：每条 event 带唯一 ID（X-Risk-Event-ID），商户应根据 ID 去重
//   - 商户配置：MerchantID → URL + Secret（端点订阅）；不配 = 不推
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"
)

// EventType 事件类型枚举。
type EventType string

const (
	EventReviewCreated  EventType = "risk.review.created"
	EventReviewDecided  EventType = "risk.review.decided"
	EventDecisionDenied EventType = "risk.decision.denied"
)

// Event 单个推送事件 payload。商户接收的 JSON body 形如：
//
//	{
//	  "id": "evt_<32hex>",
//	  "type": "risk.review.created",
//	  "created_at": "2026-04-28T12:34:56Z",
//	  "merchant_id": "m_xxx",
//	  "data": { ... 业务字段 ... }
//	}
type Event struct {
	ID         string                 `json:"id"`
	Type       EventType              `json:"type"`
	CreatedAt  time.Time              `json:"created_at"`
	MerchantID string                 `json:"merchant_id"`
	Data       map[string]any         `json:"data"`
}

// Subscription 商户的 webhook 订阅。
type Subscription struct {
	MerchantID string
	URL        string // https://merchant.example/risk-webhook
	Secret     string // HMAC secret；推送时商户用 it 验签
	// Events 订阅的事件类型；nil = 全部
	Events []EventType
	// Disabled true → 暂停推送（保留配置不删，便于排障后恢复）
	Disabled bool
}

// SubscriptionStore 商户 webhook 配置查询。生产 PG-backed。
type SubscriptionStore interface {
	Get(ctx context.Context, merchantID string) (*Subscription, error)
}

// MemSubscriptionStore 内存版（dev / 单测）。
type MemSubscriptionStore struct {
	mu   sync.RWMutex
	subs map[string]*Subscription
}

func NewMemSubscriptionStore() *MemSubscriptionStore {
	return &MemSubscriptionStore{subs: make(map[string]*Subscription)}
}

func (s *MemSubscriptionStore) Set(sub *Subscription) {
	if sub == nil || sub.MerchantID == "" {
		return
	}
	cp := *sub
	s.mu.Lock()
	s.subs[sub.MerchantID] = &cp
	s.mu.Unlock()
}

var ErrNotFound = errors.New("webhook: subscription not found")

func (s *MemSubscriptionStore) Get(_ context.Context, merchantID string) (*Subscription, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sub, ok := s.subs[merchantID]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *sub
	return &cp, nil
}

// Publisher 异步推送队列。Publish 入 channel + worker 拉取发送。
//
// 配置约束：
//   - workers 越多吞吐越大；默认 4 足够单 region 中等流量
//   - max_retries 指数退避上限（500ms → 1s → 2s → 4s → 8s）
//   - 队列满时丢弃 + log（fail-open，不阻塞 Screen 路径）
type Publisher struct {
	store      SubscriptionStore
	client     *http.Client
	logger     *zap.Logger
	queue      chan *deliveryJob
	maxRetries int
	dlq        DLQStore // nil-safe：未注入时退到 log + drop
	stopCh     chan struct{}
	stopOnce   sync.Once
}

// SetDLQ 注入死信队列。重试耗尽的 event 落 DLQ 而不是 log + drop。
// nil = 不开 DLQ（保留旧行为）。
func (p *Publisher) SetDLQ(s DLQStore) {
	if p == nil {
		return
	}
	p.dlq = s
}

// DLQ 当前注入的死信存储；nil 时返 nil。给 admin handler 用。
func (p *Publisher) DLQ() DLQStore {
	if p == nil {
		return nil
	}
	return p.dlq
}

// Replay 从 DLQ 拉一条重新投递。给 admin /admin/webhook/dlq/replay 端点用。
// 不删 DLQ 条目（成功投递自然不会再 retry）；失败会更新 attempts/last_error。
// 返 nil = 重投递任务已入队；错误 = DLQ 没找到 / 商户订阅没了等。
func (p *Publisher) Replay(eventID string) error {
	if p == nil || p.dlq == nil {
		return errors.New("DLQ not configured")
	}
	entry, ok := p.dlq.Get(eventID)
	if !ok {
		return errors.New("dlq entry not found")
	}
	if entry.Status == "discarded" {
		return errors.New("entry was discarded; un-discard first")
	}
	// 重新解 event + 找 subscription + 入队 (attempt 清零给一次机会)
	var ev Event
	if err := json.Unmarshal(entry.Body, &ev); err != nil {
		return err
	}
	sub, err := p.store.Get(context.Background(), entry.MerchantID)
	if err != nil || sub == nil {
		return errors.New("merchant subscription gone")
	}
	job := &deliveryJob{sub: sub, event: &ev, attempt: 0}
	select {
	case p.queue <- job:
		return nil
	default:
		return errors.New("queue full")
	}
}

type deliveryJob struct {
	sub      *Subscription
	event    *Event
	attempt  int
	nextSend time.Time
}

// NewPublisher buf 0 → 默认 1024；workers 0 → 4；maxRetries 0 → 5。
func NewPublisher(store SubscriptionStore, logger *zap.Logger, buf, workers, maxRetries int) *Publisher {
	if buf <= 0 {
		buf = 1024
	}
	if workers <= 0 {
		workers = 4
	}
	if maxRetries <= 0 {
		maxRetries = 5
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	p := &Publisher{
		store:      store,
		client:     &http.Client{Timeout: 5 * time.Second},
		logger:     logger,
		queue:      make(chan *deliveryJob, buf),
		maxRetries: maxRetries,
		stopCh:     make(chan struct{}),
	}
	for i := 0; i < workers; i++ {
		go p.worker()
	}
	return p
}

// Publish 异步入队一条事件给 merchant_id。商户没订阅 / 订阅 disabled / event_type
// 不在 Events 列表 → 立即丢弃（不算错）。
func (p *Publisher) Publish(ctx context.Context, merchantID string, e *Event) {
	if p == nil || merchantID == "" || e == nil {
		return
	}
	sub, err := p.store.Get(ctx, merchantID)
	if err != nil || sub == nil || sub.Disabled {
		return
	}
	if !subscribesTo(sub, e.Type) {
		return
	}
	if e.ID == "" {
		e.ID = newEventID()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	if e.MerchantID == "" {
		e.MerchantID = merchantID
	}
	job := &deliveryJob{sub: sub, event: e, attempt: 0, nextSend: time.Now()}
	select {
	case p.queue <- job:
	default:
		p.logger.Warn("webhook queue full; event dropped (fail-open)",
			zap.String("event_id", e.ID), zap.String("merchant_id", merchantID))
	}
}

// Stop 优雅停。已入队的事件尽力发完（worker 自然 drain）；超时由 client.Timeout 控。
func (p *Publisher) Stop() {
	p.stopOnce.Do(func() { close(p.stopCh) })
}

func subscribesTo(sub *Subscription, t EventType) bool {
	if len(sub.Events) == 0 {
		return true
	}
	for _, e := range sub.Events {
		if e == t {
			return true
		}
	}
	return false
}

func (p *Publisher) worker() {
	for {
		select {
		case <-p.stopCh:
			return
		case job := <-p.queue:
			if d := time.Until(job.nextSend); d > 0 {
				select {
				case <-time.After(d):
				case <-p.stopCh:
					return
				}
			}
			err := p.deliver(job)
			if err == nil {
				continue
			}
			job.attempt++
			if job.attempt >= p.maxRetries {
				p.logger.Warn("webhook delivery exhausted retries → DLQ",
					zap.String("event_id", job.event.ID),
					zap.String("merchant_id", job.event.MerchantID),
					zap.Error(err))
				if p.dlq != nil {
					body, _ := json.Marshal(job.event)
					_ = p.dlq.Put(DLQEntry{
						EventID:       job.event.ID,
						MerchantID:    job.event.MerchantID,
						EventType:     job.event.Type,
						Body:          body,
						LastError:     err.Error(),
						Attempts:      job.attempt,
						FirstFailedAt: time.Now().UTC(),
						LastAttemptAt: time.Now().UTC(),
						Status:        "pending",
					})
				}
				continue
			}
			// 指数退避 + jitter
			backoff := time.Duration(1<<uint(job.attempt)) * 500 * time.Millisecond
			jitter := time.Duration(rand.Int64N(int64(backoff / 4)))
			job.nextSend = time.Now().Add(backoff + jitter)
			select {
			case p.queue <- job:
			default:
				p.logger.Warn("retry queue full; event dropped",
					zap.String("event_id", job.event.ID))
			}
		}
	}
}

func (p *Publisher) deliver(job *deliveryJob) error {
	body, err := json.Marshal(job.event)
	if err != nil {
		return err
	}
	sig := signBody(job.sub.Secret, body)
	req, err := http.NewRequest(http.MethodPost, job.sub.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Risk-Event-Id", job.event.ID)
	req.Header.Set("X-Risk-Event-Type", string(job.event.Type))
	req.Header.Set("X-Risk-Signature", sig)
	req.Header.Set("X-Risk-Delivery-Attempt", fmt.Sprintf("%d", job.attempt+1))
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("webhook upstream HTTP %d", resp.StatusCode)
}

// signBody HMAC-SHA256(secret, body) → hex。商户用 secret 验签：
//
//	expected := hmac_sha256(secret, raw_body)
//	hmac.Equal(expected, signature_from_header)
func signBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// newEventID 16-byte hex（同 audit decision_id）。
func newEventID() string {
	var b [16]byte
	if _, err := crand.Read(b[:]); err != nil {
		return "fallback-" + time.Now().UTC().Format("20060102150405.000000000")
	}
	return "evt_" + hex.EncodeToString(b[:])
}
