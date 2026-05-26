// Package service: per-stage timeout budget for Screen()。
//
// 动机：Screen 此前把 caller 的 ctx 一路透传给 ipintel.Lookup / mlscore.Score /
// engine.Evaluate / auditSink.Write。任一下游卡住即把整条 Screen 卡住，
// breaker 是事后熔断 — 首批 5 笔受害请求毫无保护，SLA 99.95% 写不出来。
//
// 现在每个 stage 用独立 sub-context 强制 deadline；超时 fail-open（用空结果
// 继续）并打 metric。global_ceiling 给整个 Screen 一个总上限，确保任何
// 编排错误都不会把单笔 Screen 拖过 ceiling。
//
// fail-policy 当前固定 fail-open。ScreenTimeouts.FailPolicy 字段预留，**未
// 实装**；为后续 per-merchant 策略（高风险商户可选 fail-close）留接口。
package service

import (
	"context"
	"os"
	"time"
)

// ScreenTimeouts per-stage budget + 全局上限。零值 → 默认宽松值（见 Defaults）。
//
// 单位是 time.Duration 而不是 ms，让 caller 表达更自然
// （prod 配 15 * time.Millisecond，test 配 1 * time.Second）。
//
// 默认值刻意调宽（每个 1s，ceiling 5s）—— 若所有现有测试用零值构造 svc
// 而没配 timeout，它们的 mock 都在 µs 级返回，绝不会触发；同时 prod 配
// 通过 SetScreenTimeouts 覆盖到严格值（feature_extract 20ms / ip_intel 15ms /
// ml_score 30ms / engine_eval 10ms / audit_write async / ceiling 100ms）。
type ScreenTimeouts struct {
	FeatureExtract time.Duration
	IPIntel        time.Duration
	MLScore        time.Duration
	EngineEval     time.Duration
	// AuditWrite 0 或 < 0 视作 async（fire-and-forget goroutine，不进 stage 计时）。
	// > 0 时同步等待并 enforce timeout —— 不推荐（audit 慢不应阻塞主路径），但留口。
	AuditWrite time.Duration
	// GlobalCeiling 整个 Screen 不能超过的硬上限。0 或 < 0 → 不设上限
	// （保留旧行为，给现有测试不破坏）。
	GlobalCeiling time.Duration
	// FailPolicy 当前固定 "open"。未来扩展 "close" / "challenge"；
	// 字段保留但 Screen 代码暂不读，避免 silently 引入新行为。
	FailPolicy string
}

// Defaults 缺省：宽松值，给现有测试 / dev 环境用。prod 走 main.go
// 从 viper / env 覆盖。FailPolicy = "open"。
//
// 为何宽松不严格：现有 ~10 个 service / engine 测试用零值构造 RiskService，
// 严格值（如 IPIntel 15ms）会让 MemService 在高负载机器上偶发 flaky；
// 宽松默认 + 显式 prod 覆盖 = 测试稳定 + 生产严格。
func defaultScreenTimeouts() ScreenTimeouts {
	return ScreenTimeouts{
		FeatureExtract: 1 * time.Second,
		IPIntel:        1 * time.Second,
		MLScore:        1 * time.Second,
		EngineEval:     1 * time.Second,
		AuditWrite:     0, // async
		GlobalCeiling:  0, // off by default
		FailPolicy:     "open",
	}
}

// normalize 补齐零值字段（让 SetScreenTimeouts 的 caller 可以只配感兴趣的字段）。
func (t ScreenTimeouts) normalize() ScreenTimeouts {
	d := defaultScreenTimeouts()
	if t.FeatureExtract <= 0 {
		t.FeatureExtract = d.FeatureExtract
	}
	if t.IPIntel <= 0 {
		t.IPIntel = d.IPIntel
	}
	if t.MLScore <= 0 {
		t.MLScore = d.MLScore
	}
	if t.EngineEval <= 0 {
		t.EngineEval = d.EngineEval
	}
	// AuditWrite: 保留 ≤0 = async 语义，不覆盖
	// GlobalCeiling: 保留 ≤0 = 不设上限，不覆盖
	if t.FailPolicy == "" {
		t.FailPolicy = d.FailPolicy
	}
	return t
}

// disabled 用于测试 / env 紧急关停：完全跳过 timeout 包装走旧路径。
// 触发条件：RISK_SCREEN_TIMEOUTS_DISABLED=1。
func screenTimeoutsDisabled() bool {
	return os.Getenv("RISK_SCREEN_TIMEOUTS_DISABLED") == "1"
}

// SetScreenTimeouts 注入 per-stage timeout budget。零值字段会用 default
// 补齐；nil-safe（caller 不调 = 用 default）。main.go 启动期调一次即可。
func (s *RiskService) SetScreenTimeouts(t ScreenTimeouts) {
	s.timeouts = t.normalize()
}

// activeTimeouts 返回有效的 timeout 配置；service 未显式 SetScreenTimeouts
// 时退到 default 宽松值。
func (s *RiskService) activeTimeouts() ScreenTimeouts {
	if s.timeouts.FeatureExtract == 0 && s.timeouts.IPIntel == 0 &&
		s.timeouts.MLScore == 0 && s.timeouts.EngineEval == 0 {
		return defaultScreenTimeouts()
	}
	return s.timeouts
}

// stageWithTimeout 跑一个可能阻塞的 stage 函数。返回是否 timeout / 是否 err。
// stage 名只用于 metric label / log；fn 不许返回值（外面用闭包捕获）。
//
// 实现：fn 在 goroutine 跑，select wait done / ctx.Done()。timeout 时
// goroutine 不被强制 kill（Go runtime 不支持），但 ctx 已取消 → 下游若
// 正确处理 ctx 会自行退出。本函数不等 goroutine，保证 caller 不被卡。
func stageWithTimeout(parent context.Context, budget time.Duration, fn func(ctx context.Context)) (timedOut bool) {
	if budget <= 0 {
		// budget 0 视作"不 enforce"；直接同步跑（保留旧行为兜底）
		fn(parent)
		return false
	}
	ctx, cancel := context.WithTimeout(parent, budget)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(ctx)
	}()
	select {
	case <-done:
		return false
	case <-ctx.Done():
		// parent 也可能 Done（global ceiling 触发）。统一返 timeout=true
		// 让 caller 走 fail-open；区分 parent vs stage 由 caller 查
		// parent.Err() 自行决定。
		return true
	}
}
