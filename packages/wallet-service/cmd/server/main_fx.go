//go:build fx
package main
import (
	"github.com/xiongwp/payment-util/scaffold"
	"go.uber.org/fx"
)
func main() {
	scaffold.RunFx(scaffold.FxOpts{
		ServiceName:  "wallet-service",
		ConfigPath:   "config/config.yaml",
		MigrationDir: "migrations",
		AppModules: []fx.Option{
			// fx.Provide(repo.NewMySQL)
			// fx.Provide(client.NewAccounting)   // 余额走 accounting
			// fx.Provide(kyc.NewTierEvaluator)
			// fx.Provide(service.NewWallet)
		},
	})
}
