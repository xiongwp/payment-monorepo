//go:build fx
package main
import (
	"github.com/xiongwp/payment-util/scaffold"
	"go.uber.org/fx"
)
func main() {
	scaffold.RunFx(scaffold.FxOpts{
		ServiceName:  "kyc-service",
		ConfigPath:   "config/config.yaml",
		MigrationDir: "migrations",
		AppModules: []fx.Option{
			// fx.Provide(repo.NewMySQL)
			// fx.Provide(amlclient.New)        // 接 aml-screening
			// fx.Provide(idverify.NewPersona)  // 接 Persona / Onfido / Trulioo
			// fx.Provide(service.NewKYB)
		},
	})
}
