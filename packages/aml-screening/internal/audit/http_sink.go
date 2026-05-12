// http_sink.go — 真接 audit-log 服务的 HTTP sink.
//
// 兜底: BaseURL 空 → 退化成 LogSink (不发 HTTP, 只 log).
// 真生产建议用 payment-util/auditlog.Client (async batch + retry). 这里给一个
// self-contained 实现, 4 个 P0 服务共享代码 (复制粘贴 OK; 各自独立无依赖).

package audit

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

// HTTPConfig audit-log 连接配置
type HTTPConfig struct {
	BaseURL string // eg http://audit-log:8087; 空则退化 LogSink
	Token   string
	Service string // 写到 audit_log.service 字段

	// 可选 — 用默认即可
	BatchSize     int           // 默认 50
	FlushInterval time.Duration // 默认 1s
	BufferSize    int           // 默认 1000
	MaxRetries    int           // 默认 5
	HTTPTimeout   time.Duration // 默认 5s
}

// HTTPSink async batch HTTP audit sink.
type HTTPSink struct {
	cfg     HTTPConfig
	hc      *http.Client
	log     *zap.Logger
	ch      chan Event
	wg      sync.WaitGroup
	stopCh  chan struct{}
	dropped uint64

	// 兜底 fallback (BaseURL 空时用)
	fallback Sink
}

// NewHTTPSink 构造; BaseURL 空时返兜底 LogSink wrap.
func NewHTTPSink(cfg HTTPConfig, log *zap.Logger) *HTTPSink {
	if log == nil {
		log = zap.NewNop()
	}
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
	s := &HTTPSink{
		cfg:    cfg,
		hc:     &http.Client{Timeout: cfg.HTTPTimeout},
		log:    log,
		ch:     make(chan Event, cfg.BufferSize),
		stopCh: make(chan struct{}),
		fallback: LogSink{L: log},
	}
	if cfg.BaseURL != "" {
		s.wg.Add(1)
		go s.flushLoop()
	}
	return s
}

// Emit 实现 Sink. 非阻塞.
func (s *HTTPSink) Emit(ctx context.Context, e Event) error {
	if s.cfg.BaseURL == "" {
		return s.fallback.Emit(ctx, e)
	}
	select {
	case s.ch <- e:
		return nil
	default:
		atomic.AddUint64(&s.dropped, 1)
		// drop 时降级写 zap
		_ = s.fallback.Emit(ctx, e)
		return errors.New("auditlog buffer full")
	}
}

// Dropped 给 metric 看
func (s *HTTPSink) Dropped() uint64 { return atomic.LoadUint64(&s.dropped) }

// Stop 优雅停止 — drain buffer.
func (s *HTTPSink) Stop() {
	if s.cfg.BaseURL == "" {
		return
	}
	close(s.stopCh)
	s.wg.Wait()
}

func (s *HTTPSink) flushLoop() {
	defer s.wg.Done()
	t := time.NewTicker(s.cfg.FlushInterval)
	defer t.Stop()
	batch := make([]Event, 0, s.cfg.BatchSize)

	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := s.sendBatch(batch); err != nil {
			s.log.Error("auditlog HTTP send failed (giving up)",
				zap.Int("batch_size", len(batch)), zap.Error(err))
		}
		batch = batch[:0]
	}

	for {
		select {
		case <-s.stopCh:
			for {
				select {
				case e := <-s.ch:
					batch = append(batch, e)
					if len(batch) >= s.cfg.BatchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		case e := <-s.ch:
			batch = append(batch, e)
			if len(batch) >= s.cfg.BatchSize {
				flush()
			}
		case <-t.C:
			flush()
		}
	}
}

// auditEntryWire 跟 audit-log 服务 /api/v1/audit/log 字段对齐.
type auditEntryWire struct {
	Service      string `json:"service"`
	ActorEmail   string `json:"actor_email"`
	ActorIP      string `json:"actor_ip,omitempty"`
	Action       string `json:"action"`
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id,omitempty"`
	Before       string `json:"before,omitempty"`
	After        string `json:"after,omitempty"`
	Note         string `json:"note,omitempty"`
	TraceID      string `json:"trace_id,omitempty"`
}

// sendBatch 走 audit-log /api/v1/audit/batch 一次 POST 多条; 200 条上限.
func (s *HTTPSink) sendBatch(batch []Event) error {
	// 大 batch 切片到 ≤200
	const maxPerBatch = 200
	for off := 0; off < len(batch); off += maxPerBatch {
		end := off + maxPerBatch
		if end > len(batch) {
			end = len(batch)
		}
		if err := s.sendOneBatch(batch[off:end]); err != nil {
			return err
		}
	}
	return nil
}

func (s *HTTPSink) sendOneBatch(batch []Event) error {
	wires := make([]auditEntryWire, 0, len(batch))
	for _, e := range batch {
		w := auditEntryWire{
			Service:      s.cfg.Service,
			ActorEmail:   e.Actor,
			ActorIP:      e.SourceIP,
			Action:       e.Action,
			ResourceType: "aml_request",
			ResourceID:   e.Subject,
		}
		if b, err := json.Marshal(e.Details); err == nil {
			w.Note = string(b)
		}
		wires = append(wires, w)
	}
	body, err := json.Marshal(map[string]any{"entries": wires})
	if err != nil {
		return err
	}
	url := s.cfg.BaseURL + "/api/v1/audit/batch"
	var lastErr error
	for i := 0; i < s.cfg.MaxRetries; i++ {
		req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if s.cfg.Token != "" {
			req.Header.Set("X-Service-Token", s.cfg.Token)
		}
		resp, err := s.hc.Do(req)
		if err != nil {
			lastErr = err
		} else {
			resp.Body.Close()
			if resp.StatusCode < 300 {
				return nil
			}
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			// 4xx 不重试 (除 429)
			if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 429 {
				return lastErr
			}
		}
		time.Sleep(time.Duration(100*(1<<i)) * time.Millisecond)
	}
	return lastErr
}
