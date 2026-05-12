//go:build fx
package main
import (
	"github.com/xiongwp/payment-util/scaffold"
	"go.uber.org/fx"
	"google.golang.org/grpc"
)
func main() {
	scaffold.RunFx(scaffold.FxOpts{
		ServiceName:  "risk-manage",
		ConfigPath:   "config/config.yaml",
		MigrationDir: "migrations",
		GRPC: &scaffold.GRPCOpts{
			Addr: ":9490",
			Register: func(srv *grpc.Server) {},
		},
		AppModules: []fx.Option{
			// fx.Provide(rules.NewEngine)
			// fx.Provide(mlscore.NewEnsemble)
			// fx.Provide(velocity.NewSlidingWindow)
			// fx.Provide(session.NewStore)
			// fx.Provide(review.NewQueue)
			// fx.Provide(service.NewRiskManage)
		},
	})
}
