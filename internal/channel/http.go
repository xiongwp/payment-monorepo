package channel

import (
	"net"
	"net/http"
	"time"
)

// sharedTransport 所有 adapter 共用一个 *http.Transport，以便 TCP 连接 /
// TLS handshake / HTTP/2 stream 能在同进程跨 adapter 复用。各 adapter 的
// host 不同，连接池按 host 分桶。
//
// 默认值针对 10 家 adapter 的混合流量调优：
//   - MaxIdleConnsPerHost = 64：每家渠道 64 条 keep-alive 连接够撑 2-3k RPS
//   - MaxConnsPerHost = 256：硬上限，压测极端情况下不让单家渠道把 FD 吃光
//   - IdleConnTimeout = 90s：和多数云厂商/CDN 的 idle kill 窗口匹配
//   - DisableCompression = true：json body 通常很小，gzip 收不回启用成本
var sharedTransport = &http.Transport{
	DialContext: (&net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	ForceAttemptHTTP2:     true,
	MaxIdleConns:          512,
	MaxIdleConnsPerHost:   64,
	MaxConnsPerHost:       256,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   5 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
	DisableCompression:    true,
}

// NewHTTPClient 返回一个共享 transport 的 http.Client。timeout 0 → 不设超时
// （只适合内网调 mock 服务，生产一定传）。
func NewHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: sharedTransport,
		Timeout:   timeout,
	}
}
