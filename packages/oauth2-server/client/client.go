// Package client — OAuth2 client_credentials SDK。
//
// 给商户 / 内部服务复用，自动:
//   - 拿 access_token
//   - exp 前 5min 自动 refresh
//   - 单实例并发请求合并 (一个 inflight refresh)
//   - 失败重试 (3 次 / 指数退避)
//
// 用法:
//
//   tc := client.New(client.Config{
//       TokenURL:     "https://oauth.payment.example.com/oauth2/token",
//       ClientID:     "mer_xxx_oauth",
//       ClientSecret: os.Getenv("PAYMENT_CLIENT_SECRET"),
//       Scope:        "charge:write refund:write",
//   })
//
//   req, _ := http.NewRequest("POST", "https://api.payment.example.com/charges", body)
//   if err := tc.AuthorizeRequest(ctx, req); err != nil {
//       return err
//   }
//   resp, err := http.DefaultClient.Do(req)
//
// 标准: RFC 6749 §4.4

package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Config SDK 配置。
type Config struct {
	TokenURL     string        // /oauth2/token
	ClientID     string
	ClientSecret string
	Scope        string        // 可选
	HTTP         *http.Client  // 自定义 client (默认 5s timeout)
	RefreshSkew  time.Duration // 提前 refresh 阈值 (默认 5min)
}

// TokenClient 自动管理 access_token 生命周期。
type TokenClient struct {
	cfg Config

	mu      sync.Mutex
	token   string
	expires time.Time
}

// New 构造。
func New(cfg Config) *TokenClient {
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 5 * time.Second}
	}
	if cfg.RefreshSkew == 0 {
		cfg.RefreshSkew = 5 * time.Minute
	}
	return &TokenClient{cfg: cfg}
}

// Token 拿 access_token (自动 refresh)。
func (c *TokenClient) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Add(c.cfg.RefreshSkew).Before(c.expires) {
		return c.token, nil
	}
	if err := c.fetchLocked(ctx); err != nil {
		return "", err
	}
	return c.token, nil
}

// AuthorizeRequest 给 http.Request 加 Authorization: Bearer ...。
func (c *TokenClient) AuthorizeRequest(ctx context.Context, r *http.Request) error {
	tok, err := c.Token(ctx)
	if err != nil {
		return err
	}
	r.Header.Set("Authorization", "Bearer "+tok)
	return nil
}

// Do 一次性发送 + 自动加 token (用最多/最方便)。
func (c *TokenClient) Do(req *http.Request) (*http.Response, error) {
	if err := c.AuthorizeRequest(req.Context(), req); err != nil {
		return nil, err
	}
	return c.cfg.HTTP.Do(req)
}

func (c *TokenClient) fetchLocked(ctx context.Context) error {
	var lastErr error
	backoff := []time.Duration{0, 500 * time.Millisecond, 1 * time.Second, 2 * time.Second}
	for i := 0; i < 4; i++ {
		if backoff[i] > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff[i]):
			}
		}
		err := c.tryFetch(ctx)
		if err == nil {
			return nil
		}
		lastErr = err
		// 4xx 不重试 (除了 429)
		var oe *OAuthError
		if errors.As(err, &oe) && oe.HTTPStatus < 500 && oe.HTTPStatus != http.StatusTooManyRequests {
			return err
		}
	}
	return fmt.Errorf("oauth2 token fetch failed after retries: %w", lastErr)
}

func (c *TokenClient) tryFetch(ctx context.Context) error {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", c.cfg.ClientID)
	form.Set("client_secret", c.cfg.ClientSecret)
	if c.cfg.Scope != "" {
		form.Set("scope", c.cfg.Scope)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.cfg.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		oe := &OAuthError{HTTPStatus: resp.StatusCode}
		_ = json.Unmarshal(body, oe)
		if oe.Code == "" {
			oe.Code = "http_" + fmt.Sprint(resp.StatusCode)
		}
		return oe
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return fmt.Errorf("parse token response: %w", err)
	}
	c.token = tr.AccessToken
	c.expires = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	return nil
}

// OAuthError RFC 6749 §5.2 错误响应。
type OAuthError struct {
	HTTPStatus int    `json:"-"`
	Code       string `json:"error"`
	Desc       string `json:"error_description"`
}

func (e *OAuthError) Error() string {
	return fmt.Sprintf("oauth2 %d %s: %s", e.HTTPStatus, e.Code, e.Desc)
}
