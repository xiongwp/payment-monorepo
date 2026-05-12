//go:build fx
package main
import (
	"github.com/xiongwp/payment-util/scaffold"
	"go.uber.org/fx"
	"google.golang.org/grpc"
)
func main() {
	scaffold.RunFx(scaffold.FxOpts{
		ServiceName:  "kms-manage",
		ConfigPath:   "config/config.yaml",
		GRPC: &scaffold.GRPCOpts{
			Addr: ":9390",
			Register: func(srv *grpc.Server) {},
		},
		AppModules: []fx.Option{
			// fx.Provide(hsm.NewClient)
			// fx.Provide(envelope.NewWrapper)
			// fx.Provide(service.NewKMSManage)
		},
	})
}
