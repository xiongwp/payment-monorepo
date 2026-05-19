// Package grpc — loadshed: 老 gRPC UnaryServerInterceptor 全删 (Kitex 切换).
// 留下 LoadShedConfig + loadShedder 数据结构 (Server 持有 + main.go 配置),
// 等 kitexutil.LoadShedMW 接通后从这个 struct 读 maxInflight + token bucket.
package grpc

import (
	"context"
	"sync/atomic"
	"time"
)

// LoadShedConfig 负载保护配置, main.go ListenAndServe 时传入.
type LoadShedConfig struct {
	// MaxInflight 同时处理的 RPC 上限; 0 = 不限.
	MaxInflight int64
	// RatePerSecond 令牌桶速率 (RPC/s); 0 = 不限速.
	RatePerSecond int
}

// loadShedder 双闸门: inflight 信号量 + 令牌桶.
// 当前老 gRPC interceptor 已删, 留 struct 给 Kitex MW 接好后复用.
type loadShedder struct {
	maxInflight atomic.Int64
	inflight    atomic.Int64
	tokenBucket chan struct{}
}

func newLoadShedder(cfg LoadShedConfig) *loadShedder {
	ls := &loadShedder{}
	ls.maxInflight.Store(cfg.MaxInflight)
	if cfg.RatePerSecond > 0 {
		ls.tokenBucket = make(chan struct{}, cfg.RatePerSecond)
	}
	return ls
}

// SetMaxInflight 热更新 (config-center OnChange 调).
func (ls *loadShedder) SetMaxInflight(n int64) { ls.maxInflight.Store(n) }

// MaxInflight 当前阈值.
func (ls *loadShedder) MaxInflight() int64 { return ls.maxInflight.Load() }

// Inflight 当前正在处理的 RPC 数.
func (ls *loadShedder) Inflight() int64 { return ls.inflight.Load() }

// startRefillLoop 后台 goroutine 给令牌桶定速补充. ctx 关闭即停.
// Kitex MW 接通后 MW 调 Allow() 时从 tokenBucket 取 token.
func (ls *loadShedder) startRefillLoop(ctx context.Context, ratePerSecond int) {
	if ls.tokenBucket == nil || ratePerSecond <= 0 {
		return
	}
	interval := time.Second / time.Duration(ratePerSecond)
	if interval <= 0 {
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			select {
			case ls.tokenBucket <- struct{}{}:
			default:
				// bucket full, drop
			}
		}
	}
}
