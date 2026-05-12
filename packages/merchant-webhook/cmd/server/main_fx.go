//go:build fx
package main
import (
	"github.com/xiongwp/payment-util/scaffold"
	"go.uber.org/fx"
)
func main() {
	scaffold.RunFx(scaffold.FxOpts{
		ServiceName:  "merchant-webhook",
		ConfigPath:   "config/config.yaml",
		MigrationDir: "migrations",
		AppModules: []fx.Option{
			// fx.Provide(repo.NewMemoryOrMySQL)
			// fx.Provide(dispatcher.New)        // worker pool + 重试 + DLQ
			// fx.Provide(sign.NewMultiKeyRotator) // 双 key 滚动
		},
	})
}
