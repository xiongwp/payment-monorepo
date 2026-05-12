//go:build fx
package main
import (
	"github.com/xiongwp/payment-util/scaffold"
	"go.uber.org/fx"
)
func main() {
	scaffold.RunFx(scaffold.FxOpts{
		ServiceName: "biz-admin-web",
		ConfigPath:  "config/config.yaml",
		AppModules: []fx.Option{
			// fx.Provide(proxy.NewMultiBackend) // 反代到 11 个 backend
			// fx.Invoke 注册:
			//   /                 → admin.html (Alpine)
			//   /p0               → p0-services.html
			//   /approval         → approval.html
			//   /static-react/*   → React SPA (新)
			//   /api/<svc>/*      → proxy
			//   /health/all       → 各 backend healthz 聚合
		},
	})
}
