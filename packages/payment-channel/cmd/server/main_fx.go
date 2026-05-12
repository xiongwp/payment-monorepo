// main_fx.go — payment-channel uber/fx 版.
//go:build fx

package main

import (
	"github.com/xiongwp/payment-util/scaffold"
	"go.uber.org/fx"
	"google.golang.org/grpc"
)

func main() {
	scaffold.RunFx(scaffold.FxOpts{
		ServiceName:  "payment-channel",
		ConfigPath:   "config/config.yaml",
		MigrationDir: "migrations",
		GRPC: &scaffold.GRPCOpts{
			Addr: ":9092",
			Register: func(srv *grpc.Server) {
				// pb.RegisterPaymentChannelServer(srv, impl)
			},
		},
		AppModules: []fx.Option{
			// fx.Provide(adapter.NewRegistry)
			// fx.Provide(breaker.NewManager)
			// fx.Provide(threeds.NewEngine)
			// fx.Provide(service.NewPaymentChannel)
		},
	})
}
