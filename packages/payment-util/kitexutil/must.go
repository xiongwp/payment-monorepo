// must.go — Kitex client 构造 helper, 跟 monorepo 全局 sweep 配套.
//
// Background: KX-WIDE 大批量 sweep 把所有 grpc <pbpkg>.New*Client(conn) 调用
// 改成 kitexutil.MustKitexClient(<svc>service.NewClient("svc")). 这里给一个统一的
// "Must" wrapper 让旧代码语法兼容 (旧的 NewXClient 是 panic-free 返回 1 值,
// Kitex NewClient 返回 (Client, error)).
//
// 真生产代码不应用 mustKitexClient — 应该正经处理 err 并把 endpoint /
// resolver / middleware 配齐. 这是过渡期的兼容 shim.
package kitexutil

import "fmt"

// MustKitexClient panics on dial error — 用于 init / test / batch sweep 占位.
// 业务路径推荐直接接 (Client, error) 处理.
func MustKitexClient[T any](cli T, err error) T {
	if err != nil {
		panic(fmt.Errorf("kitexutil.MustKitexClient: %w", err))
	}
	return cli
}
