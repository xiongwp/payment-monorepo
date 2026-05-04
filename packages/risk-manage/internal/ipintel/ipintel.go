// Package ipintel 提供 IP 信息富化（geo / proxy / VPN / data center / ASN）
// 给风控引擎用。
//
// 接口分两层：
//   - Service：异步本地查询（典型实现：本地 MaxMind GeoLite2 mmdb 文件）
//   - Adapter（未来）：远程外部服务（Sift Science / IPQS / IPInfo）
//
// 当前是 stub：MemService 用 process-local map 配置；生产应注入 mmdb 阅读器
// 或外部 API 客户端。
package ipintel

import (
	"context"
	"net"
	"strings"
	"sync"
)

// Result 一次 IP 查询的结果。所有字段都允许为空 / false（未知 = 不冒充）。
type Result struct {
	Country    string // ISO-2，"PH" / "US"
	ASN        string // "AS15169"
	Proxy      bool   // 已知代理 IP
	VPN        bool   // 商业 VPN 节点
	DataCenter bool   // 数据中心 IP（云厂商 / 主机商）
	// Tor 出口节点。Tor 流量在支付场景视作高风险。
	Tor bool
}

// Service 给定 IP 字符串返回富化信息。实现需要在 ms 级返回（典型 < 5ms）；
// 慢实现应内部带本地缓存。
type Service interface {
	Lookup(ctx context.Context, ip string) Result
}

// MemService 内存版 stub：用预填的 map 模拟 IPIntel。dev / 单测用；生产
// 替换为真实数据源。
type MemService struct {
	mu       sync.RWMutex
	byPrefix map[string]Result // CIDR / 单 IP / 前缀 → Result
}

// NewMemService 构造空 stub。
func NewMemService() *MemService {
	return &MemService{byPrefix: make(map[string]Result)}
}

// Set 注册一条 IP / 前缀 → Result 映射。前缀匹配按 longest-prefix 选 winner。
//
// 示例：
//
//	mem.Set("1.2.3.4", Result{VPN: true})
//	mem.Set("1.2.3.0/24", Result{DataCenter: true, Country: "US"})
func (m *MemService) Set(prefix string, r Result) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byPrefix[strings.TrimSpace(prefix)] = r
}

// Lookup 在已注册前缀里找最长匹配；找不到返回零值（视作未知）。
func (m *MemService) Lookup(_ context.Context, ip string) Result {
	if ip == "" {
		return Result{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	// 简化版：先精确匹配，再走 CIDR 包含。规模 < 几千条够用；生产改 trie。
	if r, ok := m.byPrefix[ip]; ok {
		return r
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return Result{}
	}
	var best Result
	bestBits := -1
	for k, r := range m.byPrefix {
		if !strings.Contains(k, "/") {
			continue
		}
		_, cidr, err := net.ParseCIDR(k)
		if err != nil {
			continue
		}
		if !cidr.Contains(parsed) {
			continue
		}
		bits, _ := cidr.Mask.Size()
		if bits > bestBits {
			best = r
			bestBits = bits
		}
	}
	return best
}
