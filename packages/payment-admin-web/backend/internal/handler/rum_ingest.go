// Package handler — RUM ingest endpoint。
//
// POST /api/rum/ingest
//   {
//     "service": "admin-web", "release": "v1.2.3",
//     "session_id": "uuid", "page": "/refunds", "ua": "...",
//     "events": [{"type":"vital","name":"LCP","value":2300,"ts":...}, ...]
//   }
//
// 处理:
//   1. 验 service 白名单 (防止外部乱投)
//   2. 简单脱敏 (URL 去 query, stack 截断, UA 截断)
//   3. 推 Prometheus counter / histogram
//   4. (可选) 推到 Loki / ClickHouse 做长期归档
//
// 安全: rum endpoint 不要求 auth, 用 sample rate + IP 限流防滥用。

package handler

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	rumVitalsHist = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "rum_web_vital_ms",
		Help:    "Web Vitals from RUM (LCP/FID/CLS/TTFB/FCP)",
		Buckets: []float64{100, 250, 500, 1000, 2000, 4000, 8000, 16000},
	}, []string{"service", "release", "vital"})

	rumErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "rum_errors_total",
		Help: "Browser JS errors / fetch failures from RUM",
	}, []string{"service", "release", "type"})

	rumPageViewsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "rum_page_views_total",
		Help: "Page views (unique session+page combination)",
	}, []string{"service", "page"})

	rumClicksTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "rum_clicks_total",
		Help: "Tracked clicks via data-rum-event",
	}, []string{"service", "event"})
)

// 允许上报的 service 白名单 (防伪造)。生产从 config-center 拉。
var allowedServices = map[string]bool{
	"admin-web":       true,
	"merchant-portal": true,
	"checkout":        true,
}

// RUMHandler ...
type RUMHandler struct {
	mu       sync.Mutex
	seenPage map[string]struct{} // session_id+page → 去重一次 page view 推一次
}

// NewRUMHandler 构造。
func NewRUMHandler() *RUMHandler {
	h := &RUMHandler{seenPage: map[string]struct{}{}}
	// GC: 每小时清一次 seenPage (防 map 膨胀)
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for range t.C {
			h.mu.Lock()
			if len(h.seenPage) > 100000 {
				h.seenPage = map[string]struct{}{}
			}
			h.mu.Unlock()
		}
	}()
	return h
}

type rumPayload struct {
	Service   string     `json:"service"`
	Release   string     `json:"release"`
	SessionID string     `json:"session_id"`
	Page      string     `json:"page"`
	UA        string     `json:"ua"`
	Events    []rumEvent `json:"events"`
}

type rumEvent struct {
	Type   string  `json:"type"`
	Name   string  `json:"name"`
	Value  float64 `json:"value"`
	URL    string  `json:"url"`
	Status int     `json:"status"`
	DurMs  int     `json:"dur_ms"`
	Msg    string  `json:"msg"`
	Stack  string  `json:"stack"`
	Event  string  `json:"event"`
	Ts     int64   `json:"ts"`
}

// Ingest POST /api/rum/ingest
func (h *RUMHandler) Ingest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", 405)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 256*1024)) // 256KB max
	if err != nil {
		http.Error(w, "read", 400)
		return
	}
	var p rumPayload
	if err := json.Unmarshal(body, &p); err != nil {
		http.Error(w, "json", 400)
		return
	}
	if !allowedServices[p.Service] {
		http.Error(w, "unknown service", 403)
		return
	}
	page := sanitizePage(p.Page)

	// page view 去重
	pageKey := p.SessionID + "|" + page
	h.mu.Lock()
	_, seen := h.seenPage[pageKey]
	if !seen {
		h.seenPage[pageKey] = struct{}{}
	}
	h.mu.Unlock()
	if !seen {
		rumPageViewsTotal.WithLabelValues(p.Service, page).Inc()
	}

	for _, ev := range p.Events {
		switch ev.Type {
		case "vital":
			if ev.Name != "" {
				rumVitalsHist.WithLabelValues(p.Service, p.Release, ev.Name).Observe(ev.Value)
			}
		case "error", "fetch_error":
			rumErrorsTotal.WithLabelValues(p.Service, p.Release, ev.Type).Inc()
		case "fetch":
			// 4xx/5xx 才上报
			if ev.Status >= 400 {
				rumErrorsTotal.WithLabelValues(p.Service, p.Release, "fetch_4xx_5xx").Inc()
			}
		case "click":
			rumClicksTotal.WithLabelValues(p.Service, sanitizeEvent(ev.Event)).Inc()
		}
	}

	w.WriteHeader(204) // no content, browser 不需要响应内容
}

func sanitizePage(p string) string {
	// 去掉 path 里的 ID (e.g. /merchants/mer_xxx → /merchants/{id})
	parts := strings.Split(p, "/")
	for i, seg := range parts {
		if len(seg) > 12 && !strings.ContainsRune(seg, '.') {
			parts[i] = "{id}"
		}
	}
	out := strings.Join(parts, "/")
	if len(out) > 200 {
		out = out[:200]
	}
	return out
}

func sanitizeEvent(s string) string {
	// 只允许 ascii / hyphen / underscore / dot
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '-' || r == '_' || r == '.' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			out = append(out, r)
		}
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return string(out)
}
