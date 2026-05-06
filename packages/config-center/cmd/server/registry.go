// Service self-registration to etcd. config-center 启动期把自己写到 etcd；
// 业务服务 SDK 可用 etcd:///config-center 走 round_robin LB。
//
// registry.endpoints 留空 → 跳过自注册（dev 单实例 / docker 直连 hostname）。
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
		logger.Info("registry.endpoints empty; config-center skipping etcd self-registration")
		return
	}
	service := v.GetString("registry.service_name")
	if service == "" {
		service = "config-center"
	}
	port := v.GetInt("server.http_port")
	if port == 0 {
		port = 9691
	}
	// 注册地址三层优先级：advertise_host > AdvertiseAddr (POD_IP env / 主网卡探测) > hostname
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
				// 注册失败不阻断启动；HTTP listener 仍在跑，业务可走直连 endpoint
				logger.Error("config-center self-registration failed",
					zap.Error(err),
					zap.String("service", service),
					zap.String("addr", addr))
				return nil
			}
			sr = s
			logger.Info("config-center registered to etcd",
				zap.String("service", service),
				zap.String("addr", addr),
				zap.Strings("etcd", endpoints),
				zap.Duration("ttl", ttl))
			return nil
		},
		OnStop: func(_ context.Context) error {
			if sr == nil {
				return nil
			}
			if err := sr.Close(); err != nil {
				logger.Warn("config-center deregister failed", zap.Error(err))
			}
			return nil
		},
	})
}
