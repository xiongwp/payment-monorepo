// hostports.go — Kitex client 拨号 host:port 统一 helper.
//
// 解决问题: Kitex client 不接 etcd resolver + 不传 client.WithHostPorts → 报
// "no resolver available". 全 monorepo 把 etcd resolver 真接通前, 所有 client
// 调 NewClient 必须显式给 host:port.
//
// 调用方式:
//
//	cli, _ := accountingservice.NewClient("accounting-system",
//	    kitexutil.DefaultHostPorts("accounting-system"),
//	)
//
// 解析顺序:
//   1. env var <SVC>_GRPC_ADDR (svc 大写, '-' / '.' → '_'). 例:
//      ACCOUNTING_SYSTEM_GRPC_ADDR=10.0.0.5:50051
//   2. 内置 docker 容器 DNS 默认 (svc-name:default-port).
//
// 任何新服务接入时只需补 defaultPorts 表即可.
package kitexutil

import (
	"context"
	"os"
	"strings"

	"github.com/cloudwego/kitex/client"
	"github.com/cloudwego/kitex/pkg/discovery"
	"github.com/cloudwego/kitex/pkg/rpcinfo"
	"github.com/cloudwego/kitex/transport"
)

// defaultPorts 各服务在 docker-compose 内部网络的 gRPC 端口默认值.
// 新增服务时加一行即可.
var defaultPorts = map[string]string{
	"accounting-system":  "50051",
	"user-merchant-core": "9191",
	"order-core":         "9091",
	"payment-core":       "9091",
	"payment-channel":    "9091",
	"card-payment":       "9091",
	"card-center":        "9443",
	"kms-manage":         "9290",
	"risk-manage":        "9090",
	"split-payment":      "9098",
	"config-center":      "9092",
	"id-generator":       "9093",
}

// DefaultHostPorts 给 Kitex client 装上一个 host:port 解析:
//
//   - 先看 env var <SVC>_GRPC_ADDR (大写, '-' 转 '_')
//   - 再回退到内置 docker 容器 DNS 默认 (服务名 + 端口表)
//   - 都没命中时 → 走 "<svcName>:80" (一般跑不通, 但至少不会 "no resolver available" panic)
//
// 用法: kitexutil.DefaultHostPorts("accounting-system")
//
// 注意: 单 Option 只设 host:port, 不设 transport. 想避开 "dial unix host:port"
// 那类网络栈混淆, 用 DefaultClientOptions(...)... (variadic spread) 把
// transport.GRPC 一起带上.
func DefaultHostPorts(svcName string) client.Option {
	return client.WithHostPorts(resolveHostPort(svcName))
}

// resolveHostPort 把 svcName → host:port 字符串. DefaultHostPorts /
// DefaultClientOptions 共用.
func resolveHostPort(svcName string) string {
	if envKey := envVarFor(svcName); envKey != "" {
		if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
			return v
		}
	}
	if port, ok := defaultPorts[svcName]; ok {
		return svcName + ":" + port
	}
	return svcName + ":80"
}

// envVarFor 把 svc 名转成 ENV 变量 key. "accounting-system" → "ACCOUNTING_SYSTEM_GRPC_ADDR".
func envVarFor(svcName string) string {
	if svcName == "" {
		return ""
	}
	r := strings.NewReplacer("-", "_", ".", "_")
	return strings.ToUpper(r.Replace(svcName)) + "_GRPC_ADDR"
}

// DefaultClientOptions 返回 Kitex client 推荐的拨号 Options 组合:
//
//  1. WithResolver(tcpStaticResolver) — 把 host:port 包成显式 network="tcp"
//     的 discovery.Instance, 绕过 Kitex 默认 WithHostPorts 在 v0.16.x 下
//     生成的 instance.Network()=="" 缺陷 (会被 netpoll 解读成 unix socket,
//     报 "dial unix accounting-system:50051: no such file or directory").
//  2. WithTransportProtocol(transport.GRPC) — 强制 gRPC over HTTP/2 over TCP.
//
// 用法 (variadic spread):
//
//	cli, _ := accountingservice.NewClient("accounting-system",
//	    kitexutil.DefaultClientOptions("accounting-system")...,
//	)
func DefaultClientOptions(svcName string) []client.Option {
	addr := resolveHostPort(svcName)
	return []client.Option{
		client.WithResolver(newTCPStaticResolver(addr)),
		client.WithTransportProtocol(transport.GRPC),
	}
}

// tcpStaticResolver 静态 resolver, 每次 Resolve 都返同一个 instance, 强制
// Network()="tcp". 是 Kitex WithHostPorts 的安全替代.
type tcpStaticResolver struct {
	addr     string
	instance discovery.Instance
}

func newTCPStaticResolver(addr string) discovery.Resolver {
	return &tcpStaticResolver{
		addr:     addr,
		instance: discovery.NewInstance("tcp", addr, 10, nil),
	}
}

func (r *tcpStaticResolver) Target(_ context.Context, _ rpcinfo.EndpointInfo) string {
	return r.addr
}

func (r *tcpStaticResolver) Resolve(_ context.Context, _ string) (discovery.Result, error) {
	return discovery.Result{
		Cacheable: true,
		CacheKey:  r.addr,
		Instances: []discovery.Instance{r.instance},
	}, nil
}

func (r *tcpStaticResolver) Diff(cacheKey string, prev, next discovery.Result) (discovery.Change, bool) {
	// 静态 resolver, 不变化.
	return discovery.Change{}, false
}

func (r *tcpStaticResolver) Name() string { return "kitexutil-tcp-static" }
