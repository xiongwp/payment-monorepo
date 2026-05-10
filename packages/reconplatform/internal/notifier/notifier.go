// Package notifier 把对账脚本输出的 diff 分发到外部系统：
//
//   webhook   任意 HTTP POST（默认 sink）
//   dingtalk  钉钉群机器人（markdown 富文本）
//   slack     Slack incoming webhook
//   log       仅 log（兜底，永远不挂）
//
// 设计原则：
//
//   - **每脚本可单独配 sinks**：脚本 metadata 加 notify_sinks: ["webhook:oncall", "dingtalk:risk"]
//   - **失败不阻塞主流程**：sink HTTP 调用失败仅 log + 写 DLQ（dlq.go），脚本运行成功仍标 success
//   - **rate limit + dedup**：window 内同 (script_id, diff_type, key) 仅发 1 次（dlq 留意 — see dedup.go）
//   - **配置走 config-center**（key = reconplatform/notifier）：endpoint / token 改完 OnChange 秒生效
//
// 用法（Loader.Run 后）：
//
//	if len(result.Diffs) > 0 {
//	    notifier.Dispatch(ctx, scriptID, result)
//	}

package notifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Diff 一条对账差异（与 script.Diff 同结构；这里复制避免 reverse import）。
type Diff struct {
	Type   string `json:"type"`
	Key    string `json:"key"`
	Want   any    `json:"want,omitempty"`
	Got    any    `json:"got,omitempty"`
	Detail any    `json:"detail,omitempty"`
}

// RunResult 一次脚本运行的可分发摘要。
type RunResult struct {
	ScriptID    string    `json:"script_id"`
	RunID       string    `json:"run_id"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at"`
	Status      string    `json:"status"`
	Error       string    `json:"error,omitempty"`
	TriggeredBy string    `json:"triggered_by"`
	Diffs       []Diff    `json:"diffs"`
}

// Sink 一个分发后端的接口。
type Sink interface {
	// Name 用作 config 里的 sink id（scripts 引用：notify_sinks: ["dingtalk:risk-oncall"]）
	Name() string
	// Send 阻塞投递；失败返 err。Dispatcher 决定是否进 DLQ。
	Send(ctx context.Context, r *RunResult) error
}

// Dispatcher 持有 N 个 Sink 实例 + 路由表（script_id → sink_names）。
type Dispatcher struct {
	mu       sync.RWMutex
	sinks    map[string]Sink           // sink_name → impl
	routing  map[string][]string       // script_id → 该脚本要走的 sinks
	defaults []string                  // 全局兜底 sinks（脚本没指定走这些）
	log      *zap.Logger
	dlq      DLQ                        // 投递失败兜底（可 nil）
}

// DLQ 投递失败时落盘 / Stream，admin 能 replay。本接口实现见 dlq.go。
type DLQ interface {
	Push(ctx context.Context, sinkName string, r *RunResult, err error) error
}

// New 构造空 dispatcher；caller 注 sinks + routing。
func New(log *zap.Logger) *Dispatcher {
	if log == nil {
		log = zap.NewNop()
	}
	d := &Dispatcher{
		sinks:   make(map[string]Sink),
		routing: make(map[string][]string),
		log:     log,
	}
	// 默认装一个 log sink，保证不丢
	d.RegisterSink(&LogSink{log: log})
	d.SetDefault([]string{"log"})
	return d
}

// RegisterSink 注册一个 sink 实现，name 必须唯一。
func (d *Dispatcher) RegisterSink(s Sink) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sinks[s.Name()] = s
}

// SetRouting 给某脚本指定走哪些 sinks（覆盖默认）。
func (d *Dispatcher) SetRouting(scriptID string, sinks []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.routing[scriptID] = sinks
}

// SetDefault 设置全局兜底 sinks（脚本没单独配时走这些）。
func (d *Dispatcher) SetDefault(sinks []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.defaults = sinks
}

// SetDLQ 注 DLQ 实例。
func (d *Dispatcher) SetDLQ(q DLQ) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dlq = q
}

// Dispatch 把 RunResult 发到 routing 表里指定的 sinks。
//
// 行为：
//   - 0 个 diff 不发（diff_type=info / Status=success 的空结果）
//   - 错误：仅 log + push DLQ；返回 nil（不阻塞 caller 的 SaveResult）
//   - sink 不存在（脚本配错 sink 名）：log warn 跳过
//
// 不并行投递：sinks 数量典型 ≤ 3，串行简单稳定，避免 goroutine 泄漏。
func (d *Dispatcher) Dispatch(ctx context.Context, r *RunResult) {
	if r == nil || len(r.Diffs) == 0 {
		return
	}
	d.mu.RLock()
	names := d.routing[r.ScriptID]
	if len(names) == 0 {
		names = d.defaults
	}
	sinks := make([]Sink, 0, len(names))
	for _, n := range names {
		if s, ok := d.sinks[n]; ok {
			sinks = append(sinks, s)
		} else {
			d.log.Warn("notifier: unknown sink referenced",
				zap.String("script_id", r.ScriptID),
				zap.String("sink_name", n))
		}
	}
	dlq := d.dlq
	d.mu.RUnlock()

	for _, s := range sinks {
		// 单 sink 给 5s 上限，防 webhook 慢卡死整链
		sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := s.Send(sctx, r)
		cancel()
		if err != nil {
			d.log.Warn("notifier: sink send failed",
				zap.String("sink", s.Name()),
				zap.String("script_id", r.ScriptID),
				zap.Error(err))
			if dlq != nil {
				_ = dlq.Push(ctx, s.Name(), r, err)
			}
		} else {
			d.log.Debug("notifier: sink delivered",
				zap.String("sink", s.Name()),
				zap.String("script_id", r.ScriptID),
				zap.Int("diffs", len(r.Diffs)))
		}
	}
}

