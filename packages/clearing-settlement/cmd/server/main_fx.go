//go:build fx
package main
import (
	"github.com/xiongwp/payment-util/scaffold"
	"go.uber.org/fx"
)
func main() {
	scaffold.RunFx(scaffold.FxOpts{
		ServiceName:  "clearing-settlement",
		ConfigPath:   "config/config.yaml",
		MigrationDir: "migrations",
		AppModules: []fx.Option{
			// fx.Provide(repo.NewSharded)
			// fx.Provide(rails.NewDispatcher) // NACHA / SEPA / SWIFT
			// fx.Provide(outboxhook.NewPublisher)
			// fx.Provide(service.NewPayout)
		},
	})
}
