//go:build fx

package main

import (
	"github.com/xiongwp/payment-util/scaffold"
	"go.uber.org/fx"
	"google.golang.org/grpc"
)

func main() {
	scaffold.RunFx(scaffold.FxOpts{
		ServiceName:  "order-core",
		ConfigPath:   "config/config.yaml",
		MigrationDir: "migrations",
		GRPC: &scaffold.GRPCOpts{
			Addr: ":9091",
			Register: func(srv *grpc.Server) {
				// pb.RegisterOrderCoreServer(srv, impl)
			},
		},
		AppModules: []fx.Option{
			// fx.Provide(repo.NewSharded)  // 10 shard router
			// fx.Provide(service.NewOrderCore)
		},
	})
}
