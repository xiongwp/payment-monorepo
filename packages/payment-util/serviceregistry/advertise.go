// advertise.go：决定本进程在 etcd 服务发现里广播什么 host:port。
//
// 三层优先级：
//  1. REGISTRY_ADVERTISE_ADDR env —— K8s 标配，downward API 注 status.podIP
//  2. UDP-dial 探主网卡 IP —— docker bridge / k8s pod / 裸机都 work
//  3. os.Hostname() 兜底 —— 极端无网卡场景
package serviceregistry

import (
	"fmt"
	"net"
	"os"
	"strings"
)

const AdvertiseEnvKey = "REGISTRY_ADVERTISE_ADDR"

// AdvertiseAddr 返回 "host:port" 给服务注册到 etcd 用。
// 顺序：env > 主网卡 IP 探测 > os.Hostname()
func AdvertiseAddr(port int) string {
	if v := strings.TrimSpace(os.Getenv(AdvertiseEnvKey)); v != "" {
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
// net.Dial("udp", ...) 不发包，只在内核路由表查"如果发外网用哪个本地 IP"。
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
