// Service self-registration to etcd + leader election helper for cron workers.
//
// 服务发现：order-core 启动期把 (service, host:grpc_port) 写到 etcd，调用方
// （payment-core / payment-admin-web BFF / api-gateway）通过 etcd resolver
// "etcd:///order-core" 拿到所有活副本 + round_robin LB。
//
// Leader election：order-core 内置多个 cron worker（ExpireWorker /
// ReconcileWorker / AccountingOutboxWorker / WebhookRetryWorker / ...）。
// 多 pod 部署时如果每个 pod 都跑一份会重复扫表 / 重复回调 / 重复入账。
// startServiceRegistrar 同时构造一个 etcd 客户端通过 fx.Provide 暴露，
// 各 startXxxWorker 用 serviceregistry.RunLeaderLoop 包一层 → 同一时刻
// 全集群只有一个副本在跑该 worker，leader 挂了下一副本秒级接管。
//
// registry.endpoints 留空时：
//   - 跳过自注册（调用方走 DNS / 直连 fallback）
//   - etcd 客户端置 nil，RunLeaderLoop 退化为直接 task()（单 pod / dev 模式）
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

// LeaderToolkit 由 fx 注入到每个 worker start 函数；nil 客户端代表禁用 election。
type LeaderToolkit struct {
	Client   *clientv3.Client // 可能为 nil（dev / 未配 etcd）
	Identity string           // 当前 pod 标识（os.Hostname）
	TTL      time.Duration    // election session TTL
}

// newLeaderToolkit 从 config 创建 etcd 客户端 + identity；
// registry.endpoints 空 → 返回 zero value（Client nil）。
func newLeaderToolkit(v *viper.Viper, logger *zap.Logger) *LeaderToolkit {
	t := &LeaderToolkit{
		Identity: hostnameOrUnknown(),
		TTL:      ttlOrDefault(v, 30*time.Second), // REG-TTL: 10s 太短, 一次抖动就过期; 30s 更稳
	}
	endpoints := v.GetStringSlice("registry.endpoints")
	if len(endpoints) == 0 {
		logger.Info("registry.endpoints empty; cron workers run in single-instance mode (no leader election)")
		return t
	}
	cli, err := serviceregistry.NewEtcdClient(endpoints)
	if err != nil {
		logger.Warn("etcd client init failed; cron workers run in single-instance mode", zap.Error(err))
		return t
	}
	t.Client = cli
	return t
}

// RunWorker spins up a goroutine that holds leadership for `key` and runs `task`
// while leader. Returns a cancel func to stop the loop on shutdown.
//
//	  ctx, cancel := tk.RunWorker(parent, "expire-worker", w.Start)
//	  defer cancel()
func (t *LeaderToolkit) RunWorker(ctx context.Context, name string, task func(ctx context.Context)) {
	if t == nil {
		task(ctx)
		return
	}
	key := fmt.Sprintf("/leader/order-core/%s", name)
	go serviceregistry.RunLeaderLoop(ctx, t.Client, key, t.Identity, t.TTL, task)
}

// startServiceRegistrar — REG-REDESIGN no-op.
// 见 packages/payment-core/cmd/server/registry.go 的注释; 注册走 kitexutil.
// LeaderToolkit (cron worker 领导选举) 跟注册无关, 保留.
func startServiceRegistrar(_ fx.Lifecycle, _ *LeaderToolkit, _ *viper.Viper, logger *zap.Logger) {
	logger.Debug("startServiceRegistrar: no-op (REG-REDESIGN — 注册走 kitexutil.DefaultServerOptions)")
}

func hostnameOrUnknown() string {
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return "unknown"
}

func ttlOrDefault(v *viper.Viper, def time.Duration) time.Duration {
	if d := v.GetDuration("registry.ttl"); d >= time.Second {
		return d
	}
	return def
}
