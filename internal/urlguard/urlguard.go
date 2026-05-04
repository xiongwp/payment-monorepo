// Package urlguard 校验客户端传入的回调 URL（return_url / notify_url），防 SSRF。
//
// 攻击场景：商户在 PI.NotifyURL 填入 http://169.254.169.254/latest/meta-data 之类
// 的内网 / 云元数据 IP，后续 webhook 投递逻辑就会从 order-core 的内网身份去
// 访问，把内网响应作为 webhook 内容回写商户记录。
//
// 防御层次（成本由低到高）：
//  1. 解析能力：必须是 http(s) 绝对 URL
//  2. 协议白名单：仅 http / https
//  3. 字面 host 是 IP 字面量时禁止 RFC1918 / loopback / link-local / 多播
//  4. hostname == "localhost" 直接禁
//  5. （生产）egress 防火墙：在 K8s NetworkPolicy 层面禁出口到 RFC1918
//
// 本包做 1-4。第 5 层是基础设施职责，不替代。
package urlguard

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// AllowPrivate 默认 false。仅在 e2e / 本地开发显式置 true 才放行私有 IP。
// 生产强制 false；开发环境用 https://example.com 这类公网 host 测。
var AllowPrivate = false

// ErrInvalidURL 通用语义不合法。具体原因走 errors.Is + sentinel。
var (
	ErrEmptyURL          = errors.New("urlguard: url is empty")
	ErrInvalidScheme     = errors.New("urlguard: scheme must be http or https")
	ErrPrivateAddress    = errors.New("urlguard: host resolves to private / loopback / link-local / metadata address")
	ErrInvalidHost       = errors.New("urlguard: invalid host")
	ErrLocalhostHostname = errors.New("urlguard: localhost-style hostname not allowed")
)

// ValidateOutboundURL 校验商户传入的回调 URL。s == "" 视为合法（字段是 optional）。
// 客户端要传非空字符串则必须通过本检查。
func ValidateOutboundURL(s string) error {
	if s == "" {
		return nil
	}
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("urlguard: parse: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		// ok
	default:
		return ErrInvalidScheme
	}
	host := u.Hostname()
	if host == "" {
		return ErrInvalidHost
	}
	hl := strings.ToLower(host)
	if hl == "localhost" || hl == "ip6-localhost" || hl == "ip6-loopback" {
		if AllowPrivate {
			return nil
		}
		return ErrLocalhostHostname
	}
	// host 可能是 IP 字面量。是 → 检查；不是（hostname）→ 不在此处做 DNS（TOCTOU
	// 风险）；信任 egress firewall + 投递层 dialer 控制。
	if ip := net.ParseIP(host); ip != nil {
		if AllowPrivate {
			return nil
		}
		if isBlockedIP(ip) {
			return ErrPrivateAddress
		}
	}
	return nil
}

// isBlockedIP 判断 IP 是否属于 SSRF 高危段。
//
//	IsLoopback        127.0.0.0/8 + ::1
//	IsPrivate         RFC1918（10/8、172.16/12、192.168/16）+ ULA fc00::/7
//	IsLinkLocalUnicast 169.254/16 + fe80::/10
//	IsMulticast       224/4 + ff00::/8
//	IsUnspecified     0.0.0.0 / ::
//	额外：169.254.169.254 是 AWS / GCP / Azure 元数据服务，IsLinkLocalUnicast 已覆盖
func isBlockedIP(ip net.IP) bool {
	switch {
	case ip.IsLoopback(),
		ip.IsPrivate(),
		ip.IsLinkLocalUnicast(),
		ip.IsLinkLocalMulticast(),
		ip.IsMulticast(),
		ip.IsUnspecified():
		return true
	}
	return false
}
