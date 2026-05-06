// 服务自注册到 etcd。config-center 启动期把 (service="config-center", host:http_port)
// 写到 etcd；调用方（业务服务的 SDK）通过 etcd resolver "etcd:///config-center"
// 拿到所有活副本走 round_robin。
//
// registry.endpoints 留空 → 跳过自注册（dev 单仓 / 本地 docker 用直连 hostname）。
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
		endpoints = splitRegCSV(v.GetString("registry.endpoints"))
	}
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
	// 注册地址：advertise_host 优先；否则走 POD_IP / UDP-dial 探主网卡
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

	var (
		cli      *clientv3.Client
		canceler context.CancelFunc
	)
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			c, err := clientv3.New(clientv3.Config{
				Endpoints:   endpoints,
				DialTimeout: 5 * time.Second,
			})
			if err != nil {
				return fmt.Errorf("etcd client: %w", err)
			}
			cli = c
			rctx, cancel := context.WithCancel(context.Background())
			canceler = cancel
			if err := serviceregistry.Register(rctx, c, service, addr, ttl, logger); err != nil {
				cancel()
				_ = c.Close()
				return fmt.Errorf("self-register %s @%s: %w", service, addr, err)
			}
			logger.Info("config-center: registered to etcd",
				zap.String("service", service),
				zap.String("addr", addr),
				zap.Duration("ttl", ttl))
			return nil
		},
		OnStop: func(_ context.Context) error {
			if canceler != nil {
				canceler()
			}
			if cli != nil {
				return cli.Close()
			}
			return nil
		},
	})
}

func splitRegCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
