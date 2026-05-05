package replay

import (
	"context"
	"encoding/json"
	"math/rand"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/xiongwp/payment-util/shadow"
)

// CaptureConfig capture interceptor 配置。
type CaptureConfig struct {
	// Producer Kafka producer（生产者方注入）
	Producer Producer
	// RawTopic 原始（含 PII）topic，sanitizer 后续从这读
	RawTopic string
	// SampleRate 采样率，0..1
	SampleRate float64
	// MethodBlacklist 不录的 method 前缀（如 admin / debug RPC）
	MethodBlacklist []string
	// HeaderWhitelist 录哪些 metadata header（其它的全部丢弃避免敏感信息进 Kafka）
	HeaderWhitelist []string
}

// defaultHeaderWhitelist 默认录这几个：trace_id 必须，shadow 不录（不能有），
// 其它业务相关字段按需放进 whitelist。
var defaultHeaderWhitelist = []string{
	"x-trace-id",
	"x-request-id",
	"x-account-system",
	"x-merchant-id",
}

// defaultMethodBlacklist 默认不录的 RPC 前缀：
//
//   - grpc.health.* / grpc.reflection.*：探针
//   - admin RPC：包含敏感数据，且回放可能有副作用
var defaultMethodBlacklist = []string{
	"/grpc.health.",
	"/grpc.reflection.",
	"/admin.",
}

// stdRand 用 sync.Mutex 包 *rand.Rand，避免并发触发 race。
type stdRand struct {
	mu sync.Mutex
	r  *rand.Rand
}

func (s *stdRand) Float64() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.r.Float64()
}

// UnaryCaptureInterceptor 创建 gRPC server unary interceptor，把每条请求按
// 采样率写入 Kafka raw topic。
//
// 装在 grpc.ChainUnaryInterceptor 链里，建议位置：
//
//	trace → shadow → capture → auth → handler
//
// 这样能拿到 trace_id（写 Kafka 用作 key 方便回放追溯），又能跳过 shadow 流量
// （capture 不录 shadow=1 的请求，否则回放循环放大）。
func UnaryCaptureInterceptor(cfg CaptureConfig) grpc.UnaryServerInterceptor {
	if cfg.HeaderWhitelist == nil {
		cfg.HeaderWhitelist = defaultHeaderWhitelist
	}
	if cfg.MethodBlacklist == nil {
		cfg.MethodBlacklist = defaultMethodBlacklist
	}
	if cfg.RawTopic == "" {
		cfg.RawTopic = "traffic-replay.raw"
	}
	rng := &stdRand{r: rand.New(rand.NewSource(time.Now().UnixNano()))}
	host, _ := os.Hostname()

	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		// 录决策：shadow 不录 / 黑名单 / 采样
		if !IsCapturable(shadow.IsShadow(ctx), info.FullMethod, cfg.MethodBlacklist, cfg.SampleRate, rng) {
			return handler(ctx, req)
		}
		// 序列化 body 用 protojson（gRPC 请求一定是 protobuf）。
		// non-proto 类型走最佳努力 json.Marshal 兜底。
		var body []byte
		var err error
		if pm, ok := req.(proto.Message); ok {
			body, err = protojson.Marshal(pm)
		} else {
			body, err = json.Marshal(req)
		}
		if err == nil && cfg.Producer != nil {
			ev := Event{
				CapturedAt:  time.Now().UTC(),
				Method:      info.FullMethod,
				Headers:     extractHeaders(ctx, cfg.HeaderWhitelist),
				BodyJSON:    body,
				CaptureNode: host,
				SampleRate:  cfg.SampleRate,
			}
			if raw, err := json.Marshal(&ev); err == nil {
				key := []byte(ev.Method)
				_ = cfg.Producer.Send(cfg.RawTopic, key, raw)
				// 错误吞掉：capture 永远不能阻断业务请求。Kafka 故障由 Kafka 监控负责。
			}
		}
		// 业务调用照常进行
		return handler(ctx, req)
	}
}

// extractHeaders 按白名单从 incoming metadata 取 header。
// 不在白名单的（authorization / x-shadow / cookie 等敏感字段）一律不录。
func extractHeaders(ctx context.Context, whitelist []string) map[string]string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(whitelist))
	for _, k := range whitelist {
		if vals := md.Get(k); len(vals) > 0 {
			out[k] = vals[0]
		}
	}
	return out
}
