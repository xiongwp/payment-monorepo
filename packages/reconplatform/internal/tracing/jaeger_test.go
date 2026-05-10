// jaeger_test.go — sanitizeTraceID 是 trust-boundary 函数（admin web 输入直
// 接进 URL），必须用穷举的 hostile inputs 覆盖。

package tracing

import (
	"strings"
	"testing"
)

func TestSanitizeTraceID_Valid(t *testing.T) {
	cases := []string{
		"abc123",
		"00000000-0000-0000-0000-000000000000",   // OTel 标准 32 hex w/ dash
		"4bf92f3577b34da6a3ce929d0e0e4736",       // 16-byte trace
		"trace_id_123",                            // underscore
		"AbCdEf012345",                            // mixed case hex
		strings.Repeat("a", 128),                  // 128 字符 (boundary)
	}
	for _, in := range cases {
		got := sanitizeTraceID(in)
		if got != in {
			t.Errorf("sanitizeTraceID(%q) = %q; want %q", in, got, in)
		}
	}
}

func TestSanitizeTraceID_Hostile(t *testing.T) {
	hostile := []struct {
		name string
		in   string
	}{
		{"path traversal", "../../etc/passwd"},
		{"slash", "abc/def"},
		{"backslash", "abc\\def"},
		{"query injection", "abc?evil=1"},
		{"hash injection", "abc#evil"},
		{"angle XSS", "<script>alert(1)</script>"},
		{"newline CRLF", "abc\r\nLocation: evil.com"},
		{"null byte", "abc\x00"},
		{"semicolon", "abc;rm -rf"},
		{"space", "abc def"},
		{"chinese", "中文"},
		{"emoji", "🔥"},
		{"too long (129)", strings.Repeat("a", 129)},
		{"way too long (10MB)", strings.Repeat("x", 10*1024*1024)},
		{"url-like", "http://evil.com/abc"},
		{"protocol-relative", "//evil.com"},
		{"javascript scheme", "javascript:void(0)"},
		{"data uri", "data:text/html,<script>"},
		{"single quote SQL", "abc' OR '1'='1"},
		{"double quote", "abc\"def"},
		{"backtick", "abc`whoami`"},
		{"percent encoded", "%2e%2e%2f"},
	}
	for _, c := range hostile {
		t.Run(c.name, func(t *testing.T) {
			got := sanitizeTraceID(c.in)
			if got != "" {
				t.Errorf("sanitizeTraceID(%q) = %q; want empty (hostile input)",
					c.in, got)
			}
		})
	}
}

func TestSanitizeTraceID_TrimsWhitespace(t *testing.T) {
	got := sanitizeTraceID("  abc123  ")
	if got != "abc123" {
		t.Errorf("expected trimmed 'abc123', got %q", got)
	}
}

