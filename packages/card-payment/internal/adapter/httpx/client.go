// Package httpx 给 5 个 network adapter 共享 HTTPS 客户端 + 鉴权签名 +
// 重试 + 响应解析的通用工具。
//
// 不强加任何 network 特定的 wire 格式：每家卡组织的 Authorize/Refund 等
// payload schema 由各自 adapter 自己定义；本包只管：
//
//   1. 起一个 mTLS / TLS-only 的 *http.Client（带 timeout 和连接池）
//   2. 提供 PostJSON / PostForm helper，调用方传 path + body + headers
//   3. 失败重试策略（5xx + 网络错重试，4xx 直接返）
//   4. 响应 status code → canonical 错误分类
//
// **关键纪律**：
//   - 调用方传进来的 body 里的 PAN 字段，本包**绝不 log**
//   - 仅 log method + url + status + reqID + 响应耗时
//   - 调用方应把 PAN-touching 的 marshalling 局限在 adapter.Authorize 函数 stack
package httpx

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Config 客户端配置。
type Config struct {
	// BaseURL 卡组织 endpoint 根，如 "https://api.visa.com"。生产必须 https://。
	BaseURL string

	// mTLS：三件齐全才启用客户端证书。Visa Net / MIP 都要。
	ClientCertPath string
	ClientKeyPath  string
	ServerCAPath   string

	// 跳过 TLS 验证（**仅** dev sandbox 自签证书；prod assertProdSafety 拒）
	InsecureSkipVerify bool

	// 单次 HTTP 请求 timeout（含拨号 + read + write）。默认 30s。
	Timeout time.Duration

	// 单 RPC 重试次数（含首次）。默认 3。仅 5xx + 网络错重试。
	MaxRetries int

	// 重试 base backoff（指数退避，capped at 5s）。默认 200ms。
	BackoffBase time.Duration

	Logger *zap.Logger
}

// Client mTLS HTTP 客户端 + 重试。
type Client struct {
	baseURL     *url.URL
	hc          *http.Client
	maxRetries  int
	backoffBase time.Duration
	logger      *zap.Logger
}

// New 构造。BaseURL 不解析失败 / mTLS 文件不存在 → 返 error。
func New(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("httpx: base_url required")
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("httpx: base_url parse: %w", err)
	}
	if u.Scheme != "https" && !cfg.InsecureSkipVerify {
		return nil, fmt.Errorf("httpx: base_url must be https:// (got %q)", u.Scheme)
	}

	tlsCfg, err := buildTLS(cfg)
	if err != nil {
		return nil, err
	}
	// 10K TPS 容量：每 network adapter ~3s P50 RT 时在飞 ~3K 连接 / 5 networks ≈ 600
	// per-network。MaxIdleConnsPerHost 拉到 200，MaxIdleConns 总池 1000。Idle timeout
	// 90s 比卡组织 keepalive 长，avoid 频繁握手 / 会话票据失效。
	tr := &http.Transport{
		TLSClientConfig:     tlsCfg,
		MaxIdleConns:        1000,
		MaxIdleConnsPerHost: 200,
		MaxConnsPerHost:     500, // 硬上限防 SYN flood 把对端打垮
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
		// DisableCompression false：gzip body 减半带宽，对 KMS / Detokenize 等
		// 大响应（rich JSON）显著省 NIC
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	maxR := cfg.MaxRetries
	if maxR <= 0 {
		maxR = 3
	}
	bo := cfg.BackoffBase
	if bo <= 0 {
		bo = 200 * time.Millisecond
	}
	return &Client{
		baseURL:     u,
		hc:          &http.Client{Transport: tr, Timeout: timeout},
		maxRetries:  maxR,
		backoffBase: bo,
		logger:      cfg.Logger,
	}, nil
}

