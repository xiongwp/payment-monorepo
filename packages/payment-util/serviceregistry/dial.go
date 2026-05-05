package serviceregistry

import (
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

// hardenedServiceConfig：round_robin LB + 幂等 RPC 自动重试瞬态错误。
const hardenedServiceConfig = `{
  "loadBalancingConfig":[{"round_robin":{}}],
  "methodConfig":[{
    "name":[{}],
    "retryPolicy":{
      "maxAttempts":3,
      "initialBackoff":"0.1s",
      "maxBackoff":"1s",
      "backoffMultiplier":2,
      "retryableStatusCodes":["UNAVAILABLE","DEADLINE_EXCEEDED"]
    }
  }]
}`

// hardenedKeepalive：10s ping / 3s timeout / PermitWithoutStream
// 配套 hardenedServerKeepalive，少一边就被 GOAWAY ENHANCE_YOUR_CALM 踢。
var hardenedKeepalive = keepalive.ClientParameters{
	Time:                10 * time.Second,
	Timeout:             3 * time.Second,
	PermitWithoutStream: true,
}

var hardenedServerKeepalive = keepalive.EnforcementPolicy{
	MinTime:             5 * time.Second,
	PermitWithoutStream: true,
}

func hardenedOptions() []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithDefaultServiceConfig(hardenedServiceConfig),
		grpc.WithKeepaliveParams(hardenedKeepalive),
	}
}

// HardenedServerOptions 必须挂在 grpc.NewServer(...) 上，跟 hardenedKeepalive 配套。
func HardenedServerOptions() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.KeepaliveEnforcementPolicy(hardenedServerKeepalive),
	}
}

// Dial 通过 etcd resolver "etcd:///<service>" 拨号，含 round_robin + keepalive + retry。
func Dial(service string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	if service == "" {
		return nil, fmt.Errorf("serviceregistry: empty service name")
	}
	target := fmt.Sprintf("%s:///%s", resolverScheme, service)
	fixed := hardenedOptions()
	fixed = append(fixed, opts...)
	return grpc.NewClient(target, fixed...)
}

// DialWithFallback：endpoints 非空 → etcd resolver；空 → 退回 fallbackAddr 静态 DNS。
func DialWithFallback(endpoints []string, service, fallbackAddr string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	if len(endpoints) > 0 {
		if service == "" {
			return nil, fmt.Errorf("serviceregistry: empty service name")
		}
		return DialFromEndpoints(endpoints, service, opts...)
	}
	if fallbackAddr == "" {
		return nil, fmt.Errorf("serviceregistry: empty endpoints and empty fallbackAddr for service %q", service)
	}
	fixed := hardenedOptions()
	fixed = append(fixed, opts...)
	return grpc.NewClient(fallbackAddr, fixed...)
}

// DialDirect：只走静态 endpoint + hardened opts，不需要走 etcd。
// 等价于 DialWithFallback(nil, "", endpoint, opts...)
func DialDirect(endpoint string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("serviceregistry: empty endpoint")
	}
	fixed := hardenedOptions()
	fixed = append(fixed, opts...)
	return grpc.NewClient(endpoint, fixed...)
}
