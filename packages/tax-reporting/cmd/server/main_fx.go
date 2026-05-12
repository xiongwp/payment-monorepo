//go:build fx
package main
import (
	"github.com/xiongwp/payment-util/scaffold"
	"go.uber.org/fx"
)
func main() {
	scaffold.RunFx(scaffold.FxOpts{
		ServiceName:  "tax-reporting",
		ConfigPath:   "config/config.yaml",
		MigrationDir: "migrations",
		AppModules: []fx.Option{
			// fx.Provide(repo.NewGorm)
			// fx.Provide(aggregator.NewMonthly)
			// fx.Provide(forms.NewGenerator)  // 1099-K / W-9 / W-8 / VAT OSS
			// fx.Provide(efile.NewIRSFire)    // IRS FIRE / Avalara
		},
	})
}
