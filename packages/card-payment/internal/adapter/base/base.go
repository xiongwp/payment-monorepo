// Package base — 卡组 (Visa / Mastercard / JCB / Amex / UnionPay) adapter 共用基类.
//
// 抽离重复模板 (mTLS init / mock fallback / retry / 错误码归一化),
// 各卡组 adapter 只需提供 :
//   - networkName
//   - 业务字段映射 (DeclineCodeMapper, BuildAuthorizePayload, ParseAuthorizeResponse)
//   - 卡组 base URL + auth header
//
// 重构前: 5 个 adapter 各 1000+ 行,80% 是模板;
// 重构后: 每个 adapter ~250 行,只剩业务映射.

package base

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Config 所有卡组通用配置.
type Config struct {
	Network         string        // visa / mastercard / jcb / amex / unionpay
	BaseURL         string        // 卡组 API base
	ClientCertPath  string        // mTLS client cert
	ClientKeyPath   string        // mTLS client key
	ServerCAPath    string        // 卡组 server CA (固定根证书)
	APIKey          string        // 业务 header 鉴权 (e.g. Authorization: Bearer)
	HTTPTimeout     time.Duration
	MockFallback    bool          // dev/staging 没 mTLS 证书时降级到 mock
	MaxRetries      int           // 网络错误重试次数 (默认 2)
	RetryBaseDelay  time.Duration // 指数退避基线 (默认 100ms)
}

// Network 卡组 adapter 共用接口 (业务方调用).
type Network interface {
	Name() string
	Authorize(ctx context.Context, req *AuthorizeRequest) (*AuthorizeResponse, error)
	Capture(ctx context.Context, req *CaptureRequest) (*OpResponse, error)
	Void(ctx context.Context, req *VoidRequest) (*OpResponse, error)
	Refund(ctx context.Context, req *RefundRequest) (*OpResponse, error)
	Query(ctx context.Context, req *QueryRequest) (*QueryResponse, error)
}

// ─── 业务请求 / 响应 (跨卡组通用形式) ──────────────────────────

type AuthorizeRequest struct {
	PiID           string
	PAN            string  // **唯一含 PAN 的字段**,严格不写日志
	Amount         int64
	Currency       string
	ExpMonth, ExpYear int
	CVV            string
	HolderName     string
	MerchantDesc   string
	IdempotencyKey string
}

type AuthorizeResponse struct {
	NetworkRefNo  string
	Status        string // approved / declined / pending
	DeclineCode   string
	DeclineReason string
	MaskedPAN     string
	Network       string
	ARN           string
}

type CaptureRequest struct {
	NetworkRefNo string
	PiID         string
	Amount       int64
}

type RefundRequest struct {
	NetworkRefNo string
	PiID         string
	Amount       int64
	Reason       string
}

type VoidRequest struct {
	NetworkRefNo string
	PiID         string
	Reason       string
}

type QueryRequest struct {
	NetworkRefNo string
	PiID         string
}

type OpResponse struct {
	NetworkRefNo string
	Status       string
}

type QueryResponse struct {
	NetworkRefNo string
	Status       string
	Amount       int64
	Currency     string
	DeclineCode  string
}

// ─── 模板基类 ────────────────────────────────────────────────

// Adapter base 抽象:每个卡组嵌入 Adapter 并提供 Mapper 即可.
type Adapter struct {
	cfg    Config
	logger *zap.Logger
	mocked bool
	http   *http.Client
}

// DeclineCodeMapper 卡组 raw decline code → 归一化内部 code.
//
// e.g. Visa "05" / Mastercard "DECL" → "do_not_honor"
type DeclineCodeMapper func(rawCode string) (normalized string)

