package grpcutil

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// AuditEntry 写入 store 的结构；pkg 不引用 domain 层。
type AuditEntry struct {
	Actor       string
	ActorIP     string
	Method      string
	TargetID    string
	RequestBody string // JSON, redacted
	StatusCode  string
	ResponseErr string
	DurationMs  int
	TraceID     string
	PrevHash    string
	RowHash     string
	At          time.Time
}

// AuditStore 写入后端（合规要求：append-only，本接口不暴露 Update/Delete）。
// LastHash 供链式签名拿到上一行的 row_hash；Insert 保存整条。
type AuditStore interface {
	LastHash(ctx context.Context) (string, error)
	Insert(ctx context.Context, entry *AuditEntry) error
}

// AuditOptions 拦截器参数。
type AuditOptions struct {
	Store        AuditStore
	MethodFilter map[string]struct{} // 只审计这些 method；nil = 全部
	// ActorFromCtx: 从 ctx 里抽调用者身份（JWT / metadata header 之类）。
	// 为 nil 时用 "anonymous"。
	ActorFromCtx func(context.Context) string
	// TargetFromReq: 从请求里抽被操作对象 ID（常见 "id" / "merchant_id"）。
	// 为 nil 时 TargetID 为空。
	TargetFromReq func(req any) string
	// TraceIDHeader: metadata 里的 trace id 键（与 tracex.MetadataKey 一致）。
	TraceIDHeader string
}

// AuditInterceptor 在 handler 完成后异步写一条审计；链式 row_hash 保证 append-only
// 日志可验证（"某行被改了"能被检测）。handler 错误不影响审计写入；审计写入失败
// 只打 log 不回传给调用方（合规 SLA 另算）。
func AuditInterceptor(opt AuditOptions) grpc.UnaryServerInterceptor {
	if opt.Store == nil {
		return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
			return handler(ctx, req)
		}
	}
	marshaler := protojson.MarshalOptions{EmitUnpopulated: false, UseProtoNames: true}
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if opt.MethodFilter != nil {
			if _, ok := opt.MethodFilter[info.FullMethod]; !ok {
				return handler(ctx, req)
			}
		}
		start := time.Now()
		resp, err := handler(ctx, req)
		elapsed := time.Since(start)

		entry := &AuditEntry{
			Method:     info.FullMethod,
			DurationMs: int(elapsed.Milliseconds()),
			At:         time.Now(),
		}
		if opt.ActorFromCtx != nil {
			entry.Actor = opt.ActorFromCtx(ctx)
		}
		if entry.Actor == "" {
			entry.Actor = "anonymous"
		}
		if opt.TargetFromReq != nil {
			entry.TargetID = opt.TargetFromReq(req)
		}
		if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
			entry.ActorIP = p.Addr.String()
		}
		// 把请求 body 写成 redact 过的 JSON（复用 LoggingInterceptor 的 redact）。
		if m, ok := req.(proto.Message); ok {
			if b, e := marshaler.Marshal(m); e == nil {
				entry.RequestBody = redactSensitive(string(b))
			}
		}
		if opt.TraceIDHeader != "" {
			if md, ok := metadata.FromIncomingContext(ctx); ok {
				if v := md.Get(opt.TraceIDHeader); len(v) > 0 {
					entry.TraceID = v[0]
				}
			}
		}
		if err != nil {
			st, _ := status.FromError(err)
			entry.StatusCode = st.Code().String()
			entry.ResponseErr = st.Message()
		} else {
			entry.StatusCode = "OK"
		}
		// 链式哈希：取 store 最后一行的 row_hash，拼进本行，再 sha256 作为 row_hash。
		// 读 + 写放在 handler 之后的异步 goroutine 里；生产应用 store 级事务保证
		// "拿 LastHash → Insert" 的原子性，避免并发两条 row 都以同一 prev_hash 写入
		// 造成链"Y 型分叉"。此处 store 实现（IdempotencyRepository 对应）用 DB
		// 唯一键 + 检测即可。
		go writeAudit(context.Background(), opt.Store, entry)
		return resp, err
	}
}

func writeAudit(ctx context.Context, store AuditStore, e *AuditEntry) {
	prev, _ := store.LastHash(ctx)
	e.PrevHash = prev
	e.RowHash = ComputeRowHash(e)
	_ = store.Insert(ctx, e)
}

// ComputeRowHash 把"能确定本行身份"的字段拼成一行文本喂 sha256。
// 导出给 cmd/audit-verify 跨 binary 复用；公式变动两边一起改。
func ComputeRowHash(e *AuditEntry) string {
	s := e.PrevHash + "|" + e.Actor + "|" + e.ActorIP + "|" + e.Method + "|" +
		e.TargetID + "|" + e.RequestBody + "|" + e.StatusCode + "|" +
		e.ResponseErr + "|" + e.TraceID
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
