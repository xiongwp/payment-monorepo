//go:build fx
package main
import (
	"github.com/xiongwp/payment-util/scaffold"
	"go.uber.org/fx"
)
func main() {
	scaffold.RunFx(scaffold.FxOpts{
		ServiceName:  "billing-system",
		ConfigPath:   "config/config.yaml",
		MigrationDir: "migrations",
		AppModules: []fx.Option{
			// fx.Provide(repo.NewMySQL)
			// fx.Provide(feerule.NewEngine)
			// fx.Provide(statement.NewAggregator)
			// fx.Provide(trialbalance.NewDaily)  // CronJob 调
		},
	})
}
