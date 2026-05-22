// shadow.go — 全链路压测 shadow flag 的 context-based 传递
//
// 设计：跟 accounting-system 的 shadow_callback 思路对齐。
// 流量入口（loadtest / grpcsvc 中间件）把 ctx 打上 shadow=true 标记，repo 层用
// IsShadow(ctx) 判断后路由到 *_shadow 子表。生产业务请求不带这个 flag，正常走主表。
//
// 用法示例（caller 侧）：
//   ctx = sharding.WithShadow(ctx)              // mark traffic as shadow
//   _, err = runRepo.Save(ctx, &domain.RunPlan{ ... })  // 自动落 moneyflow_runs_NN_shadow
//
// 用法示例（repo 侧）：
//   shadow := sharding.IsShadow(ctx)
//   tableName := r.router.TableName(family, tblIdx, shadow)
//
// 注意：shadow flag 不应该再往下传给 accounting-service（split-payment → accounting
// 的 gRPC call）—— 只在 split-payment 自己的 DB 上影子化。loadtest 走 accounting
// 是否影子化由 accounting 自己的 metadata 控制（跟 accounting shadow_callback 协同）。
package sharding

import "context"

// shadowKey context.WithValue 用的 unexported type，避免 key 冲突
type shadowKeyType struct{}

var shadowKey = shadowKeyType{}

// WithShadow 在 ctx 上标记 shadow=true. 之后这条调用链所有 repo 操作都走 _shadow 子表.
func WithShadow(ctx context.Context) context.Context {
	return context.WithValue(ctx, shadowKey, true)
}

// IsShadow 读 ctx 上的 shadow flag. 没设过返 false（默认走主表）.
func IsShadow(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(shadowKey).(bool)
	return v
}
