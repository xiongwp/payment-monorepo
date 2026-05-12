// http_sink.go — HTTP sink for tokenization-vault audit events.
// 详细注释见 aml-screening/internal/audit/http_sink.go (相同实现).

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

type HTTPConfig struct {
	BaseURL       string
	Token         string
	Service       string
	BatchSize     int
	FlushInterval time.Duration
	BufferSize    int
	MaxRetries    int
	HTTPTimeout   time.Duration
}

type HTTPSink struct {
	cfg      HTTPConfig
	hc       *http.Client
	log      *zap.Logger
	ch       chan Event
	wg       sync.WaitGroup
	stopCh   chan struct{}
	dropped  uint64
	fallback Sink
}

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
		cfg: cfg, hc: &http.Client{Timeout: cfg.HTTPTimeout}, log: log,
		ch: make(chan Event, cfg.BufferSize), stopCh: make(chan struct{}),
		fallback: LogSink{L: log},
	}
	if cfg.BaseURL != "" {
		s.wg.Add(1)
		go s.flushLoop()
	}
	return s
}

func (s *HTTPSink) Emit(ctx context.Context, e Event) error {
	if s.cfg.BaseURL == "" {
		return s.fallback.Emit(ctx, e)
	}
	select {
	case s.ch <- e:
		return nil
	default:
		atomic.AddUint64(&s.dropped, 1)
		_ = s.fallback.Emit(ctx, e)
		return errors.New("auditlog buffer full")
	}
}

func (s *HTTPSink) Dropped() uint64 { return atomic.LoadUint64(&s.dropped) }

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
			s.log.Error("auditlog HTTP send failed", zap.Error(err))
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

type auditEntryWire struct {
	Service      string `json:"service"`
	ActorEmail   string `json:"actor_email"`
	ActorIP      string `json:"actor_ip,omitempty"`
	Action       string `json:"action"`
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id,omitempty"`
	Note         string `json:"note,omitempty"`
}

func (s *HTTPSink) sendBatch(batch []Event) error {
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
			Service: s.cfg.Service, ActorEmail: e.Actor, ActorIP: e.SourceIP,
			Action: e.Action, ResourceType: "vault_token", ResourceID: e.Token,
		}
		if b, err := json.Marshal(e.Details); err == nil {
			w.Note = string(b)
		}
		wires = append(wires, w)
	}
	body, _ := json.Marshal(map[string]any{"entries": wires})
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
			if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 429 {
				return lastErr
			}
		}
		time.Sleep(time.Duration(100*(1<<i)) * time.Millisecond)
	}
	return lastErr
}
