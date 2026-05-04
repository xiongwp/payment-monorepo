// 本包是 payment-util/shadow 的薄壳；核心契约的单测放在
// payment-util/shadow/shadow_test.go。这里只做最小的 smoke test：
// 验证 alias 转发不丢语义。
package shadow

import (
	"context"
	"testing"
)

// 转发链最基本验证：本包 WithShadow / IsShadow 能正常往返。
// 任何 alias 出错（比如未来误把变量赋成 nil）会立刻在这里炸。
func TestAliasRoundTrip(t *testing.T) {
	ctx := WithShadow(context.Background(), true)
	if !IsShadow(ctx) {
		t.Fatal("alias forward broken: WithShadow→IsShadow lost flag")
	}
	if got := TableName(ctx, "foo"); got != "foo"+Suffix {
		t.Fatalf("alias TableName = %s, want foo%s", got, Suffix)
	}
}
