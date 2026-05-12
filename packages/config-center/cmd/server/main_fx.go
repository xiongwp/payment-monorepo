//go:build fx
package main
import (
	"github.com/xiongwp/payment-util/scaffold"
	"go.uber.org/fx"
)
func main() {
	scaffold.RunFx(scaffold.FxOpts{
		ServiceName: "config-center",
		ConfigPath:  "config/config.yaml",
		AppModules: []fx.Option{
			// fx.Provide(etcd.NewClient)
			// fx.Provide(sse.NewPublisher)  // 配置变更推订阅方
			// fx.Provide(service.NewConfig)
		},
	})
}
