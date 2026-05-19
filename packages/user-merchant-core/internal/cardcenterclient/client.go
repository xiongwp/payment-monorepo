// Package cardcenterclient — 临时 STUB.
//
// 原版通过 Kitex 调 card-center.DeleteCard. cross-service kitex_gen 还没接进
// docker build 流程 (additional_contexts + replace), 暂改 stub: DeleteCard 返
// ErrNotWired, 调用方走降级 (admin UI 删卡按钮临时不可用).
package cardcenterclient

import (
	"context"
	"errors"
	"time"
)

// Client stub.
type Client struct{ timeout time.Duration }

// Config 保持原 signature.
type Config struct {
	Endpoint   string
	RPCTimeout time.Duration
}

// ErrNotWired stub 返此 sentinel.
var ErrNotWired = errors.New("cardcenterclient STUB: card-center kitex_gen not wired in build")

// New 构造 stub client.
func New(cfg Config) (*Client, error) {
	t := cfg.RPCTimeout
	if t <= 0 {
		t = 5 * time.Second
	}
	return &Client{timeout: t}, nil
}

// Close no-op.
func (c *Client) Close() error { return nil }

// DeleteCard stub: 永远返 ErrNotWired.
func (c *Client) DeleteCard(_ context.Context, _ int64, storedToken, _, _ string) error {
	if storedToken == "" {
		return errors.New("stored_token required")
	}
	return ErrNotWired
}