// ─── 内置 Sinks ────────────────────────────────────────────────────────

// LogSink 永远不失败，仅 log。所有 dispatcher 默认装一个，保证 diff 至少落进日志。
type LogSink struct{ log *zap.Logger }

func (LogSink) Name() string { return "log" }

func (l *LogSink) Send(_ context.Context, r *RunResult) error {
	l.log.Info("recon diff",
		zap.String("script_id", r.ScriptID),
		zap.String("run_id", r.RunID),
		zap.String("status", r.Status),
		zap.Int("diffs", len(r.Diffs)),
		zap.Any("diffs_sample", firstNDiffs(r.Diffs, 3)))
	return nil
}

func firstNDiffs(diffs []Diff, n int) []Diff {
	if len(diffs) <= n {
		return diffs
	}
	return diffs[:n]
}

// WebhookSink 通用 HTTP POST。body 是 RunResult JSON。
type WebhookSink struct {
	id      string            // 配置里的 name（webhook:oncall）
	URL     string            // POST 目标
	Headers map[string]string // 例如 X-Auth-Token: xxx
	Client  *http.Client
}

// NewWebhookSink id 形如 "webhook:oncall"，URL 必填。
func NewWebhookSink(id, url string, headers map[string]string) *WebhookSink {
	return &WebhookSink{
		id:      id,
		URL:     url,
		Headers: headers,
		Client:  &http.Client{Timeout: 5 * time.Second},
	}
}

func (w *WebhookSink) Name() string { return w.id }

func (w *WebhookSink) Send(ctx context.Context, r *RunResult) error {
	body, _ := json.Marshal(r)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range w.Headers {
		req.Header.Set(k, v)
	}
	resp, err := w.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("webhook %s: HTTP %d: %s", w.URL, resp.StatusCode, string(respBody))
}

// DingTalkSink 钉钉群机器人。配置 access_token + 可选 sign secret。
//
// 钉钉 API: https://oapi.dingtalk.com/robot/send?access_token=<token>
// payload  {msgtype:"markdown", markdown:{title, text}}
type DingTalkSink struct {
	id          string
	AccessToken string
	Client      *http.Client
}

func NewDingTalkSink(id, accessToken string) *DingTalkSink {
	return &DingTalkSink{
		id:          id,
		AccessToken: accessToken,
		Client:      &http.Client{Timeout: 5 * time.Second},
	}
}

func (d *DingTalkSink) Name() string { return d.id }

func (d *DingTalkSink) Send(ctx context.Context, r *RunResult) error {
	if d.AccessToken == "" {
		return fmt.Errorf("dingtalk: access_token empty")
	}
	url := "https://oapi.dingtalk.com/robot/send?access_token=" + d.AccessToken

	// Markdown 排版：标题 + 关键 diff 类型聚合
	title := fmt.Sprintf("对账脚本 %s 报警 (%d 条 diff)", r.ScriptID, len(r.Diffs))
	var sb strings.Builder
	sb.WriteString("### " + title + "\n\n")
	sb.WriteString(fmt.Sprintf("- run_id: `%s`\n", r.RunID))
	sb.WriteString(fmt.Sprintf("- triggered_by: `%s`\n", r.TriggeredBy))
	sb.WriteString(fmt.Sprintf("- status: `%s`\n\n", r.Status))
	sb.WriteString("**Diff 抽样**:\n\n")
	for i, dd := range r.Diffs {
		if i >= 5 {
			sb.WriteString(fmt.Sprintf("- ... 余 %d 条略\n", len(r.Diffs)-5))
			break
		}
		sb.WriteString(fmt.Sprintf("- `%s` / key=`%s`", dd.Type, dd.Key))
		if dd.Want != nil || dd.Got != nil {
			sb.WriteString(fmt.Sprintf(" want=%v got=%v", dd.Want, dd.Got))
		}
		sb.WriteString("\n")
	}

	body, _ := json.Marshal(map[string]any{
		"msgtype": "markdown",
		"markdown": map[string]string{
			"title": title,
			"text":  sb.String(),
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return fmt.Errorf("dingtalk HTTP %d: %s", resp.StatusCode, string(respBody))
}

// SlackSink Slack incoming webhook。payload {text: "...", blocks: [...]}
type SlackSink struct {
	id     string
	URL    string
	Client *http.Client
}

func NewSlackSink(id, url string) *SlackSink {
	return &SlackSink{id: id, URL: url, Client: &http.Client{Timeout: 5 * time.Second}}
}

func (s *SlackSink) Name() string { return s.id }

func (s *SlackSink) Send(ctx context.Context, r *RunResult) error {
	if s.URL == "" {
		return fmt.Errorf("slack: webhook url empty")
	}
	text := fmt.Sprintf(":warning: *Recon %s* — %d diffs (run %s)",
		r.ScriptID, len(r.Diffs), r.RunID)
	body, _ := json.Marshal(map[string]any{"text": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return fmt.Errorf("slack HTTP %d: %s", resp.StatusCode, string(respBody))
}