// New 构造.
//
// 若 ClientCertPath 等为空且 MockFallback=true → mock 模式 (dev/staging).
// 其余情况配置不全直接 panic-on-startup 防止生产无声降级。
func New(cfg Config, logger *zap.Logger) (*Adapter, error) {
	if cfg.Network == "" {
		return nil, errors.New("base: Network required")
	}
	if cfg.HTTPTimeout == 0 {
		cfg.HTTPTimeout = 30 * time.Second
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 2
	}
	if cfg.RetryBaseDelay == 0 {
		cfg.RetryBaseDelay = 100 * time.Millisecond
	}
	if cfg.BaseURL == "" || cfg.ClientCertPath == "" {
		if cfg.MockFallback {
			logger.Warn(cfg.Network + " adapter: missing mTLS config, mock mode")
			return &Adapter{cfg: cfg, logger: logger, mocked: true}, nil
		}
		return nil, fmt.Errorf("%s adapter: BaseURL + mTLS cert required", cfg.Network)
	}

	tlsCfg, err := buildTLS(cfg)
	if err != nil {
		return nil, fmt.Errorf("%s tls: %w", cfg.Network, err)
	}
	tr := &http.Transport{
		TLSClientConfig:     tlsCfg,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 50,
		IdleConnTimeout:     90 * time.Second,
	}
	return &Adapter{
		cfg:    cfg,
		logger: logger,
		http:   &http.Client{Transport: tr, Timeout: cfg.HTTPTimeout},
	}, nil
}

// Name impl.
func (a *Adapter) Name() string { return a.cfg.Network }

// Mocked 是否处于 mock 模式 (单测 helper).
func (a *Adapter) Mocked() bool { return a.mocked }

// MockAuthorize 给 mock 模式用,各卡组的 Authorize 调用方在 mocked 时调本函数.
func (a *Adapter) MockAuthorize(req *AuthorizeRequest) *AuthorizeResponse {
	return &AuthorizeResponse{
		NetworkRefNo:  "mock_" + a.cfg.Network + "_" + req.IdempotencyKey,
		Status:        "approved",
		MaskedPAN:     maskPAN(req.PAN),
		Network:       a.cfg.Network,
		ARN:           "ARN_MOCK",
	}
}

// DoRetry 带指数退避的 HTTP 调用. 调用方 build *http.Request,本方法负责发 +
// 在 5xx / 网络错误时按 (attempt, RetryBaseDelay) 退避重试.
func (a *Adapter) DoRetry(ctx context.Context, reqFn func() (*http.Request, error)) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt <= a.cfg.MaxRetries; attempt++ {
		req, err := reqFn()
		if err != nil {
			return nil, err
		}
		req = req.WithContext(ctx)
		resp, err := a.http.Do(req)
		if err == nil && resp.StatusCode < 500 {
			return resp, nil
		}
		if resp != nil {
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("%s %d", a.cfg.Network, resp.StatusCode)
		} else {
			lastErr = err
		}
		if attempt < a.cfg.MaxRetries {
			delay := a.cfg.RetryBaseDelay * (1 << attempt)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}
	}
	return nil, fmt.Errorf("after %d retries: %w", a.cfg.MaxRetries+1, lastErr)
}

// BaseURL exposes cfg.BaseURL for adapter use.
func (a *Adapter) BaseURL() string { return a.cfg.BaseURL }

// APIKey exposes auth token.
func (a *Adapter) APIKey() string { return a.cfg.APIKey }

// Close 关闭 transport (优雅停机).
func (a *Adapter) Close() error {
	if a.http != nil {
		a.http.CloseIdleConnections()
	}
	return nil
}

// ─── helpers ─────────────────────────────────────────────────

func buildTLS(cfg Config) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(cfg.ClientCertPath, cfg.ClientKeyPath)
	if err != nil {
		return nil, fmt.Errorf("client keypair: %w", err)
	}
	out := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	if cfg.ServerCAPath != "" {
		pool := x509.NewCertPool()
		caBytes, err := os.ReadFile(cfg.ServerCAPath)
		if err != nil {
			return nil, fmt.Errorf("server CA: %w", err)
		}
		if !pool.AppendCertsFromPEM(caBytes) {
			return nil, errors.New("server CA PEM parse failed")
		}
		out.RootCAs = pool
	}
	return out, nil
}

// maskPAN 给 mock / log 用.
func maskPAN(pan string) string {
	pan = strings.TrimSpace(pan)
	n := len(pan)
	if n < 4 {
		return "****"
	}
	if n <= 8 {
		return strings.Repeat("*", n-4) + pan[n-4:]
	}
	return pan[:4] + strings.Repeat("*", n-8) + pan[n-4:]
}
