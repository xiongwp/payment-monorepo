//go:build fx
package main
import (
	"github.com/xiongwp/payment-util/scaffold"
	"go.uber.org/fx"
)
func main() {
	scaffold.RunFx(scaffold.FxOpts{
		ServiceName:  "oauth2-server",
		ConfigPath:   "config/config.yaml",
		MigrationDir: "database/init",
		AppModules: []fx.Option{
			// fx.Provide(store.NewSharded) // 100 shard
			// fx.Provide(jwks.NewRSAKeyStore)
			// fx.Provide(ratelimit.NewPerClient)
			// fx.Provide(adminhttp.NewServer)
		},
	})
}
