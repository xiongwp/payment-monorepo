// main_fx.go — payment-core uber/fx 版.

//go:build fx

package main

import (
	"github.com/xiongwp/payment-util/scaffold"
	"go.uber.org/fx"
	"google.golang.org/grpc"
)

func main() {
	scaffold.RunFx(scaffold.FxOpts{
		ServiceName:  "payment-core",
		ConfigPath:   "config/config.yaml",
		MigrationDir: "migrations",

		// payment-core 主要是 gRPC server
		GRPC: &scaffold.GRPCOpts{
			Addr: ":9090",
			Register: func(srv *grpc.Server) {
				// pb.RegisterPaymentCoreServer(srv, impl)
				// 现 main.go 里的 NewPaymentService(...) + 注册逻辑搬过来
			},
		},

		AppModules: []fx.Option{
			// payment-core 业务依赖 (复用现有 internal pkg):
			//   fx.Provide(channelclient.New)
			//   fx.Provide(riskclient.New)
			//   fx.Provide(kmsclient.New)
			//   fx.Provide(routing.NewRouter)
			//   fx.Provide(service.New)
			//
			// 跟 main.go 原 wire 逻辑等价, 但 fx.Decorate 测试时可 mock 任意 client.
		},
	})
}
