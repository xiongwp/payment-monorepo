//go:build fx
package main
import (
	"github.com/xiongwp/payment-util/scaffold"
	"go.uber.org/fx"
)
// reconplatform engine — Starlark 对账引擎主入口.
// 跟 admin (cmd/recon-admin) 分两 cmd 不同 process.
func main() {
	scaffold.RunFx(scaffold.FxOpts{
		ServiceName: "reconplatform-engine",
		ConfigPath:  "config/config.yaml",
		AppModules: []fx.Option{
			// fx.Provide(starlark.NewEngine)
			// fx.Provide(cdc.NewCanalConsumer)
			// fx.Provide(external.NewManager)    // MT940 / CAMT.053 / CSV
			// fx.Provide(diff.NewAggregator)
			// fx.Provide(notifier.NewMultiSink)  // webhook / 钉钉 / Slack
		},
	})
}
