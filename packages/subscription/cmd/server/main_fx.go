//go:build fx
package main
import (
	"github.com/xiongwp/payment-util/scaffold"
	"go.uber.org/fx"
)
func main() {
	scaffold.RunFx(scaffold.FxOpts{
		ServiceName:  "subscription",
		ConfigPath:   "config/config.yaml",
		MigrationDir: "migrations",
		AppModules: []fx.Option{
			// fx.Provide(repo.NewMySQL)
			// fx.Provide(workflow.NewCycle)   // dunning + proration + trial
			// fx.Provide(retry.NewSmartDecline)
			// fx.Provide(service.NewSubscription)
		},
	})
}
