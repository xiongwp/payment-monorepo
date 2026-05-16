package serviceregistry

import (
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

// hardenedServiceConfig 让 grpc-go：
//  1. 对 resolver 返回的每个端点都建一条子连接，RPC 在子连接间轮询
//     （resolver 推送变更——上线/下线——后自动 rebalance）。
//  2. 对幂等 RPC 自动重试瞬态 Unavailable / DeadlineExceeded —— 副本切换
//     窗口期单条 RPC 不会立即失败给 caller，而是 grpc-go 内部退让重试。
//
// 注意：retryPolicy 默认是 server-streaming/unary 都会重试。如果某接口非幂等
// 且不能重试（如 Encrypt 反复执行会浪费 nonce 但不破数据，OK；但如果接口
// 实际有副作用比如 CreateCharge，就要在 caller 层用 grpc-go 的 method-level
// service config 关掉 retry）——目前 monorepo 内没这种 case，统一开 retry。
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

// hardenedKeepalive 让客户端 10s 没流量就发一个 HTTP/2 ping，3s 收不到回包
// 直接关掉 subconn 触发 reconnect。这是修 "副本被 scale/restart 后 client
// 长期粘 stale IP" 的关键（grpc-go 默认 DNS TTL 30 分钟、pick_first 不主动
// 探活，stale 连接能挂半小时）。
//
// PermitWithoutStream=true 让空闲连接也发 ping —— card-center 这种"绑卡才
// 调一次 KMS"的低频调用必须开，否则 keepalive 完全不生效。
//
// ⚠ 重要：grpc-go server 默认 EnforcementPolicy.MinTime=5min +
// PermitWithoutStream=false——会把这里 10s 一次的 ping 当 abuse 用 GOAWAY
// "ENHANCE_YOUR_CALM / too_many_pings" 踢回来。所以**所有 server 必须挂
// HardenedServerOptions()**（或等价 EnforcementPolicy{MinTime:5s,
// PermitWithoutStream:true}）才能配合本 client。
var hardenedKeepalive = keepalive.ClientParameters{
	Time:                10 * time.Second,
	Timeout:             3 * time.Second,
	PermitWithoutStream: true,
}

// hardenedServerKeepalive 是与 hardenedKeepalive 配套的服务端 EnforcementPolicy。
//
// MinTime=5s：允许客户端最快 5s 发一次 ping（client 实际 10s 一次，留一倍裕度）。
// PermitWithoutStream=true：允许空闲 ping，否则 client 在两次 RPC 之间的 idle
// 期发 ping 会被 server 视作 abuse 直接 GOAWAY。
//
// 不挂这个的 server，碰到挂了 hardenedKeepalive 的 client 会立即 GOAWAY
// "ENHANCE_YOUR_CALM / too_many_pings"，client 反复重连永远建不稳。
var hardenedServerKeepalive = keepalive.EnforcementPolicy{
	MinTime:             5 * time.Second,
	PermitWithoutStream: true,
}

// HardenedServerOptions 返回 monorepo 内所有 gRPC server 推荐的固定 ServerOption。
// 必须 append 到 grpc.NewServer(...) 的 opts 里，才能接受本包 client 的 keepalive ping。
func HardenedServerOptions() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.KeepaliveEnforcementPolicy(hardenedServerKeepalive),
	}
}

// hardenedOptions 返回 monorepo 内所有 grpc client 推荐的固定 dial options。
// 调用方传入的 opts 排在它之后，可以个别覆盖（grpc-go 同类 option 取最后一个）。
func hardenedOptions() []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithDefaultServiceConfig(hardenedServiceConfig),
		grpc.WithKeepaliveParams(hardenedKeepalive),
	}
}

