// Service self-registration to etcd. kms-manage 启动期把自己写到 etcd。
// 调用方（payment-channel / user-merchant-core）通过 etcd resolver 拿到所有
// 活副本 + round_robin LB。registry.endpoints 留空则跳过。
//
// 注意：kms-manage 持有 master key，多副本部署时所有副本必须读到同一份 keystore
// （K8s 用 ReadOnlyMany PV 或 Vault sidecar）。本注册只做服务发现，不解决密钥同步。
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/viper"
	"github.com/xiongwp/payment-util/serviceregistry"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

func startServiceRegistrar(lc fx.Lifecycle, v *viper.Viper, logger *zap.Logger) {
	endpoints := v.GetStringSlice("registry.endpoints")
	if len(endpoints) == 0 {
		logger.Info("registry.endpoints empty; service self-registration disabled")
		return
	}
	service := v.GetString("registry.service_name")
	if service == "" {
		service = "kms-manage"
	}
	port := v.GetInt("server.grpc_port")
	if port == 0 {
		port = 9290
	}
	host := os.Getenv("REGISTRY_ADVERTISE_ADDR")
	if host == "" {
		host = v.GetString("registry.advertise_host")
	}
	if host == "" {
		h, err := os.Hostname()
		if err != nil {
			logger.Warn("os.Hostname failed; service registration skipped", zap.Error(err))
			return
		}
		host = h
	}
	addr := fmt.Sprintf("%s:%d", host, port)

	ttl := v.GetDuration("registry.ttl")
	if ttl < time.Second {
		ttl = 10 * time.Second
	}

	var sr *serviceregistry.SelfRegistration
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			regCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			s, err := serviceregistry.RegisterSelf(regCtx, endpoints, service, addr, ttl)
			if err != nil {
				logger.Error("service registration failed", zap.Error(err),
					zap.String("service", service), zap.String("addr", addr))
				return nil
			}
			sr = s
			logger.Info("service registered to etcd",
				zap.String("service", service), zap.String("addr", addr),
				zap.Strings("etcd", endpoints), zap.Duration("ttl", ttl))
			return nil
		},
		OnStop: func(_ context.Context) error {
			// 必须先判 nil：OnStart 注册失败时 sr 还是 nil，直接 sr.Close()
			// 会 nil-deref；之前 else-if 在 Close() 之后判断已经晚了。
			if sr == nil {
				return nil
			}
			if err := sr.Close(); err != nil {
				logger.Warn("service deregister failed", zap.Error(err))
			} else {
				logger.Info("service deregistered from etcd",
					zap.String("service", service), zap.String("addr", addr))
			}
			return nil
		},
	})
}
