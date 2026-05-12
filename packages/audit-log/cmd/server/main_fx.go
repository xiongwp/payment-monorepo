//go:build fx
package main
import (
	"github.com/xiongwp/payment-util/scaffold"
	"go.uber.org/fx"
)
func main() {
	scaffold.RunFx(scaffold.FxOpts{
		ServiceName:  "audit-log",
		ConfigPath:   "config/config.yaml",
		MigrationDir: "migrations",
		AppModules: []fx.Option{
			// fx.Provide(chain.NewHashChain) // sha256 chain (防篡改)
			// fx.Provide(repo.NewAppendOnly)
			// 注册路由: /api/v1/audit/log /batch /logs /verify /export
		},
	})
}