// buildTLS 三件齐全 → mTLS；空时 server-auth-only。
func buildTLS(cfg Config) (*tls.Config, error) {
	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: cfg.InsecureSkipVerify, //nolint:gosec // dev only, prod 拒
	}
	if cfg.ClientCertPath != "" && cfg.ClientKeyPath != "" {
		cert, err := tls.LoadX509KeyPair(cfg.ClientCertPath, cfg.ClientKeyPath)
		if err != nil {
			return nil, fmt.Errorf("httpx: client cert load: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	if cfg.ServerCAPath != "" {
		pool := x509.NewCertPool()
		caBytes, err := os.ReadFile(cfg.ServerCAPath)
		if err != nil {
			return nil, fmt.Errorf("httpx: server CA: %w", err)
		}
		if !pool.AppendCertsFromPEM(caBytes) {
			return nil, errors.New("httpx: server CA PEM parse failed")
		}
		tlsCfg.RootCAs = pool
	}
	return tlsCfg, nil
}

// Response 解码后的 HTTP 响应。Body 已经全读到内存，调用方拿 []byte 自己 unmarshal。
type Response struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// IsRetryable 5xx + 408 + 429 视为可重试；其它 4xx 业务错不重试。
func (r *Response) IsRetryable() bool {
	if r == nil {
		return true // 网络错（调用方传 nil 标 retry）
	}
	if r.StatusCode >= 500 {
		return true
	}
	return r.StatusCode == http.StatusRequestTimeout || r.StatusCode == http.StatusTooManyRequests
}

// PostJSON path + headers + JSON body，自动加 Content-Type。重试 5xx / 408 / 429。
//
// path 是相对 BaseURL 的（"/v1/payments"）；绝对 URL 也接受（仅当跨域调试用）。
// 调用方在 headers 里塞 Authorization / 签名 / Idempotency-Key 等。
func (c *Client) PostJSON(ctx context.Context, path string, headers map[string]string, body any) (*Response, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("httpx: marshal body: %w", err)
	}
	return c.do(ctx, http.MethodPost, path, headers, "application/json", buf)
}

// PostForm 银联 / 部分老接口走 application/x-www-form-urlencoded。
func (c *Client) PostForm(ctx context.Context, path string, headers map[string]string, form url.Values) (*Response, error) {
	body := []byte(form.Encode())
	return c.do(ctx, http.MethodPost, path, headers, "application/x-www-form-urlencoded", body)
}

// GetJSON 查询类（Inquiry / Query）走 GET。
func (c *Client) GetJSON(ctx context.Context, path string, headers map[string]string) (*Response, error) {
	return c.do(ctx, http.MethodGet, path, headers, "", nil)
}

// do 实际发送 + 重试。
func (c *Client) do(ctx context.Context, method, path string, headers map[string]string, contentType string, body []byte) (*Response, error) {
	target, err := c.resolveURL(path)
	if err != nil {
		return nil, err
	}
	var lastResp *Response
	var lastErr error
	for attempt := 0; attempt < c.maxRetries; attempt++ {
		if attempt > 0 {
			// 指数退避 + jitter 简化：base * 2^(n-1)，cap 5s
			d := c.backoffBase << (attempt - 1)
			if d > 5*time.Second {
				d = 5 * time.Second
			}
			select {
			case <-ctx.Done():
				return lastResp, ctx.Err()
			case <-time.After(d):
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("httpx: new request: %w", err)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		start := time.Now()
		httpResp, err := c.hc.Do(req)
		if err != nil {
			lastErr = err
			lastResp = nil
			c.log("http error (will retry)",
				zap.String("method", method), zap.String("url", target),
				zap.Int("attempt", attempt+1), zap.Error(err))
			continue
		}
		respBody, _ := io.ReadAll(httpResp.Body)
		_ = httpResp.Body.Close()
		resp := &Response{
			StatusCode: httpResp.StatusCode,
			Header:     httpResp.Header,
			Body:       respBody,
		}
		c.log("http response",
			zap.String("method", method), zap.String("url", target),
			zap.Int("status", resp.StatusCode),
			zap.Duration("dur", time.Since(start)),
			zap.Int("attempt", attempt+1))
		lastResp = resp
		lastErr = nil
		if !resp.IsRetryable() {
			return resp, nil
		}
	}
	if lastErr != nil {
		return nil, fmt.Errorf("httpx: %s %s: %w (after %d attempts)", method, target, lastErr, c.maxRetries)
	}
	return lastResp, nil
}

// resolveURL 拼相对 path 到 BaseURL；绝对 URL 直接用。
func (c *Client) resolveURL(path string) (string, error) {
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path, nil
	}
	u, err := c.baseURL.Parse(path)
	if err != nil {
		return "", fmt.Errorf("httpx: resolve path %q: %w", path, err)
	}
	return u.String(), nil
}

func (c *Client) log(msg string, fields ...zap.Field) {
	if c.logger == nil {
		return
	}
	c.logger.Debug(msg, fields...)
}
