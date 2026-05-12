//go:build fx
package main
import (
	"github.com/xiongwp/payment-util/scaffold"
	"go.uber.org/fx"
)
func main() {
	scaffold.RunFx(scaffold.FxOpts{
		ServiceName: "id-generator",
		ConfigPath:  "config/config.yaml",
		AppModules: []fx.Option{
			// fx.Provide(leaf.NewAllocator)  // leaf_alloc segment 模式
			// fx.Provide(snowflake.New)      // 备方案
		},
	})
}
