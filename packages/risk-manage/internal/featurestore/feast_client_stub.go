//go:build !feast

// Package featurestore —— Feast client stub (default build).
//
// 默认 build 不引 Feast SDK / gRPC proto，保证 risk-manage 主仓不被未定型
// 的 ML 依赖污染。需要时用 `go build -tags=feast` 打开。
//
// 真实实现在 feast_client.go（同包，//go:build feast）。
package featurestore

import (
	"context"
	"errors"
	"time"
)

// ErrFeastNotEnabled 表示当前 binary 未编入 Feast client（缺 -tags=feast）。
var ErrFeastNotEnabled = errors.New("featurestore: feast client not enabled (build without -tags=feast)")

// ErrFeastTimeout —— stub 也提供该错误常量，方便上层引用不分 tag。
var ErrFeastTimeout = errors.New("featurestore: feast online get timeout")

// FeastClient stub —— 所有方法返 ErrFeastNotEnabled。
type FeastClient struct {
	FeatureService string
}

func NewFeastClient(addr, project string, timeout time.Duration) (*FeastClient, error) {
	return nil, ErrFeastNotEnabled
}

func (c *FeastClient) GetOnlineFeatures(ctx context.Context, customerID string, featureRefs []string) (map[string]float64, error) {
	return nil, ErrFeastNotEnabled
}

func (c *FeastClient) Close() error { return nil }
