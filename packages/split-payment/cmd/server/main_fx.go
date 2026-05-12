//go:build fx
package main
import (
	"github.com/xiongwp/payment-util/scaffold"
	"go.uber.org/fx"
)
func main() {
	scaffold.RunFx(scaffold.FxOpts{
		ServiceName:  "split-payment",
		ConfigPath:   "config/config.yaml",
		MigrationDir: "migrations",
		AppModules: []fx.Option{
			// fx.Provide(workflow.NewTranslator)  // Graph + event → RunPlan
			// fx.Provide(workflow.NewEngine)      // Kafka consumer → translate → accounting
			// fx.Provide(clients.NewAccounting)
		},
	})
}
