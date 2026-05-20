// server.go — Kitex server 端 etcd 自注册 helper.
//
// 跟 client 端 DefaultClientOptions 对应; server 端 main.go 一行接通:
//
//	srv := kitexserver.NewServer(append(
//	    []kitexserver.Option{kitexserver.WithServiceAddr(addr)},
//	    kitexutil.DefaultServerOptions("accounting-service", advertiseAddr)...,
//	)...)
//
// 行为:
//   - REGISTRY_ENDPOINTS env 非空 → server.WithRegistry(EtcdRegistry) +
//     WithRegistryInfo(ServiceName + Addr); Kitex 启动期自动 Register, Stop() 时 Deregister.
//   - REGISTRY_ENDPOINTS env 空    → 返空 slice; server 仍能起, 只是不注册到 etcd
//     (caller 自己决定要不要 fail-fast).
package kitexutil

import (
	"fmt"
	"net"
	"os"
	"time"

	kitexserver "github.com/cloudwego/kitex/server"
	"github.com/cloudwego/kitex/pkg/registry"
)

// DefaultServerOptions 给 kitexserver.NewServer 返一组 etcd 自注册 options.
//
//   - svcName: 注册到 etcd 的服务名. 这个值就是 caller 端 EtcdResolver 解析时用的
//     key. dev 一般等于 docker DNS 名 (e.g. "accounting-service"), prod 等于
//     k8s Service 名 / Consul service name.
//   - advertiseAddr: 注册广播的 host:port. 必须从 caller 容器 / pod 可路由.
//     dev 用 "<container-name>:<port>" (docker DNS); prod 用 POD_IP:port.
//     传空字符串则用 hostname:port 兜底 (server.WithRegistryInfo 也会填一份).
//
// 没配 REGISTRY_ENDPOINTS env → 返 nil slice, kitexserver.NewServer 不做注册.
func DefaultServerOptions(svcName, advertiseAddr string) []kitexserver.Option {
	eps := RegistryEndpointsFromEnv()
	if len(eps) == 0 {
		// REG-TTL: 把"没注册"明确打出来, 别让 ops 误以为已注册.
		fmt.Fprintf(os.Stderr, "[kitexutil] svc=%s REGISTRY_ENDPOINTS empty, etcd registration disabled (caller will fail to discover)\n", svcName)
		return nil
	}
	cli, err := NewEtcdClient(eps, 5*time.Second)
	if err != nil {
		// REG-TTL: etcd 不通时 loud log; 进程仍起来 (静态 DNS 兜底), 但 ops 必须知道.
		fmt.Fprintf(os.Stderr, "[kitexutil] svc=%s etcd dial failed: %v — skipping registration\n", svcName, err)
		return nil
	}
	if advertiseAddr == "" {
		advertiseAddr = guessAdvertiseAddr()
	}
	reg, err := NewEtcdRegistry(cli, svcName, advertiseAddr, 30*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[kitexutil] svc=%s NewEtcdRegistry failed: %v — skipping registration\n", svcName, err)
		_ = cli.Close()
		return nil
	}
	fmt.Fprintf(os.Stderr, "[kitexutil] svc=%s etcd registration armed (addr=%s, ttl=30s, endpoints=%v)\n", svcName, advertiseAddr, eps)
	info := &registry.Info{
		ServiceName: svcName,
		Addr:        plainAddr{addr: advertiseAddr},
	}
	return []kitexserver.Option{
		kitexserver.WithRegistry(reg),
		kitexserver.WithRegistryInfo(info),
	}
}

// guessAdvertiseAddr 在 main.go 没显式传 advertiseAddr 时兜底.
//
//   - 优先读 env ADVERTISE_HOST (业务方在 docker-compose / k8s manifest 里设)
//   - 否则用 os.Hostname() (docker 容器内 = container-name, k8s pod = pod 名)
//   - port 用 env SERVER_PORT, 默认 ""
func guessAdvertiseAddr() string {
	host := os.Getenv("ADVERTISE_HOST")
	if host == "" {
		host, _ = os.Hostname()
	}
	port := os.Getenv("SERVER_PORT")
	if host == "" || port == "" {
		return ""
	}
	return net.JoinHostPort(host, port)
}

// plainAddr 给 registry.Info.Addr 用的最简 net.Addr 包装. Kitex 内部 .String()
// 即可取出 host:port; 不需要真的拨号能力.
type plainAddr struct{ addr string }

func (p plainAddr) Network() string { return "tcp" }
func (p plainAddr) String() string  { return p.addr }
