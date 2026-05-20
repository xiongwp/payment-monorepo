// Service self-registration to etcd — REG-REDESIGN no-op stub.
//
// 老 startServiceRegistrar 用 serviceregistry.RegisterSelf 写 etcd, 跟 kitexutil
// EtcdRegistry 双写同一份数据 (不同 lease + 不同 addr 格式) 导致 caller 间歇性
// 拿到 stale endpoint 拨号失败. REG-REDESIGN 后所有 etcd 注册收敛到
// payment-util/kitexutil.DefaultServerOptions (server.go), 本函数仅保留签名让
// main.go fx.Invoke 不破坏 build, 实际是 no-op.
package main

import (
	"github.com/spf13/viper"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

func startServiceRegistrar(_ fx.Lifecycle, _ *viper.Viper, logger *zap.Logger) {
	logger.Debug("startServiceRegistrar: no-op (REG-REDESIGN — 注册走 kitexutil.DefaultServerOptions)")
}
