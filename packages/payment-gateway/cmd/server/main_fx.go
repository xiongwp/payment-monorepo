//go:build fx
package main
import (
	"github.com/xiongwp/payment-util/scaffold"
	"go.uber.org/fx"
)
func main() {
	scaffold.RunFx(scaffold.FxOpts{
		ServiceName:  "payment-gateway",
		ConfigPath:   "config/config.yaml",
		MigrationDir: "migrations",
		AppModules: []fx.Option{
			// fx.Provide(routing.NewSelector)    // 商户 → channel 路由
			// fx.Provide(checkout.NewSession)    // 统一收银台
			// fx.Provide(sdk.NewJSStub)
		},
	})
}
