// advertise.go：决定本进程在 etcd 服务发现里广播什么 host:port。
//
// 三层优先级，确保 dev / 裸机 / K8s 都拿到一个**对端真的能拨过来**的地址：
//
//  1. REGISTRY_ADVERTISE_ADDR 环境变量
//     —— K8s 标配：deployment manifest 通过 downward API 注入 status.podIP：
//        env:
//          - name: REGISTRY_ADVERTISE_ADDR
//            valueFrom: { fieldRef: { fieldPath: status.podIP } }
//     —— 也允许 docker compose 显式覆盖（少数 dev 调试场景）
//     —— 值可以是 "host" 或 "host:port"；后者直接用，前者拼上 port
//
//  2. UDP-dial 探测主网卡 IP
//     —— 给 docker bridge / k8s pod （没设 downward API 时）/ 裸机部署用
//     —— net.Dial("udp", "1.1.1.1:80") 不发包，只走内核路由表查询，返回
//        "如果要连外网，本地用哪张网卡的哪个 IP"——这就是对端能回拨的 IP
//     —— Docker bridge 网络的容器 IP（172.x.x.x）这条路径能拿到，且对同网络
//        其他容器可达；解决了"os.Hostname() 是容器 ID，docker DNS 不解析它"
//        那个 alias 错绑死循环
//
//  3. os.Hostname() 兜底
//     —— 极端情况（无网卡 / 容器 namespace 没分到 IP），起码让进程能起来
//     —— 真到这一步基本注册的 addr 也没人能拨，但至少不 panic
package serviceregistry

import (
	"fmt"
	"net"
	"os"
	"strings"
)

// AdvertiseEnvKey 是约定的 env key，K8s downward API 也用这个名字。
const AdvertiseEnvKey = "REGISTRY_ADVERTISE_ADDR"

// AdvertiseAddr 返回 "host:port" 给服务注册到 etcd 用。
//
// 顺序：env > 主网卡 IP 探测 > os.Hostname()。详见 package doc。
//
// port 是 caller 进程实际监听的 gRPC 端口（不是 k8s service port）；helper
// 不会校验/绑定，只是拼字符串。
func AdvertiseAddr(port int) string {
	if v := strings.TrimSpace(os.Getenv(AdvertiseEnvKey)); v != "" {
		// env 可能是 "host" 或 "host:port"
		if _, _, err := net.SplitHostPort(v); err == nil {
			return v
		}
		return fmt.Sprintf("%s:%d", v, port)
	}
	if ip := primaryNonLoopbackIP(); ip != "" {
		return fmt.Sprintf("%s:%d", ip, port)
	}
	h, _ := os.Hostname()
	if h == "" {
		h = "unknown"
	}
	return fmt.Sprintf("%s:%d", h, port)
}

// primaryNonLoopbackIP 用 UDP-dial trick 拿当前进程主网卡的 IPv4。
//
// net.Dial("udp", ...) 只在内核路由表上做一次"如果要发包到这个目标，本地用
// 哪个 IP"的查询，**不实际发包**——所以哪怕 1.1.1.1 不可达也没关系。返回的
// LocalAddr.IP 就是对端能用来回拨的本地 IP。
//
// IPv6 / 多网卡 / VPN 等场景下可能不准；但对 docker bridge / k8s pod 这两个
// 主流容器化场景这是最稳的方案。失败返回空串，caller fallback。
func primaryNonLoopbackIP() string {
	conn, err := net.Dial("udp", "1.1.1.1:80")
	if err != nil {
		return ""
	}
	defer func() { _ = conn.Close() }()
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return ""
	}
	if addr.IP.IsLoopback() || addr.IP.IsUnspecified() {
		return ""
	}
	return addr.IP.String()
}
