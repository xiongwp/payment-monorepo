// 服务自注册到 etcd —— card-center 启动期把 (service="card-center", host:grpc_port)
// 写到 etcd，调用方（card-payment / payment-admin-web BFF / api-gateway）通过
// etcd resolver "etcd:///card-center" 拿到所有活副本 + round_robin LB。
//
// 跟其它服务（order-core / payment-core / kms-manage / payment-channel）的
// startServiceRegistrar 形态一致；card-center 无 cron worker，所以省掉 leader
// election 的 LeaderToolkit，只保留单纯 self-register。
//
// registry.endpoints 留空（dev 单仓 docker run / 本地 unit test）→ 跳过自注册，
// 调用方走 DNS / 直连 fallback；不影响主路径。
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/viper"
	"github.com/xiongwp/payment-util/serviceregistry"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

func startServiceRegistrar(lc fx.Lifecycle, v *viper.Viper, logger *zap.Logger) {
	endpoints := v.GetStringSlice("registry.endpoints")
	if len(endpoints) == 0 {
		endpoints = splitCSV(v.GetString("registry.endpoints"))
	}
	if len(endpoints) == 0 {
		logger.Info("registry.endpoints empty; card-center skipping etcd self-registration")
		return
	}

	service := v.GetString("registry.service_name")
	if service == "" {
		service = "card-center"
	}
	port := v.GetInt("server.grpc_port")
	if port == 0 {
		port = 9443
	}
	host := os.Getenv("REGISTRY_ADVERTISE_ADDR")
	if host == "" {
		host = v.GetString("registry.advertise_host")
	}
	if host == "" {
		host, _ = os.Hostname()
	}
	if host == "" {
		host = "unknown"
	}
	addr := fmt.Sprintf("%s:%d", host, port)

	ttl := v.GetDuration("registry.ttl")
	if ttl < time.Second {
		ttl = 10 * time.Second
	}

	var (
		client *clientv3.Client
		reg    *serviceregistry.Registrar
	)
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			cli, err := serviceregistry.NewEtcdClient(endpoints)
			if err != nil {
				logger.Error("etcd client init failed; skipping self-register",
					zap.Error(err), zap.Strings("endpoints", endpoints))
				return nil
			}
			client = cli

			r, err := serviceregistry.NewRegistrar(client, service, addr)
			if err != nil {
				logger.Error("registrar setup failed", zap.Error(err))
				return nil
			}
			regCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if err := r.Register(regCtx, ttl); err != nil {
				logger.Error("service registration failed", zap.Error(err),
					zap.String("service", service), zap.String("addr", addr))
				return nil
			}
			reg = r
			logger.Info("service registered to etcd",
				zap.String("service", service), zap.String("addr", addr),
				zap.Strings("etcd", endpoints), zap.Duration("ttl", ttl))
			return nil
		},
		OnStop: func(_ context.Context) error {
			if reg != nil {
				_ = reg.Close()
				logger.Info("service deregistered from etcd",
					zap.String("service", service), zap.String("addr", addr))
			}
			if client != nil {
				_ = client.Close()
			}
			return nil
		},
	})
}
