// Service self-registration to etcd. payment-channel 启动期把自己写到 etcd。
// payment-core 通过 etcd resolver 拿到所有活副本 + round_robin LB。
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
		service = "payment-channel"
	}
	port := v.GetInt("server.grpc_port")
	if port == 0 {
		port = 9092
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
			if err := sr.Close(); err != nil {
				logger.Warn("service deregister failed", zap.Error(err))
			} else if sr != nil {
				logger.Info("service deregistered from etcd",
					zap.String("service", service), zap.String("addr", addr))
			}
			return nil
		},
	})
}