func TestJaegerURL(t *testing.T) {
	cfg := Config{UIBaseURL: "http://jaeger:16686"}
	cases := []struct {
		in, want string
	}{
		{"abc123", "http://jaeger:16686/trace/abc123"},
		{"4bf92f3577b34da6a3ce929d0e0e4736", "http://jaeger:16686/trace/4bf92f3577b34da6a3ce929d0e0e4736"},
		{"", ""},
		{"../../etc/passwd", ""}, // hostile → 拒
		{"abc/def", ""},
	}
	for _, c := range cases {
		got := cfg.JaegerURL(c.in)
		if got != c.want {
			t.Errorf("JaegerURL(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

func TestJaegerURL_TrailingSlash(t *testing.T) {
	cfg := Config{UIBaseURL: "http://jaeger:16686/"} // trailing /
	got := cfg.JaegerURL("abc")
	want := "http://jaeger:16686/trace/abc"
	if got != want {
		t.Errorf("trailing-slash baseURL: got %q want %q", got, want)
	}
}

func TestJaegerURL_EmptyBase(t *testing.T) {
	cfg := Config{UIBaseURL: ""}
	if got := cfg.JaegerURL("abc123"); got != "" {
		t.Errorf("empty base should return empty, got %q", got)
	}
}

func TestExtractTraceID_TopLevel(t *testing.T) {
	d := map[string]any{"trace_id": "abc123", "other": 1}
	if got := ExtractTraceID(d); got != "abc123" {
		t.Errorf("top-level trace_id: got %q", got)
	}
}

func TestExtractTraceID_Aliases(t *testing.T) {
	for _, key := range []string{"trace_id", "x_trace_id", "request_id"} {
		d := map[string]any{key: "xyz789"}
		if got := ExtractTraceID(d); got != "xyz789" {
			t.Errorf("alias %s: got %q", key, got)
		}
	}
}

func TestExtractTraceID_NestedIndexes(t *testing.T) {
	d := map[string]any{
		"indexes": map[string]any{"trace_id": "nested-id"},
	}
	if got := ExtractTraceID(d); got != "nested-id" {
		t.Errorf("nested indexes: got %q", got)
	}
}

func TestExtractTraceID_DoubleNested(t *testing.T) {
	d := map[string]any{
		"event": map[string]any{
			"indexes": map[string]any{"trace_id": "deep"},
		},
	}
	if got := ExtractTraceID(d); got != "deep" {
		t.Errorf("double-nested: got %q", got)
	}
}

func TestExtractTraceID_PriorityOrder(t *testing.T) {
	// 顶层优先于 nested
	d := map[string]any{
		"trace_id": "top",
		"indexes":  map[string]any{"trace_id": "nested"},
	}
	if got := ExtractTraceID(d); got != "top" {
		t.Errorf("top should win over nested: got %q", got)
	}
}

func TestExtractTraceID_NilSafe(t *testing.T) {
	if got := ExtractTraceID(nil); got != "" {
		t.Errorf("nil map: got %q", got)
	}
	if got := ExtractTraceID(map[string]any{}); got != "" {
		t.Errorf("empty map: got %q", got)
	}
	// non-string value
	d := map[string]any{"trace_id": 12345}
	if got := ExtractTraceID(d); got != "" {
		t.Errorf("non-string trace_id should not match: got %q", got)
	}
	// empty string value
	d2 := map[string]any{"trace_id": ""}
	if got := ExtractTraceID(d2); got != "" {
		t.Errorf("empty string trace_id: got %q", got)
	}
}

func TestExtractTraceID_HostileSanitized(t *testing.T) {
	// 即便 detail 里塞恶意 trace_id 也要被 sanitize 成空
	d := map[string]any{"trace_id": "../../etc/passwd"}
	if got := ExtractTraceID(d); got != "" {
		t.Errorf("hostile trace_id should be sanitized: got %q", got)
	}
}

func TestSearchURL(t *testing.T) {
	cfg := Config{UIBaseURL: "http://jaeger:16686", ServiceTag: "reconplatform"}
	url := cfg.SearchURL("payment-core", 60)
	if !strings.Contains(url, "service=payment-core") {
		t.Errorf("expected service= in URL, got %q", url)
	}
	if !strings.HasPrefix(url, "http://jaeger:16686/search?") {
		t.Errorf("expected base/search prefix, got %q", url)
	}
}

func TestSearchURL_FallbackServiceTag(t *testing.T) {
	cfg := Config{UIBaseURL: "http://jaeger:16686", ServiceTag: "reconplatform"}
	url := cfg.SearchURL("", 60) // 空 service → fallback to ServiceTag
	if !strings.Contains(url, "service=reconplatform") {
		t.Errorf("expected fallback service tag, got %q", url)
	}
}

func TestFromEnv_Defaults(t *testing.T) {
	t.Setenv("JAEGER_UI_URL", "")
	t.Setenv("JAEGER_SERVICE_TAG", "")
	c := FromEnv()
	if c.UIBaseURL != "http://jaeger:16686" {
		t.Errorf("default UIBaseURL: got %q", c.UIBaseURL)
	}
	if c.ServiceTag != "" {
		t.Errorf("default ServiceTag: got %q", c.ServiceTag)
	}
}

func TestFromEnv_Override(t *testing.T) {
	t.Setenv("JAEGER_UI_URL", "https://jaeger.prod.example.com")
	t.Setenv("JAEGER_SERVICE_TAG", "prod")
	c := FromEnv()
	if c.UIBaseURL != "https://jaeger.prod.example.com" {
		t.Errorf("override UIBaseURL: got %q", c.UIBaseURL)
	}
	if c.ServiceTag != "prod" {
		t.Errorf("override ServiceTag: got %q", c.ServiceTag)
	}
}
