// REG-REDESIGN: StartServiceRegistrar 已变 no-op.
// 注册路径统一到 payment-util/kitexutil.DefaultServerOptions.
//
// NewEtcdClient 保留 — 给 leader election / shared etcd client 用 (不做注册).
package main

import (
	"fmt"

	"github.com/spf13/viper"
	"github.com/xiongwp/payment-util/serviceregistry"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

// NewEtcdClient creates an etcd client used for leader election / shared etcd
// state. Service registration is handled separately by kitexutil.
// Returns (nil, nil) when registry.endpoints is empty so dev still boots.
func NewEtcdClient(v *viper.Viper, logger *zap.Logger) (*clientv3.Client, error) {
	endpoints := v.GetStringSlice("registry.endpoints")
	if len(endpoints) == 0 {
		logger.Info("registry.endpoints empty; leader election etcd client disabled")
		return nil, nil
	}
	cli, err := serviceregistry.NewEtcdClient(endpoints)
	if err != nil {
		return nil, fmt.Errorf("etcd client: %w", err)
	}
	logger.Info("etcd client connected (for leader election)", zap.Strings("endpoints", endpoints))
	return cli, nil
}

// StartServiceRegistrar — REG-REDESIGN no-op.
// 注册走 kitexutil.DefaultServerOptions. 此函数保留签名让 main.go fx.Invoke
// 不破坏 build.
func StartServiceRegistrar(_ fx.Lifecycle, _ *clientv3.Client, _ *viper.Viper, logger *zap.Logger) {
	logger.Debug("StartServiceRegistrar: no-op (REG-REDESIGN — 注册走 kitexutil.DefaultServerOptions)")
}
