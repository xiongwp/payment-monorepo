//go:build fx
package main
// accounting-grpc-api 主要是 proto + client lib;
// 如果它也有 server, 用同模板. 否则 skip.
import (
	"github.com/xiongwp/payment-util/scaffold"
	"go.uber.org/fx"
)
func main() {
	scaffold.RunFx(scaffold.FxOpts{
		ServiceName: "accounting-grpc-api",
		ConfigPath:  "config/config.yaml",
		AppModules:  []fx.Option{},
	})
}
