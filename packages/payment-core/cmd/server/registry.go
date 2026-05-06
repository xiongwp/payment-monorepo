// Service self-registration to etcd. payment-core 启动期把自己写到 etcd，
// 调用方（payment-admin-web BFF / order-core）通过 etcd resolver 拿到所有
// 活副本 + round_robin LB。registry.endpoints 留空则跳过。
//
// 配置：
//   config.yaml:
//     registry:
//       endpoints: ["etcd:2379"]
//       service_name: payment-core   # 默认就是 "payment-core"
//       advertise_host: ""           # 默认 os.Hostname()，docker 用容器 ID
//       ttl: 10s                     # 默认 10s
//   或 env: PAYCORE_REGISTRY_ENDPOINTS=etcd:2379
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
		service = "payment-core"
	}
	port := v.GetInt("server.grpc_port")
	if port == 0 {
		port = 9090
	}
	// 注册地址：viper override > serviceregistry.AdvertiseAddr (env / 探主网卡 IP / hostname 兜底)
	// 之前直接 os.Hostname() 会拿到容器 ID，docker DNS 不解析它，client 拿到端点后无法 dial。
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
				// 不 fail-fast：etcd 抖动时不希望 payment-core 整片重启；
				// 调用方拨号会拿到 "no children to pick from" → 503，
				// 由 BFF 直连 fallback 兜住（mustDial 在 etcd 拨号失败时不会启用 fallback，
				// 这里 graceful degrade 仍依赖运维监控 etcd 注册数）。
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
			// SelfRegistration.Close() 自身处理 nil 接收者，但保持显式 nil 检查更清晰。
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
