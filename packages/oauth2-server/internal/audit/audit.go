// Package audit — admin actions 审计日志。
//
// 每次 admin 动作 (create client / rotate / suspend / key-rotate) 写一条:
//   - actor:    谁 (ops email 或 service)
//   - action:   做什么
//   - target:   操作对象
//   - source_ip
//   - details:  请求参数 (脱敏)
//   - 时间戳
//
// Sink 抽象 — 当前 zap log + DB 表 admin_audit, 未来可接 SIEM。

package audit

import (
	"context"
	"encoding/json"

	"go.uber.org/zap"
)

// Event 一条审计事件。
type Event struct {
	Actor     string         `json:"actor"`
	Action    string         `json:"action"`     // create-client / rotate-secret / suspend / activate / revoke / key-rotate
	ClientID  string         `json:"client_id,omitempty"`
	SourceIP  string         `json:"source_ip,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
}

// Sink 审计输出抽象。
type Sink interface {
	Write(ctx context.Context, e Event) error
}

// LogSink 简单 zap 落日志 (默认)。
type LogSink struct{ Log *zap.Logger }

// Write impl。
func (s *LogSink) Write(_ context.Context, e Event) error {
	body, _ := json.Marshal(e)
	s.Log.Info("AUDIT", zap.ByteString("event", body))
	return nil
}

// Multi 同时写多个 sink (LogSink + MySQL + audit-log service)。
type Multi []Sink

// Write 串行写，best-effort，单个 sink 失败不阻塞其它。
func (m Multi) Write(ctx context.Context, e Event) error {
	var lastErr error
	for _, s := range m {
		if err := s.Write(ctx, e); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

// ─── 脱敏 helper ─────────────────────────────────────────────────────

// Redact 去掉敏感字段 (secret, key 内容)。
func Redact(details map[string]any) map[string]any {
	if details == nil {
		return nil
	}
	out := map[string]any{}
	for k, v := range details {
		switch k {
		case "client_secret", "secret", "password", "private_key", "secret_hash":
			out[k] = "<redacted>"
		default:
			out[k] = v
		}
	}
	return out
}
