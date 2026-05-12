//go:build fx
package main
import (
	"github.com/xiongwp/payment-util/scaffold"
	"go.uber.org/fx"
	"google.golang.org/grpc"
)
func main() {
	scaffold.RunFx(scaffold.FxOpts{
		ServiceName:  "accounting-system",
		ConfigPath:   "config/config.yaml",
		MigrationDir: "migrations",
		GRPC: &scaffold.GRPCOpts{
			Addr: ":9290",
			Register: func(srv *grpc.Server) {},
		},
		AppModules: []fx.Option{
			// fx.Provide(repo.NewShardedJournal)
			// fx.Provide(service.NewAtomicBatchBooking)
			// fx.Provide(outboxconsumer.NewHandler) // 接 payment/refund/payout 事件
		},
	})
}
