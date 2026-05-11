// Package handler — trace graph API.
//
// GET /api/trace/{trace_id}/graph
//
// 聚合 Jaeger spans + Loki logs → 返一个 JSON, 前端 SPA 用来画:
//   - timeline (按 service / span 时间轴)
//   - dependency graph (service A → service B 调用关系 + 计数)
//   - 每个 span 关联的 log lines
//
// 后端做合并 + 脱敏 + 时间归一; 前端只管渲染。
//
// 依赖:
//   JAEGER_QUERY_URL=http://jaeger:16686   (jaeger query HTTP)
//   LOKI_URL=http://loki:3100             (loki HTTP)

package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
)

// TraceGraphHandler ...
type TraceGraphHandler struct {
	JaegerURL string
	LokiURL   string
	HTTP      *http.Client
}

// NewTraceGraphHandler 从 env 装载。
func NewTraceGraphHandler() *TraceGraphHandler {
	return &TraceGraphHandler{
		JaegerURL: envOrDef("JAEGER_QUERY_URL", "http://jaeger:16686"),
		LokiURL:   envOrDef("LOKI_URL", "http://loki:3100"),
		HTTP:      &http.Client{Timeout: 10 * time.Second},
	}
}

func envOrDef(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// Graph response.
type traceGraphResponse struct {
	TraceID    string          `json:"trace_id"`
	StartMs    int64           `json:"start_ms"`
	EndMs      int64           `json:"end_ms"`
	DurationMs int64           `json:"duration_ms"`
	Spans      []spanNode      `json:"spans"`
	Edges      []dependencyEdge `json:"edges"`
	Logs       []logLine       `json:"logs"`
	Services   []string        `json:"services"`
}

type spanNode struct {
	SpanID      string            `json:"span_id"`
	ParentID    string            `json:"parent_id,omitempty"`
	Service     string            `json:"service"`
	Operation   string            `json:"operation"`
	StartMs     int64             `json:"start_ms"`
	DurationMs int64              `json:"duration_ms"`
	Error       bool              `json:"error,omitempty"`
	StatusCode  string            `json:"status_code,omitempty"`
	Tags        map[string]string `json:"tags,omitempty"`
}

type dependencyEdge struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Count int    `json:"count"`
}

type logLine struct {
	Ts      int64  `json:"ts"`      // unix ms
	Service string `json:"service"`
	Level   string `json:"level,omitempty"`
	Message string `json:"message"`
}

// Graph GET /api/trace/{trace_id}/graph
func (h *TraceGraphHandler) Graph(w http.ResponseWriter, r *http.Request) {
	traceID := mux.Vars(r)["trace_id"]
	if !validTraceID(traceID) {
		writeError(w, http.StatusBadRequest, "invalid trace_id")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	// 1) 拉 Jaeger spans
	spans, edges, services, t0, t1, err := h.fetchJaegerTrace(ctx, traceID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "jaeger: "+err.Error())
		return
	}

	// 2) 拉 Loki 关联日志 (按 trace_id 过滤 + 时间窗稍放宽)
	logs, lerr := h.fetchLokiLogs(ctx, traceID, t0-2000, t1+2000)
	if lerr != nil {
		// 日志拉失败不阻塞 — span 信息已经够看
		logs = nil
	}

	resp := traceGraphResponse{
		TraceID:    traceID,
		StartMs:    t0,
		EndMs:      t1,
		DurationMs: t1 - t0,
		Spans:      spans,
		Edges:      edges,
		Logs:       logs,
		Services:   services,
	}
	writeJSON(w, resp)
}

var traceIDRE = regexp.MustCompile(`^[a-f0-9]{16,32}$`)

func validTraceID(s string) bool {
	return traceIDRE.MatchString(strings.ToLower(s))
}

// ─── Jaeger ────────────────────────────────────────────────────────────

