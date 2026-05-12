//go:build fx
package main
import (
	"github.com/xiongwp/payment-util/scaffold"
	"go.uber.org/fx"
)
func main() {
	scaffold.RunFx(scaffold.FxOpts{
		ServiceName:  "fx-service",
		ConfigPath:   "config/config.yaml",
		MigrationDir: "migrations",
		AppModules: []fx.Option{
			// fx.Provide(sources.NewECB)
			// fx.Provide(sources.NewOANDA)
			// fx.Provide(sources.NewStripe)
			// fx.Provide(aggregator.NewMidBidAsk)
			// fx.Provide(quote.NewLocker)         // 30s TTL 锁价
		},
	})
}
