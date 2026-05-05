// Service self-registration to etcd. risk-manage 启动期把自己写到 etcd，
// 调用方（payment-core / api-gateway / payment-admin-web BFF / user-merchant-core）
// 通过 etcd resolver 拿到所有活副本 + round_robin LB。
// registry.endpoints 留空则跳过（dev / 单 pod 模式）。
//
// 配置：
//   config.yaml:
//     registry:
//       endpoints: ["etcd:2379"]
//       service_name: risk-manage    # 默认就是 "risk-manage"
//       advertise_host: ""           # 默认 os.Hostname()，docker 用容器 ID
//       ttl: 10s                     # 默认 10s
//   或 env: RISK_REGISTRY_ENDPOINTS=etcd:2379
//
// REGISTRY_ADVERTISE_ADDR 环境变量优先：K8s 注 POD_IP 进来。
package main

import (
	"context"
	"fmt"
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
		service = "risk-manage"
	}
	port := v.GetInt("server.grpc_port")
	if port == 0 {
		port = 9490
	}
var addr string
	if h := v.GetString("registry.advertise_host"); h != "" {
		addr = fmt.Sprintf("%s:%d", h, port)
	} else {
		addr = serviceregistry.AdvertiseAddr(port)
	}

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
				// graceful degrade：etcd 抖动时不阻塞 risk-manage 启动。
				// 同步规则配置 / synthetic worker 仍可独立运行。
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
