//go:build fx
package main
import (
	"github.com/xiongwp/payment-util/scaffold"
	"go.uber.org/fx"
)
func main() {
	scaffold.RunFx(scaffold.FxOpts{
		ServiceName: "payment-admin-web",
		ConfigPath:  "config/config.yaml",
		AppModules: []fx.Option{
			// fx.Provide(client.NewPaymentCore)
			// fx.Provide(client.NewRiskManage)
			// 注册路由 + 监控页面
		},
	})
}
