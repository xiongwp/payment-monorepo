// Package cardcenterclient Kitex 调 card-center 服务.
//
// 用途: 用户存卡 / 删卡 / 查询某 token 状态时调 card-center.
// 派生支付 token 不在这一侧 (在 order-core PI confirm 时调).
//
// 切 Kitex 后跟 gRPC wire 不互通; server side 已同步切. mTLS 不需要 (内部 mesh).
package cardcenterclient

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cloudwego/kitex/client"

	cardcenterv1 "github.com/xiongwp/card-center/kitex_gen/cardcenter/v1"
	cardcenterservice "github.com/xiongwp/card-center/kitex_gen/cardcenter/v1/cardcenterservice"
)

// Client 调 card-center 的 Kitex 客户端
type Client struct {
	api     cardcenterservice.Client
	timeout time.Duration
}

// Config 客户端配置
type Config struct {
	Endpoint   string
	RPCTimeout time.Duration
}

// **PAN 单跳后 Tokenize 已退役** (task #81): 浏览器 → card-center HTTPS 直连
// → user-merchant-core 只接 stored_token (已 KMS 加密). 本服务进程**完全不
// touch PAN**. 原 TokenizeRequest / TokenizeResponse 类型 + Tokenize 方法已
// 删除, 留下这段注释作为历史足迹防止后人重新加回去.
//
// 如果你看到这里想 "我加个 Tokenize 多方便" —— 不要. 任何写 PAN 字段的代码
// 都会让 user-merchant-core 进 SAQ-D scope, 相当于把 PCI 合规半径扩大三倍.
// PAN 流转走 card-center HTTPS 单跳, 只此一条路径.

// New 建立 Kitex 客户端.
func New(cfg Config) (*Client, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("cardcenterclient: endpoint required")
	}
	t := cfg.RPCTimeout
	if t <= 0 {
		t = 5 * time.Second
	}
	api, err := cardcenterservice.NewClient("card-center",
		client.WithHostPorts(cfg.Endpoint),
		client.WithRPCTimeout(t),
	)
	if err != nil {
		return nil, fmt.Errorf("kitex dial: %w", err)
	}
	return &Client{api: api, timeout: t}, nil
}

// Close — Kitex 自带 connection pool, no-op 兼容老接口.
func (c *Client) Close() error { return nil }

// DeleteCard 调 card-center.DeleteCard (业务层 soft delete).
func (c *Client) DeleteCard(ctx context.Context, userID int64, storedToken, reason, traceID string) error {
	if storedToken == "" {
		return errors.New("stored_token required")
	}
	cctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	_, err := c.api.DeleteCard(cctx, &cardcenterv1.DeleteCardRequest{
		UserId:      userID,
		StoredToken: storedToken,
		Reason:      reason,
		TraceId:     traceID,
	})
	if err != nil {
		return fmt.Errorf("card-center DeleteCard rpc: %w", err)
	}
	return nil
}