// Dial creates a gRPC ClientConn for the named service via the etcd resolver
// (RegisterResolver must have run first). It always sets round_robin LB so
// traffic spreads across all live replicas, plus tight keepalive so subconn
// staleness is detected within ~13s.
//
// 用户传入的 opts 可以覆盖默认 service config / keepalive（如想自定义 retry
// 或更宽松的 ping 周期）。Caller-provided option wins because 它出现在 fixed 之后。
func Dial(service string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	if service == "" {
		return nil, fmt.Errorf("serviceregistry: empty service name")
	}
	target := fmt.Sprintf("%s:///%s", resolverScheme, service)

	fixed := hardenedOptions()
	fixed = append(fixed, opts...)
	return grpc.NewClient(target, fixed...)
}

// DialWithFallback 是面向"既要支持 etcd resolver 又要支持单仓 dev 直连"的统一入口。
//
//	endpoints 非空 → 走 DialFromEndpoints(etcd resolver + round_robin + keepalive)
//	endpoints 为空 → 退回 grpc.NewClient(fallbackAddr) + 同样的 round_robin + keepalive
//
// 所有 daemon 调用方应该用本 helper 代替裸 grpc.NewClient(addr)，这样 docker
// 联栈模式下（容器删了 container_name 改用 --scale）能通过 etcd 而不是 DNS
// 跨项目寻址；本地 dev / 单仓 docker run 模式（无 etcd）退回直连仍然 work。
//
// fallbackAddr 在 endpoints 非空时被忽略；调用方仍要传以便降级 + 日志可读。
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
	// 直连模式也加 round_robin + keepalive：DNS 解析返回多 A 记录时（k8s
	// headless svc / docker network 多副本同 alias）会均摊；副本被 kill 后
	// 10s 内被 keepalive ping 探测出来踢掉，避免 stale 连接 hang。
	//
	// 关键: target 自动加 dns:/// 前缀。grpc.NewClient("host:port") 默认走
	// passthrough resolver, 它只在拨号时解一次 DNS, 之后永不 re-resolve。
	// 启动顺序错 (admin-web 先起,accounting-system 后起) 或后端容器重启时,
	// passthrough 拿不到 endpoint → balancer 报 "no children to pick from",
	// 后续请求一直 fail 直到进程重启。
	// 加 dns:/// 触发 gRPC 内置 DNS resolver, idle 连接重建时会重新解析。
	// SP-AC-7: dns:/// → passthrough:///
	// dns 在 docker compose 网络下经常返 "no children to pick from", 整套挂.
	// passthrough 用系统层 DNS, 行为更稳; 代价: peer 重启换 IP 后 caller 需自己重启.
	// 生产应该用 etcd resolver (DialFromEndpoints) 真服务发现.
	target := fallbackAddr
	if !strings.Contains(target, "://") {
		target = "passthrough:///" + target
	}
	fixed := hardenedOptions()
	fixed = append(fixed, opts...)
	return grpc.NewClient(target, fixed...)
}

// DialDirect 是一条只走"静态 endpoint + hardened opts"的捷径，不需要走 etcd。
// 适用于：endpoint 已经是 docker DNS / k8s headless svc 名字，调用方只想要
// 自动 keepalive + round_robin + retry 这一套防 stale 的兜底配置。
//
// 等价于 DialWithFallback(nil, "", endpoint, opts...)。
func DialDirect(endpoint string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("serviceregistry: empty endpoint")
	}
	fixed := hardenedOptions()
	fixed = append(fixed, opts...)
	return grpc.NewClient(endpoint, fixed...)
}

// MTLSDialOptions 返回用于 mTLS 的标准 gRPC dial options。
// 调用方应该添加 TransportCredentials 和任何服务特定的 options。
//
// 例如：
//
//	creds, err := mtls.Config{...}.ClientCredentials()
//	if err != nil {
//		log.Fatalf("creds: %v", err)
//	}
//	opts := serviceregistry.MTLSDialOptions()
//	opts = append(opts, grpc.WithTransportCredentials(creds))
//	conn, err := grpc.NewClient(target, opts...)
func MTLSDialOptions() []grpc.DialOption {
	return hardenedOptions()
}
