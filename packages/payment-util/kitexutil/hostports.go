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
	"os"
	"strings"

	"github.com/cloudwego/kitex/client"
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
//  1. WithHostPorts(...)  — env / 默认端口表解析出的 host:port
//  2. WithTransportProtocol(transport.GRPC) — 强制 gRPC over HTTP/2 over TCP
//
// 为什么要 (2): Kitex 默认 transport (TTHeader/Framed) 在某些 host:port 字符串下
// 会被 netpoll 解读为 unix socket 路径 (报错 "dial unix accounting-system:50051:
// no such file or directory"). 显式声明 GRPC 强制 TCP+HTTP2, 跟标准 gRPC 互通,
// 也跟服务端 (如果用 server.WithTransportProtocol(transport.GRPC)) 对齐.
//
// 用法 (variadic spread):
//
//	cli, _ := accountingservice.NewClient("accounting-system",
//	    kitexutil.DefaultClientOptions("accounting-system")...,
//	)
//
// 老 caller 用 DefaultHostPorts 单 Option 也可以工作 (默认 transport), 但建议
// 切到 DefaultClientOptions 避免 "dial unix" 那类网络栈混淆.
func DefaultClientOptions(svcName string) []client.Option {
	return []client.Option{
		DefaultHostPorts(svcName),
		client.WithTransportProtocol(transport.GRPC),
	}
}
