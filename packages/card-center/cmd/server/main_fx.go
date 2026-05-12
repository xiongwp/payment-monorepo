//go:build fx
package main
import (
	"github.com/xiongwp/payment-util/scaffold"
	"go.uber.org/fx"
	"google.golang.org/grpc"
)
func main() {
	scaffold.RunFx(scaffold.FxOpts{
		ServiceName:  "card-center",
		ConfigPath:   "config/config.yaml",
		MigrationDir: "migrations",
		GRPC: &scaffold.GRPCOpts{
			Addr: ":9443",
			Register: func(srv *grpc.Server) { /* pb.RegisterCardCenterServer */ },
		},
		AppModules: []fx.Option{
			// fx.Provide(vault.NewClient)  // 接 tokenization-vault
		},
	})
}
