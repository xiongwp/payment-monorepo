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

// NewEtcdClient creates an etcd client used for service registry + leader
// election. Returns (nil, nil) when registry.endpoints is empty so that
// dev environments without etcd still boot.
func NewEtcdClient(v *viper.Viper, logger *zap.Logger) (*clientv3.Client, error) {
	endpoints := v.GetStringSlice("registry.endpoints")
	if len(endpoints) == 0 {
		logger.Info("registry.endpoints empty; service self-registration disabled")
		return nil, nil
	}
	cli, err := serviceregistry.NewEtcdClient(endpoints)
	if err != nil {
		return nil, fmt.Errorf("etcd client: %w", err)
	}
	logger.Info("etcd client connected", zap.Strings("endpoints", endpoints))
	return cli, nil
}

// StartServiceRegistrar publishes (service_name, host:grpc_port) into etcd
// with a TTL lease + heartbeat. On graceful shutdown the lease is revoked
// so other callers stop dialing this instance immediately; on crash the
// lease expires within TTL and etcd cleans it up.
//
// The advertised addr defaults to os.Hostname() — works inside docker-compose
// (sibling DNS) and K8s (pod headless service). Override via REGISTRY_ADVERTISE_ADDR
// if you need a routable IP from outside the cluster.
func StartServiceRegistrar(lc fx.Lifecycle, cli *clientv3.Client, v *viper.Viper, logger *zap.Logger) {
	if cli == nil {
		return
	}
	service := v.GetString("registry.service_name")
	if service == "" {
		service = "accounting-service"
	}

	port := v.GetInt("server.grpc_port")
	if port == 0 {
		port = 50051
	}

<<<<<<< HEAD
	// 注册地址：viper override > serviceregistry.AdvertiseAddr (env / 探主网卡 IP / hostname 兜底)
	// 之前直接 os.Hostname() 会拿到容器 ID，docker DNS 不解析它，client 拿到端点后无法 dial。
	var addr string
=======
var addr string
>>>>>>> feat/shadow-traffic
	if h := v.GetString("registry.advertise_host"); h != "" {
		addr = fmt.Sprintf("%s:%d", h, port)
	} else {
		addr = serviceregistry.AdvertiseAddr(port)
	}

	ttl := v.GetDuration("registry.ttl")
	if ttl < time.Second {
		ttl = 10 * time.Second
	}

	reg, err := serviceregistry.NewRegistrar(cli, service, addr)
	if err != nil {
		logger.Error("registrar setup failed; continuing without registration",
			zap.Error(err), zap.String("service", service), zap.String("addr", addr))
		return
	}

	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			regCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if err := reg.Register(regCtx, ttl); err != nil {
				logger.Error("service registration failed",
					zap.Error(err), zap.String("service", service), zap.String("addr", addr))
				return nil // 不阻断启动；调用方走 DNS fallback
			}
			logger.Info("service registered to etcd",
				zap.String("service", service), zap.String("addr", addr), zap.Duration("ttl", ttl))
			return nil
		},
		OnStop: func(_ context.Context) error {
			if err := reg.Close(); err != nil {
				logger.Warn("service deregister failed", zap.Error(err))
			} else {
				logger.Info("service deregistered from etcd",
					zap.String("service", service), zap.String("addr", addr))
			}
			return nil
		},
	})
}
