// Package mloverride 运营手动 ML 降级 / 强制分数控制。
//
// 出问题时常见场景：
//   1. ML 服务推理质量突降 (drift / data poisoning) → 想立刻禁用 ML 不重启
//   2. 怀疑 ML 模型错把大批合法交易判 fraud → 想强制 score=0 走规则
//   3. 测新阈值 / 调试 ml_threshold 规则 → 临时强制 score=0.99 看下游链路
//
// 现有 mlBreaker 是熔断器（自动 OnFailure），不能由运营主动控制。本包补
// 一个 admin 可手动开关的 override：
//   - Disabled: true → service.Screen 跳过整个 ML 推理路径，MLScore 设
//     ForceScore 或 0
//   - ForceScore: 设了非 0 → 即使 ML 跑成功也用这个值覆盖（debug 用）
//
// 写时落 audit (actor / reason)；GET 端点让 dashboard 显示当前状态。
package mloverride

import (
	"sync/atomic"
	"time"
)

// Override 当前运营覆盖状态。零值 = "正常运行" (disabled=false / force_score=0)。
type Override struct {
	Disabled   bool      `json:"disabled"`
	ForceScore float64   `json:"force_score"` // 0 = 不强制；其它值会替换 ML 输出
	Reason     string    `json:"reason"`
	SetAt      time.Time `json:"set_at,omitempty"`
	SetBy      string    `json:"set_by,omitempty"`
}

// Store 线程安全的状态存储。Get 走 atomic.Pointer 让 hot path 无锁。
//
// 用法：
//
//	store := mloverride.New()
//	if cur := store.Get(); cur.Disabled {
//	    txn.MLScore = cur.ForceScore
//	    return  // 跳过 ML 推理
//	}
//
//	// admin 端点
//	store.Set(Override{Disabled: true, Reason: "drift_alert", SetBy: "alice"})
type Store struct {
	cur atomic.Pointer[Override]
}

func New() *Store {
	s := &Store{}
	s.cur.Store(&Override{})
	return s
}

// Get 当前状态（深拷贝，caller 可改）。
func (s *Store) Get() Override {
	if s == nil {
		return Override{}
	}
	if cur := s.cur.Load(); cur != nil {
		return *cur
	}
	return Override{}
}

// Set 替换当前状态。SetAt 自动填 now。
func (s *Store) Set(o Override) {
	if s == nil {
		return
	}
	if o.SetAt.IsZero() {
		o.SetAt = time.Now().UTC()
	}
	s.cur.Store(&o)
}

// Clear 把状态恢复到正常运行（disabled=false / force_score=0）。
// 保留 SetAt / SetBy 给 audit。
func (s *Store) Clear(actor string) {
	if s == nil {
		return
	}
	s.Set(Override{
		Disabled:   false,
		ForceScore: 0,
		Reason:     "cleared",
		SetBy:      actor,
	})
}

// IsActive true = override 影响主路径（disabled=true 或 force_score 非 0）。
// 给前端 / dashboard 用一个布尔判 "现在是否在覆盖状态"。
func (o Override) IsActive() bool {
	return o.Disabled || o.ForceScore != 0
}
