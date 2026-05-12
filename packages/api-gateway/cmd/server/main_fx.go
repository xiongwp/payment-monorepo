//go:build fx
package main
import (
	"github.com/xiongwp/payment-util/scaffold"
	"go.uber.org/fx"
)
func main() {
	scaffold.RunFx(scaffold.FxOpts{
		ServiceName: "api-gateway",
		ConfigPath:  "config/config.yaml",
		AppModules: []fx.Option{
			// fx.Provide(router.NewBFF)            // 商户面 BFF 路由
			// fx.Provide(client.NewPaymentCore)
			// fx.Provide(client.NewOrderCore)
			// fx.Provide(client.NewUserMerchantCore)
			// fx.Invoke 注册 fiber routes /v1/charges, /v1/refunds, /v1/customers ...
		},
	})
}
