package replay

import (
	"encoding/json"
	"fmt"
	"time"
)

// Event Kafka 上单条录制的 gRPC 请求。raw topic 上是真实 PII；
// sanitizer 处理后写入 shadow topic 时 BodyJSON 已脱敏。
type Event struct {
	// CapturedAt 服务端 capture interceptor 进入的时间（UTC）。
	CapturedAt time.Time `json:"captured_at"`
	// Method gRPC FullMethod，如 /order.v1.PaymentIntent/Create
	Method string `json:"method"`
	// Headers gRPC metadata（白名单：x-trace-id / x-account-system /
	// x-request-id 等；x-shadow / authorization 严禁录，见 capture.go）。
	Headers map[string]string `json:"headers,omitempty"`
	// BodyJSON protojson 序列化的请求体。回放时 protojson.Unmarshal 即可还原。
	BodyJSON json.RawMessage `json:"body"`
	// CaptureNode 录制 pod 的 hostname；多 pod 部署时方便定位流量来源。
	CaptureNode string `json:"capture_node,omitempty"`
	// SampleRate capture 时的采样率（用于回放后还原原始 QPS）。
	SampleRate float64 `json:"sample_rate"`
}

// ErrSkipCapture interceptor 内部信号：本次请求不该录（已 shadow / 命中黑名单）。
var ErrSkipCapture = fmt.Errorf("replay: skip capture")

// Producer 抽象 Kafka producer（避免 payment-util 强依赖某 client lib）。
// 任何实现 Send(ctx, topic, key, value) 的对象都行。
type Producer interface {
	Send(topic string, key, value []byte) error
}

// IsCapturable 是否应该录制本次请求。
//
// 规则：
//  1. 已经 shadow（避免回放被二次 capture，造成放大循环）→ 不录
//  2. method 命中黑名单（如 admin RPC / health 探针）→ 不录
//  3. 采样率随机决定 → 命中才录
//
// blacklist 是 method 前缀列表，包含即跳过。
func IsCapturable(isShadow bool, fullMethod string, blacklist []string, sampleRate float64, randSource RandSource) bool {
	if isShadow {
		return false
	}
	for _, p := range blacklist {
		if len(p) > 0 && (fullMethod == p || hasPrefix(fullMethod, p)) {
			return false
		}
	}
	if sampleRate <= 0 {
		return false
	}
	if sampleRate >= 1 {
		return true
	}
	return randSource.Float64() < sampleRate
}

// RandSource 抽象 rand.Float64() 让单测可注入确定值。
type RandSource interface {
	Float64() float64
}

func hasPrefix(s, p string) bool {
	if len(p) > len(s) {
		return false
	}
	return s[:len(p)] == p
}
