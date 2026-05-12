//go:build fx
package main
import (
	"github.com/xiongwp/payment-util/scaffold"
	"go.uber.org/fx"
)
func main() {
	scaffold.RunFx(scaffold.FxOpts{
		ServiceName:  "refund-engine",
		ConfigPath:   "config/config.yaml",
		MigrationDir: "migrations",
		AppModules: []fx.Option{
			// fx.Provide(repo.NewMySQL)
			// fx.Provide(webhook.NewClient)   // 接 merchant-webhook
			// fx.Provide(outboxhook.NewPublisher)
			// fx.Provide(service.NewRefundEngine)
		},
	})
}
