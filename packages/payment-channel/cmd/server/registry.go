// Service self-registration to etcd — REG-REDESIGN no-op stub.
// 见 packages/payment-core/cmd/server/registry.go 的注释; 同样原因.
package main

import (
	"github.com/spf13/viper"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

func startServiceRegistrar(_ fx.Lifecycle, _ *viper.Viper, logger *zap.Logger) {
	logger.Debug("startServiceRegistrar: no-op (REG-REDESIGN — 注册走 kitexutil.DefaultServerOptions)")
}
