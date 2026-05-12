//go:build fx
package main
import (
	"github.com/xiongwp/payment-util/scaffold"
	"go.uber.org/fx"
	"google.golang.org/grpc"
)
func main() {
	scaffold.RunFx(scaffold.FxOpts{
		ServiceName:  "card-payment",
		ConfigPath:   "config/config.yaml",
		MigrationDir: "migrations",
		GRPC: &scaffold.GRPCOpts{
			Addr: ":9444",
			Register: func(srv *grpc.Server) {},
		},
		AppModules: []fx.Option{
			// fx.Provide(vault.NewClient)
			// fx.Provide(threeds.NewClient)
			// fx.Provide(service.NewCardPayment)
		},
	})
}
