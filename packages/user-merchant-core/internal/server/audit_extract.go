package server

import (
	"context"
	"reflect"

	"github.com/cloudwego/kitex/pkg/rpcinfo"
)

// actorFromAuth 拦截器用: 解析当前调用方的 actor 标识.
//
// 优先级:
//  1. Kitex TTHeader/metainfo `x-admin-actor`: 上游 admin-backend 在自己的
//     JWT/Cookie 验证后注入的真实操作员标识 (user@domain); 最权威.
//  2. 老 gRPC AuthInterceptor 注入的 caller (已删, Kitex AuthMW port 后接).
//  3. 都没有 → "anonymous", 由审计层决定是否拒绝.
//
// **不再回退到 Bearer token 前缀** — token 是凭据本身, 截前缀写审计有两个风险:
//   - 凭据片段写入持久化日志/审计表 → 泄露
//   - 攻击者用任意 "Bearer admin___xxxx" 即可让审计记录显示成 admin___, 可伪造
func actorFromAuth(ctx context.Context) string {
	// Kitex 从 RPCInfo.Invocation 拿 transient kv (TTHeader 入站 metainfo).
	if ri := rpcinfo.GetRPCInfo(ctx); ri != nil {
		if from := ri.From(); from != nil {
			if actor, ok := from.Tag("x-admin-actor"); ok && actor != "" {
				return actor
			}
		}
	}
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
