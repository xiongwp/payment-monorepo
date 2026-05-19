package server

import (
	"context"
	"reflect"

	"google.golang.org/grpc/metadata"
)

// actorFromAuth 拦截器用：解析当前调用方的 actor 标识。
//
// 优先级：
//  1. metadata `x-admin-actor`：上游 admin-backend 在自己的 JWT/Cookie 验证后
//     注入的真实操作员标识（user@domain）；最权威。
//  2. AuthInterceptor 鉴权通过后 ctx 里携带的 caller label（svc-a / admin-web 等）
//     ——指向调用方服务，不指向具体操作员。
//  3. 都没有 → "anonymous"，由审计层决定是否拒绝。
//
// **不再回退到 Bearer token 前缀**。token 是凭据本身，截前缀写审计有两个风险：
//   - 凭据片段写入持久化日志/审计表 → 泄露
//   - 攻击者用任意 "Bearer admin___xxxx" 即可让审计记录显示成 admin___，可伪造
func actorFromAuth(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if ok {
		if vals := md.Get("x-admin-actor"); len(vals) > 0 && vals[0] != "" {
			return vals[0]
		}
	}
	// 老 grpcutil.CallerFromContext (gRPC AuthInterceptor 注入) 已删 —
	// Kitex 切换后 AuthMW 待 port; 此处先返 anonymous, 等 kitexutil.AuthMW
	// 接通后再加 metainfo 路径.
	return "anonymous"
}

// targetFromRequest 常见 proto 字段名探测：id / merchant_id。找不到返回 ""。
// 反射一次是可接受的开销；热路径 AuthenticateByAPIKey 不在 MutationMethods 白名单里。
func targetFromRequest(req any) string {
	if req == nil {
		return ""
	}
	v := reflect.ValueOf(req)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return ""
	}
	// 生成的 proto struct 字段名是 PascalCase：Id / MerchantId
	for _, name := range []string{"Id", "MerchantId", "MerchantID"} {
		if f := v.FieldByName(name); f.IsValid() && f.Kind() == reflect.String {
			if s := f.String(); s != "" {
				return s
			}
		}
	}
	return ""
}
