// server.go — 唯一的 Kitex server 端 etcd 自注册入口.
//
// REG-REDESIGN: 此包替代 payment-util/serviceregistry/Registrar (已删) +
// 各服务 cmd/server/registry.go::startServiceRegistrar (已删). 全 monorepo
// **单一注册路径**, 不再有双写覆盖 / addr 格式漂移 / 静默吞错的问题.
//
// 设计原则:
//   1. **Single Source of Truth** — 整个进程只通过 kitexutil.DefaultServerOptions
//      写一次 etcd. 别处不许再写, 也不许另起 Registrar.
//   2. **Fail-fast** — REGISTRY_ENDPOINTS 配了但 etcd 不通 → panic. ops 立刻知道
//      服务发现是不通的, 不会出现"进程在跑但调用方拿不到端点"的诡异状态.
//      REGISTRY_ENDPOINTS 没配 → log warn 但允许起 (单机 dev 场景).
//   3. **Addr 严格校验** — advertiseAddr 必须解析得出可路由 host:port. 拒绝:
//      - 空字符串
//      - 12-char docker container hostname (类似 "eaba3249adac")
//      - 端口为 0
//      错的话 panic, ops 立刻看到. 不允许"看似注册成功但 caller 拨不通"的状态.
//   4. **Loud logging** — 注册成功 / 重新 grant / Deregister 都打 stderr.
//
// 用法 (server main.go 一行):
//
//	srv := kitexserver.NewServer(append(
//	    []kitexserver.Option{kitexserver.WithServiceAddr(addr)},
//	    kitexutil.DefaultServerOptions("accounting-service", "accounting-service:50051")...,
//	)...)
package kitexutil

import (
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/cloudwego/kitex/pkg/registry"
	kitexserver "github.com/cloudwego/kitex/server"
)

// DefaultServerOptions 返一组 kitexserver.Option, 装入 Kitex server 后启动期会
// 自动把本进程注册到 etcd. Kitex Stop() 时自动 Deregister.
//
//   - svcName: 必填. 注册到 etcd 的服务名. caller 用 NewClient(svcName, ...) 解析.
//     一般等于 docker DNS 名 / k8s service 名, 不一定是 Go 包名.
//   - advertiseAddr: 必填. 注册广播的 host:port. caller 容器拿到这个值后 dial.
//     dev: "<docker-dns>:<port>" (e.g. "user-merchant-core:9191")
//     prod: "<pod-ip>:<port>" (通过 K8s downward API 注入)
//
// 行为:
//   - REGISTRY_ENDPOINTS env 空 → log 警告并返 nil. server 仍能起, 但 caller 拿不到
//     端点 (dev 单机调试模式).
//   - REGISTRY_ENDPOINTS 非空但 etcd 拨号失败 → **panic**. 这个状态部署 / ops 必须
//     立刻知道, 不能让进程跑起来假装正常.
//   - advertiseAddr 格式不合法 → **panic**.
func DefaultServerOptions(svcName, advertiseAddr string) []kitexserver.Option {
	eps := RegistryEndpointsFromEnv()
	if len(eps) == 0 {
		fmt.Fprintf(os.Stderr,
			"[kitexutil] svc=%s REGISTRY_ENDPOINTS empty — 跳过 etcd 注册 (dev 单机模式; "+
				"prod 必须在 docker-compose / k8s manifest 里设 REGISTRY_ENDPOINTS=etcd:2379)\n",
			svcName)
		return nil
	}

	// **Fail-fast**: addr 校验.
	if err := validateAdvertiseAddr(advertiseAddr); err != nil {
		panic(fmt.Sprintf("[kitexutil] svc=%s advertiseAddr 不合法 (%v); 请在 main.go 显式传 "+
			"\"%s:<port>\" (docker DNS) 或设 env ADVERTISE_HOST + SERVER_PORT", svcName, err, svcName))
	}

	// **Fail-fast**: etcd 拨号.
	cli, err := NewEtcdClient(eps, 5*time.Second)
	if err != nil {
		panic(fmt.Sprintf("[kitexutil] svc=%s 拨 etcd %v 失败: %v (REGISTRY_ENDPOINTS 配错 / "+
			"etcd 容器未启 / 网络不通)", svcName, eps, err))
	}

	reg, err := NewEtcdRegistry(cli, svcName, advertiseAddr, 30*time.Second)
	if err != nil {
		_ = cli.Close()
		panic(fmt.Sprintf("[kitexutil] svc=%s NewEtcdRegistry 失败: %v", svcName, err))
	}

	fmt.Fprintf(os.Stderr, "[kitexutil] svc=%s etcd 注册 armed addr=%s ttl=30s endpoints=%v\n",
		svcName, advertiseAddr, eps)

	info := &registry.Info{
		ServiceName: svcName,
		Addr:        plainAddr{addr: advertiseAddr},
	}
	return []kitexserver.Option{
		kitexserver.WithRegistry(reg),
		kitexserver.WithRegistryInfo(info),
	}
}

// validateAdvertiseAddr 校验 advertiseAddr 格式. 拒绝:
//   - 空字符串
//   - 12-char container hostname (docker 默认 hostname, caller 容器解析不到)
//   - port 不是 1-65535
//
// 合法的例子:
//   - "user-merchant-core:9191"   ← docker DNS 名 (推荐 dev)
//   - "10.244.1.5:9191"           ← pod IP (推荐 prod)
//   - "[::1]:9191"                ← IPv6 (允许但不推荐 — caller 容器可能不支持)
func validateAdvertiseAddr(addr string) error {
	if addr == "" {
		return fmt.Errorf("空字符串")
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("SplitHostPort: %w", err)
	}
	if host == "" {
		return fmt.Errorf("host 部分为空")
	}
	if dockerContainerHostnameRe.MatchString(host) {
		return fmt.Errorf("看起来是 docker container hostname (%q) — caller 容器解析不到; "+
			"请用 docker compose service 名 (如 \"user-merchant-core\") 或 POD_IP", host)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("port 不是数字: %w", err)
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("port=%d 越界", port)
	}
	return nil
}

// dockerContainerHostnameRe 匹配 12-char 十六进制 (docker 默认 hostname 格式).
// 例: "eaba3249adac". 真实 service 名一般有 - 或 letter, 不会全 hex.
var dockerContainerHostnameRe = regexp.MustCompile(`^[0-9a-f]{12}$`)

// plainAddr 给 registry.Info.Addr 用的最简 net.Addr 包装. Kitex 内部 .String()
// 即可取出 host:port; 不需要真的拨号能力.
type plainAddr struct{ addr string }

func (p plainAddr) Network() string { return "tcp" }
func (p plainAddr) String() string  { return p.addr }
