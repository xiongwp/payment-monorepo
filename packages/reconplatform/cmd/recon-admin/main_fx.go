//go:build fx
package main
import (
	"github.com/xiongwp/payment-util/scaffold"
	"go.uber.org/fx"
)
// reconplatform admin web — CDC + 动态对账 + Monaco editor + cytoscape graph.
func main() {
	scaffold.RunFx(scaffold.FxOpts{
		ServiceName: "reconplatform-admin",
		ConfigPath:  "config/config.yaml",
		AppModules: []fx.Option{
			// fx.Provide(scripts.NewMonacoEditor)  // /scripts/* 路由
			// fx.Provide(diffs.NewBrowser)         // /api/v1/diffs
			// fx.Provide(graph.NewCytoscape)        // 跨服务事件图
			// fx.Provide(dashboards.NewSSE)         // 实时事件流
		},
	})
}
