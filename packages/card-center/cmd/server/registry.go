<<<<<<< HEAD
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
=======
// 服务自注册到 etcd —— 调用方走 etcd:///card-center 发现。
>>>>>>> feat/shadow-traffic
package main

import (
	"context"
	"fmt"
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
<<<<<<< HEAD

=======
>>>>>>> feat/shadow-traffic
	service := v.GetString("registry.service_name")
	if service == "" {
		service = "card-center"
	}
	port := v.GetInt("server.grpc_port")
	if port == 0 {
		port = 9443
	}
<<<<<<< HEAD
	// 注册地址三层优先级：
	//   1. viper registry.advertise_host（dev 想显式钉死时用）
	//   2. serviceregistry.AdvertiseAddr —— 走 REGISTRY_ADVERTISE_ADDR env
	//      （K8s downward API status.podIP）或 UDP-dial 探主网卡 IP
	// 之前回退到 os.Hostname() 是错的——docker 不把容器 hostname 注册成 DNS，
	// 跨容器解析不到，会让 client 拿到 etcd 端点后无法 dial。
=======
>>>>>>> feat/shadow-traffic
	var addr string
	if h := v.GetString("registry.advertise_host"); h != "" {
		addr = fmt.Sprintf("%s:%d", h, port)
	} else {
		addr = serviceregistry.AdvertiseAddr(port)
	}
<<<<<<< HEAD

=======
>>>>>>> feat/shadow-traffic
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
<<<<<<< HEAD
				logger.Error("etcd client init failed; skipping self-register",
					zap.Error(err), zap.Strings("endpoints", endpoints))
				return nil
			}
			client = cli

=======
				logger.Error("etcd client init failed", zap.Error(err))
				return nil
			}
			client = cli
>>>>>>> feat/shadow-traffic
			r, err := serviceregistry.NewRegistrar(client, service, addr)
			if err != nil {
				logger.Error("registrar setup failed", zap.Error(err))
				return nil
			}
			regCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if err := r.Register(regCtx, ttl); err != nil {
<<<<<<< HEAD
				logger.Error("service registration failed", zap.Error(err),
					zap.String("service", service), zap.String("addr", addr))
=======
				logger.Error("service registration failed", zap.Error(err))
>>>>>>> feat/shadow-traffic
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
<<<<<<< HEAD
				logger.Info("service deregistered from etcd",
					zap.String("service", service), zap.String("addr", addr))
=======
>>>>>>> feat/shadow-traffic
			}
			if client != nil {
				_ = client.Close()
			}
			return nil
		},
	})
}
