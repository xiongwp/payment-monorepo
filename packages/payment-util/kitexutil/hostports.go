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
func DefaultHostPorts(svcName string) client.Option {
	if envKey := envVarFor(svcName); envKey != "" {
		if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
			return client.WithHostPorts(v)
		}
	}
	if port, ok := defaultPorts[svcName]; ok {
		return client.WithHostPorts(svcName + ":" + port)
	}
	return client.WithHostPorts(svcName + ":80")
}

// envVarFor 把 svc 名转成 ENV 变量 key. "accounting-system" → "ACCOUNTING_SYSTEM_GRPC_ADDR".
func envVarFor(svcName string) string {
	if svcName == "" {
		return ""
	}
	r := strings.NewReplacer("-", "_", ".", "_")
	return strings.ToUpper(r.Replace(svcName)) + "_GRPC_ADDR"
}