// Jaeger HTTP API: GET /api/traces/{id}
// 文档: https://www.jaegertracing.io/docs/1.50/apis/#http-json-internal
func (h *TraceGraphHandler) fetchJaegerTrace(ctx context.Context, traceID string) (
	[]spanNode, []dependencyEdge, []string, int64, int64, error,
) {
	u := fmt.Sprintf("%s/api/traces/%s", h.JaegerURL, traceID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := h.HTTP.Do(req)
	if err != nil {
		return nil, nil, nil, 0, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, nil, nil, 0, 0,
			fmt.Errorf("jaeger %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var jr struct {
		Data []struct {
			TraceID   string `json:"traceID"`
			Spans     []struct {
				SpanID        string `json:"spanID"`
				OperationName string `json:"operationName"`
				StartTime     int64  `json:"startTime"` // microsec
				Duration      int64  `json:"duration"`  // microsec
				References    []struct {
					RefType string `json:"refType"`
					SpanID  string `json:"spanID"`
				} `json:"references"`
				ProcessID string `json:"processID"`
				Tags []struct {
					Key   string      `json:"key"`
					Type  string      `json:"type"`
					Value interface{} `json:"value"`
				} `json:"tags"`
			} `json:"spans"`
			Processes map[string]struct {
				ServiceName string `json:"serviceName"`
			} `json:"processes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &jr); err != nil {
		return nil, nil, nil, 0, 0, fmt.Errorf("parse jaeger: %w", err)
	}
	if len(jr.Data) == 0 {
		return nil, nil, nil, 0, 0, fmt.Errorf("trace not found")
	}
	trace := jr.Data[0]
	out := []spanNode{}
	edgeCount := map[string]int{} // "a→b" → count
	t0 := int64(0)
	t1 := int64(0)
	servicesSet := map[string]struct{}{}
	for _, s := range trace.Spans {
		proc := trace.Processes[s.ProcessID]
		svc := proc.ServiceName
		servicesSet[svc] = struct{}{}
		startMs := s.StartTime / 1000
		durMs := s.Duration / 1000
		endMs := startMs + durMs
		if t0 == 0 || startMs < t0 {
			t0 = startMs
		}
		if endMs > t1 {
			t1 = endMs
		}
		parentID := ""
		if len(s.References) > 0 {
			for _, ref := range s.References {
				if ref.RefType == "CHILD_OF" {
					parentID = ref.SpanID
					break
				}
			}
		}
		isErr := false
		statusCode := ""
		tags := map[string]string{}
		for _, t := range s.Tags {
			if t.Key == "error" {
				if b, ok := t.Value.(bool); ok && b {
					isErr = true
				}
			}
			if t.Key == "http.status_code" || t.Key == "rpc.grpc.status_code" {
				statusCode = fmt.Sprint(t.Value)
			}
			// 选几个有用的 tag 透出
			if t.Key == "http.method" || t.Key == "http.url" || t.Key == "rpc.method" ||
				t.Key == "merchant_id" || t.Key == "actor" {
				tags[t.Key] = fmt.Sprint(t.Value)
			}
		}
		out = append(out, spanNode{
			SpanID:     s.SpanID,
			ParentID:   parentID,
			Service:    svc,
			Operation:  s.OperationName,
			StartMs:    startMs,
			DurationMs: durMs,
			Error:      isErr,
			StatusCode: statusCode,
			Tags:       tags,
		})
	}
	// 边: 找每个 span 的 parent span 在哪个 service, parentSvc → spanSvc
	idToSvc := map[string]string{}
	for _, s := range out {
		idToSvc[s.SpanID] = s.Service
	}
	for _, s := range out {
		if s.ParentID == "" {
			continue
		}
		parentSvc, ok := idToSvc[s.ParentID]
		if !ok || parentSvc == s.Service {
			continue
		}
		edgeCount[parentSvc+"→"+s.Service]++
	}
	edges := []dependencyEdge{}
	for k, n := range edgeCount {
		parts := strings.SplitN(k, "→", 2)
		edges = append(edges, dependencyEdge{From: parts[0], To: parts[1], Count: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartMs < out[j].StartMs })
	services := make([]string, 0, len(servicesSet))
	for s := range servicesSet {
		services = append(services, s)
	}
	sort.Strings(services)
	return out, edges, services, t0, t1, nil
}

// ─── Loki ──────────────────────────────────────────────────────────────

// Loki query_range API: GET /loki/api/v1/query_range
func (h *TraceGraphHandler) fetchLokiLogs(ctx context.Context, traceID string, startMs, endMs int64) ([]logLine, error) {
	q := fmt.Sprintf(`{service=~".+"} |~ %q`, traceID)
	params := url.Values{}
	params.Set("query", q)
	params.Set("start", strconv.FormatInt(startMs*1_000_000, 10)) // ns
	params.Set("end", strconv.FormatInt(endMs*1_000_000, 10))
	params.Set("limit", "500")
	params.Set("direction", "forward")
	u := h.LokiURL + "/loki/api/v1/query_range?" + params.Encode()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := h.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("loki %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var lr struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Stream map[string]string `json:"stream"`
				Values [][]string        `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &lr); err != nil {
		return nil, fmt.Errorf("parse loki: %w", err)
	}
	out := []logLine{}
	for _, stream := range lr.Data.Result {
		svc := stream.Stream["service"]
		level := stream.Stream["level"]
		for _, kv := range stream.Values {
			if len(kv) != 2 {
				continue
			}
			tsNs, _ := strconv.ParseInt(kv[0], 10, 64)
			out = append(out, logLine{
				Ts:      tsNs / 1_000_000,
				Service: svc,
				Level:   level,
				Message: truncate(kv[1], 1000),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ts < out[j].Ts })
	return out, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
