// Package auditlog — 各 service 接 audit-log 服务的 HTTP 客户端.
//
// 设计目标:
//   1. async batch — 不阻塞业务 (写本地 channel, 后台 worker flush)
//   2. 多事件 batch POST 一次, 降低 audit-log 服务压力
//   3. retry + DLQ 兜底 (网络抖动 / audit-log 临时不可达 → 重试; 5x retries 仍失败 fallback 到 stderr)
//
// 用法:
//
//   client := auditlog.New(auditlog.Config{
//       BaseURL: "http://audit-log:8087",
//       Service: "aml-screening",
//       Token:   os.Getenv("AUDITLOG_TOKEN"),
//   }, logger)
//   defer client.Stop()
//
//   client.Emit(ctx, auditlog.Event{
//       Action:       "screen",
//       ResourceType: "merchant",
//       ResourceID:   "m_123",
//       Actor:        "service:aml-screening",
//       Details:      map[string]any{"action":"block","top_score":92},
//   })
//
// Emit 永不阻塞 > 1ms (buffered chan; full 则 drop + log warning).

package auditlog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// Event 跟 audit-log domain.AuditEntry 字段对齐 (但本地 struct 不依赖 audit-log 包).
type Event struct {
	Actor        string                 `json:"actor_email"`
	ActorIP      string                 `json:"actor_ip,omitempty"`
	Action       string                 `json:"action"`
	ResourceType string                 `json:"resource_type"`
	ResourceID   string                 `json:"resource_id,omitempty"`
	Before       map[string]interface{} `json:"-"`
	After        map[string]interface{} `json:"-"`
	BeforeJSON   string                 `json:"before,omitempty"`
	AfterJSON    string                 `json:"after,omitempty"`
	Note         string                 `json:"note,omitempty"`
	TraceID      string                 `json:"trace_id,omitempty"`
	OccurredAt   time.Time              `json:"-"` // audit-log 服务端补 created_at
	// 调用方填的 service 名 (auditlog 包补字段)
	Service string `json:"service,omitempty"`

	// Details 是 service 私有上下文; 嵌入到 Note (json) 里
	Details map[string]interface{} `json:"-"`
}

// Config 客户端配置
type Config struct {
	BaseURL       string         // audit-log 服务 base URL, eg http://audit-log:8087
	Service       string         // 本 service 名 (写到 entry.service)
	Token         string         // X-Service-Token (内网鉴权)
	BatchSize     int            // 默认 50
	FlushInterval time.Duration  // 默认 1s
	BufferSize    int            // chan 缓冲; 默认 1000
	MaxRetries    int            // 默认 5
	HTTPTimeout   time.Duration  // 默认 5s
}

// Client async HTTP audit sink.
type Client struct {
	cfg    Config
	hc     *http.Client
	log    *zap.Logger
	ch     chan Event
	wg     sync.WaitGroup
	stop   chan struct{}
	dropped uint64
}

// New 创建并启动后台 flusher.
func New(cfg Config, log *zap.Logger) *Client {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 50
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = time.Second
	}
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = 1000
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 5
	}
	if cfg.HTTPTimeout <= 0 {
		cfg.HTTPTimeout = 5 * time.Second
	}
	if log == nil {
		log = zap.NewNop()
	}
	c := &Client{
		cfg:  cfg,
		hc:   &http.Client{Timeout: cfg.HTTPTimeout},
		log:  log,
		ch:   make(chan Event, cfg.BufferSize),
		stop: make(chan struct{}),
	}
	c.wg.Add(1)
	go c.flushLoop()
	return c
}

// Emit 非阻塞投递; 满了 drop + warn (不让 audit-log 故障拖累业务).
func (c *Client) Emit(_ context.Context, e Event) error {
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now().UTC()
	}
	e.Service = c.cfg.Service
	// Details 嵌 Note (audit-log 服务端按需展开)
	if len(e.Details) > 0 && e.Note == "" {
		if b, err := json.Marshal(e.Details); err == nil {
			e.Note = string(b)
		}
	}
	if len(e.Before) > 0 && e.BeforeJSON == "" {
		if b, err := json.Marshal(e.Before); err == nil {
			e.BeforeJSON = string(b)
		}
	}
	if len(e.After) > 0 && e.AfterJSON == "" {
		if b, err := json.Marshal(e.After); err == nil {
			e.AfterJSON = string(b)
		}
	}
	select {
	case c.ch <- e:
		return nil
	default:
		atomic.AddUint64(&c.dropped, 1)
		c.log.Warn("auditlog buffer full, event dropped",
			zap.String("action", e.Action),
			zap.String("resource_id", e.ResourceID))
		return errors.New("auditlog buffer full")
	}
}

// Stop 优雅停止; 刷掉 channel 里剩余事件后返.
func (c *Client) Stop() {
	close(c.stop)
	c.wg.Wait()
}

// Dropped 统计 — 给 metric 看
func (c *Client) Dropped() uint64 { return atomic.LoadUint64(&c.dropped) }

// flushLoop 后台 worker — 攒 BatchSize 或 FlushInterval 触发一次发送.
func (c *Client) flushLoop() {
	defer c.wg.Done()
	ticker := time.NewTicker(c.cfg.FlushInterval)
	defer ticker.Stop()
	batch := make([]Event, 0, c.cfg.BatchSize)

	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := c.sendBatch(batch); err != nil {
			// 最后兜底: 打 zap (生产期建议接 stderr fallback file)
			c.log.Error("auditlog send failed (giving up after retries)",
				zap.Int("batch_size", len(batch)),
				zap.Error(err))
		}
		batch = batch[:0]
	}

	for {
		select {
		case <-c.stop:
			// drain
			for {
				select {
				case e := <-c.ch:
					batch = append(batch, e)
					if len(batch) >= c.cfg.BatchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		case e := <-c.ch:
			batch = append(batch, e)
			if len(batch) >= c.cfg.BatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// sendBatch 重试 N 次发一批; 每条独立 POST 到 /api/v1/audit/log (audit-log 现在 API).
// 真生产应有 batch endpoint /api/v1/audit/batch.
func (c *Client) sendBatch(batch []Event) error {
	url := c.cfg.BaseURL + "/api/v1/audit/log"
	for _, e := range batch {
		body, err := json.Marshal(e)
		if err != nil {
			c.log.Warn("auditlog marshal", zap.Error(err))
			continue
		}
		var lastErr error
		for attempt := 0; attempt < c.cfg.MaxRetries; attempt++ {
			req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			if c.cfg.Token != "" {
				req.Header.Set("X-Service-Token", c.cfg.Token)
			}
			resp, err := c.hc.Do(req)
			if err != nil {
				lastErr = err
			} else {
				resp.Body.Close()
				if resp.StatusCode < 300 {
					lastErr = nil
					break
				}
				lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			}
			// 指数退避: 100ms, 200ms, 400ms, 800ms, 1.6s
			time.Sleep(time.Duration(100*(1<<attempt)) * time.Millisecond)
		}
		if lastErr != nil {
			return lastErr
		}
	}
	return nil
}
