//go:build fx
package main
import (
	"github.com/xiongwp/payment-util/scaffold"
	"go.uber.org/fx"
)
func main() {
	scaffold.RunFx(scaffold.FxOpts{
		ServiceName: "accounting-admin-web",
		ConfigPath:  "config/config.yaml",
		AppModules: []fx.Option{
			// fx.Provide(client.NewAccountingSystem)  // grpc client
			// 注册路由: /api/v1/balance / /api/v1/journal / /api/v1/reports
		},
	})
}
