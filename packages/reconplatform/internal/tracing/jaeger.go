// Package tracing — diff ↔ trace 关联 + Jaeger 跳转 URL 构造。
//
// 背景：reconplatform 监听支付链路的 binlog，业务系统（payment-core / order-core
// / accounting-system）每次写库都会带 trace_id（OTel/SkyWalking 注入到 row）。
// CDC 入流时 TraceIDEnricher 会把 trace_id / x_trace_id / request_id 提到 Indexes，
// 后续脚本生成的 diff 可以把 trace_id 带进 Detail。
//
// 这个包做两件事：
//
//  1. ExtractTraceID(detail map) — 从一条 diff 的 detail 里挖 trace_id。
//     脚本作者可写 `detail = {"trace_id": ev["trace_id"]}` 显式带；
//     也可不写，由 CDC 阶段自动从 ev.Indexes 同步。
//
//  2. JaegerURL(traceID) — 拼出能直接跳的 Jaeger UI URL。配置走 env
//     JAEGER_UI_URL（默认 http://jaeger:16686）。
//
// admin web 用法：
//
//     GET /api/v1/diffs/:id/trace
//     →  { "trace_id": "abc123", "jaeger_url": "http://.../trace/abc123" }
//
// 然后前端在 diff 详情面板显示 "🔍 Open in Jaeger" 按钮，点击新开 tab。
//
// 同时 X-Trace-ID 注入到所有 admin web 出站请求 header（middleware.go）— 这样
// reconplatform 自己写脚本时产生的 diff 也能跟 admin 操作 trace 关联。

package tracing

import (
	"net/url"
	"os"
	"strings"
)

// Config Jaeger 接入参数。
type Config struct {
	UIBaseURL  string // http://jaeger:16686 (无 trailing /)
	ServiceTag string // 默认空 — 可填 reconplatform 让 Jaeger 默认查询时高亮
}

// FromEnv 从环境变量读配置。env 缺则用安全默认。
func FromEnv() Config {
	return Config{
		UIBaseURL:  envOr("JAEGER_UI_URL", "http://jaeger:16686"),
		ServiceTag: os.Getenv("JAEGER_SERVICE_TAG"),
	}
}

// JaegerURL 给定 trace_id 拼跳转 URL。返空字符串表示无效输入（不跳转）。
//
// Jaeger UI 标准 URL 格式:
//
//	http://<host>:16686/trace/<trace_id>
//
// 也可以 deeplink:
//
//	http://<host>:16686/search?service=<svc>&traceID=<id>
//
// 我们用直链 /trace/<id> 简洁。
func (c Config) JaegerURL(traceID string) string {
	traceID = sanitizeTraceID(traceID)
	if traceID == "" {
		return ""
	}
	base := strings.TrimRight(c.UIBaseURL, "/")
	if base == "" {
		return ""
	}
	return base + "/trace/" + url.PathEscape(traceID)
}

// SearchURL 给定 service + 时间窗，构造 Jaeger search URL（无 trace_id 时备用，
// 配合 admin web "查看脚本所有出错 trace" 等场景）。
func (c Config) SearchURL(service string, lookbackMin int) string {
	if lookbackMin <= 0 {
		lookbackMin = 60
	}
	base := strings.TrimRight(c.UIBaseURL, "/")
	q := url.Values{}
	if service != "" {
		q.Set("service", service)
	} else if c.ServiceTag != "" {
		q.Set("service", c.ServiceTag)
	}
	q.Set("lookback", "1h")
	if lookbackMin != 60 {
		q.Set("lookback", "custom")
	}
	return base + "/search?" + q.Encode()
}

// ExtractTraceID 从 diff.Detail 里挖 trace_id。
//
// 优先级（脚本作者把 trace_id 放哪儿都能挖到）：
//
//	detail.trace_id (string)
//	detail.x_trace_id (string)
//	detail.request_id (string)
//	detail.indexes.trace_id (从 EventList wrapper 透传过来)
//	detail.event.indexes.trace_id (深一层)
//
// 找不到返空 string —— 不会 panic / 不返 error。
func ExtractTraceID(detail map[string]any) string {
	if detail == nil {
		return ""
	}
	for _, k := range []string{"trace_id", "x_trace_id", "request_id"} {
		if v, ok := detail[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return sanitizeTraceID(s)
			}
		}
	}
	// 嵌套 indexes
	if idx, ok := detail["indexes"].(map[string]any); ok {
		for _, k := range []string{"trace_id", "x_trace_id", "request_id"} {
			if v, ok := idx[k]; ok {
				if s, ok := v.(string); ok && s != "" {
					return sanitizeTraceID(s)
				}
			}
		}
	}
	// event.indexes
	if ev, ok := detail["event"].(map[string]any); ok {
		if idx, ok := ev["indexes"].(map[string]any); ok {
			for _, k := range []string{"trace_id", "x_trace_id", "request_id"} {
				if v, ok := idx[k]; ok {
					if s, ok := v.(string); ok && s != "" {
						return sanitizeTraceID(s)
					}
				}
			}
		}
	}
	return ""
}

// sanitizeTraceID 防 ICR (URL injection) — 仅允许 hex / dash / underscore。
func sanitizeTraceID(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 128 {
		return ""
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') ||
			(c >= 'A' && c <= 'F') || c == '-' || c == '_') {
			return ""
		}
	}
	return s
}

func envOr(k, dflt string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return dflt
}
