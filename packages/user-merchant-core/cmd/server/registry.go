// 服务自注册到 etcd —— user-merchant-core 启动期把
// (service="user-merchant-core", host:grpc_port) 写到 etcd，TTL 10s 心跳续约。
//
// 调用方（card-center / api-gateway / order-core / payment-admin-web BFF）
// 通过 etcd resolver "etcd:///user-merchant-core" 拿到所有活副本 + round_robin LB。
//
// registry.endpoints 留空（dev 单仓 / 本地 unit test）→ 跳过自注册，
// 调用方走 DNS / 直连 fallback；不影响主路径。
package main

import (
	"context"
	"fmt"
	"strings"
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
		endpoints = splitCSVLocal(v.GetString("registry.endpoints"))
	}
	if len(endpoints) == 0 {
		logger.Info("registry.endpoints empty; user-merchant-core skipping etcd self-registration")
		return
	}

	service := v.GetString("registry.service_name")
	if service == "" {
		service = "user-merchant-core"
	}
	port := v.GetInt("server.grpc_port")
	if port == 0 {
		port = 9191
	}
	// 见 card-center registry.go 同处注释：env (K8s downward API) > 探主网卡 IP > viper
	var addr string
	if h := v.GetString("registry.advertise_host"); h != "" {
		addr = fmt.Sprintf("%s:%d", h, port)
	} else {
		addr = serviceregistry.AdvertiseAddr(port)
	}

	ttl := v.GetDuration("registry.ttl")
	if ttl < time.Second {
		ttl = 30 * time.Second // REG-TTL: 10s 太短, 一次 etcd 抖动就过期
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

func splitCSVLocal(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
