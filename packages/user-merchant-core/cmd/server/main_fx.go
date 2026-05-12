//go:build fx

package main

import (
	"github.com/xiongwp/payment-util/scaffold"
	"go.uber.org/fx"
	"google.golang.org/grpc"
)

func main() {
	scaffold.RunFx(scaffold.FxOpts{
		ServiceName:  "user-merchant-core",
		ConfigPath:   "config/config.yaml",
		MigrationDir: "migrations",
		GRPC: &scaffold.GRPCOpts{
			Addr: ":9191",
			Register: func(srv *grpc.Server) {
				// pb.RegisterUserMerchantCoreServer(srv, impl)
			},
		},
		AppModules: []fx.Option{
			// fx.Provide(repo.NewMetaShard) // 1 meta + 10 shard
			// fx.Provide(service.NewUserMerchant)
		},
	})
}
