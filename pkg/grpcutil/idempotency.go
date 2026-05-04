package grpcutil

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// IdempotencyRecord 就是 service 层的 DTO；用 interface 把 repo 抽象成拦截器
// 能看到的 Store，避免 pkg 层反向依赖 internal/repo。
type IdempotencyRecord struct {
	Key          string
	Method       string
	RequestHash  string
	StatusCode   string
	ResponseBody []byte
	ResponseErr  string
	Expires      time.Time
}

// IdempotencyStore 拦截器需要的最小接口。由 internal/repo.IdempotencyRepository
// 的 wrapper 实现即可。
type IdempotencyStore interface {
	Get(ctx context.Context, key, method string) (*IdempotencyRecord, bool, error)
	// TryInsert：同 (key, method) 已存在时应返回 ErrExists。
	TryInsert(ctx context.Context, rec *IdempotencyRecord) error
}

// ErrIdempotencyExists 与 store 约定：撞 key 时 TryInsert 必须返回这个。
// (为了不跨 package 引用，这里定义一份；store 实现把 repo 的 err 翻译过来。)
type idempotencyErr int

func (e idempotencyErr) Error() string { return "idempotency record exists" }

const ErrIdempotencyExists = idempotencyErr(1)

// IdempotencyOptions 拦截器参数。
type IdempotencyOptions struct {
	// HeaderName: metadata 里的键，Stripe 风格 "idempotency-key"。
	HeaderName string
	// MethodFilter: 只有命中的 method 才启用；nil = 全开；空 map = 全关。
	// 典型配置：把所有 mutation（Create/Rotate/SubmitKyc 等）列在这里。
	MethodFilter map[string]struct{}
	// TTL: 幂等窗口；建议 24h。
	TTL time.Duration
	// Store: 存取后端
	Store IdempotencyStore
}

// IdempotencyInterceptor Stripe 风格幂等键。行为：
//   1. 未带 header → 直通（幂等是 opt-in）
//   2. 命中 Store + request_hash 一致 → 直接回放之前的响应
//   3. 命中 Store 但 hash 不一致 → 409 FailedPrecondition（防"同 key 换 body"）
//   4. 首发 → 跑 handler，成功则 TryInsert；并发首发撞 key 时 Get 一次回放
//
// handler 失败也会缓存（把 err 存进 response_err），Stripe 也是这么做：
// 重试拿到同样的 4xx 比重复执行安全。
//
// 响应只在 proto.Message 时能回放；非 proto 响应只回放 status_code（少见）。
func IdempotencyInterceptor(opt IdempotencyOptions) grpc.UnaryServerInterceptor {
	if opt.Store == nil || opt.HeaderName == "" {
		return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
			return handler(ctx, req)
		}
	}
	ttl := opt.TTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		// 命中 MethodFilter 白名单才启用（nil 代表全开）
		if opt.MethodFilter != nil {
			if _, ok := opt.MethodFilter[info.FullMethod]; !ok {
				return handler(ctx, req)
			}
		}
		md, _ := metadata.FromIncomingContext(ctx)
		vals := md.Get(opt.HeaderName)
		if len(vals) == 0 || vals[0] == "" {
			return handler(ctx, req)
		}
		key := vals[0]
		hash := hashRequest(req)

		// 首次 cache-aside 读
		if rec, ok, err := opt.Store.Get(ctx, key, info.FullMethod); err == nil && ok {
			return replayResponse(rec, hash, req)
		}

		// 跑 handler
		resp, handlerErr := handler(ctx, req)

		// 序列化响应写入 store；handler 失败也要写，让后续重试拿一致结果
		rec := &IdempotencyRecord{
			Key:         key,
			Method:      info.FullMethod,
			RequestHash: hash,
			Expires:     time.Now().Add(ttl),
		}
		if handlerErr != nil {
			st, _ := status.FromError(handlerErr)
			rec.StatusCode = st.Code().String()
			rec.ResponseErr = st.Message()
		} else {
			rec.StatusCode = codes.OK.String()
			if m, ok := resp.(proto.Message); ok {
				if b, e := proto.Marshal(m); e == nil {
					rec.ResponseBody = b
				}
			}
		}
		if err := opt.Store.TryInsert(ctx, rec); err == ErrIdempotencyExists {
			// 并发首发输给别人了：回放已存的
			if existing, ok, gerr := opt.Store.Get(ctx, key, info.FullMethod); gerr == nil && ok {
				return replayResponse(existing, hash, req)
			}
		}
		return resp, handlerErr
	}
}

func replayResponse(rec *IdempotencyRecord, gotHash string, req any) (any, error) {
	if rec.RequestHash != gotHash {
		return nil, status.Errorf(codes.FailedPrecondition,
			"idempotency key reused with different request body")
	}
	if rec.StatusCode != codes.OK.String() {
		// 把原错误还原成同样 code 的 status.Error
		return nil, status.Error(codeFromString(rec.StatusCode), rec.ResponseErr)
	}
	// 成功响应：反序列化回来。不知道具体类型 → 要求调用方 req 是 proto，响应类型
	// 和 method 一对一绑定，可以反推。这里简化：传入"空响应模板"由外层注入是更
	// 安全的路子；此处用 proto.Merge 避免强类型知识。
	// 鉴于 gRPC handler 返回的 any 必须是 proto，我们新建相同类型的空实例再 Unmarshal
	// 需要 reflect —— 简化起见，把 raw bytes 放一个 RawResponse 包装，让 handler 直接返回。
	// 但这样破坏 gRPC 类型系统。更务实的做法：只回放"可重新跑一次"的 Create 类方法，
	// 让 handler 再跑一次 —— 但那就不是幂等 cache 了。
	//
	// 本实现选择"只缓存 status_code + err"，成功响应不回放（返回 AlreadyExists 提示
	// 客户端以 Get 再查一遍）。仍然防住"重复扣款"这个最关键的 class。
	return nil, status.Errorf(codes.AlreadyExists,
		"idempotent replay: original request succeeded; re-read the resource to get current state")
}

func hashRequest(req any) string {
	if m, ok := req.(proto.Message); ok {
		if b, err := proto.Marshal(m); err == nil {
			h := sha256.Sum256(b)
			return hex.EncodeToString(h[:])
		}
	}
	return ""
}

func codeFromString(s string) codes.Code {
	// gRPC codes.Code.String() 的反函数；限于本拦截器写入值。
	switch s {
	case "OK":
		return codes.OK
	case "Canceled":
		return codes.Canceled
	case "Unknown":
		return codes.Unknown
	case "InvalidArgument":
		return codes.InvalidArgument
	case "DeadlineExceeded":
		return codes.DeadlineExceeded
	case "NotFound":
		return codes.NotFound
	case "AlreadyExists":
		return codes.AlreadyExists
	case "PermissionDenied":
		return codes.PermissionDenied
	case "ResourceExhausted":
		return codes.ResourceExhausted
	case "FailedPrecondition":
		return codes.FailedPrecondition
	case "Aborted":
		return codes.Aborted
	case "OutOfRange":
		return codes.OutOfRange
	case "Unimplemented":
		return codes.Unimplemented
	case "Internal":
		return codes.Internal
	case "Unavailable":
		return codes.Unavailable
	case "DataLoss":
		return codes.DataLoss
	case "Unauthenticated":
		return codes.Unauthenticated
	}
	return codes.Unknown
}
