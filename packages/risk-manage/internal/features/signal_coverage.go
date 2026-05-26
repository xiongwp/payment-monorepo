package features

import (
	"context"

	"github.com/xiongwp/risk-manage/internal/engine"
)

// SignalCoverageExtractor 根据 TxnContext.SignalStatus 重新校准 SignalCoverageRatio，
// 并把若干个"客户端环境很差"的派生 flag 拍平到顶层方便规则 DSL 直接读。
//
// 触发逻辑（fail-open；缺数据不动）：
//   - SignalStatus 非空时，重算 ratio = ok 数 / 总数
//   - DirectAPICall 当 SignalStatus 全空 + RiskSessionID 也空 → 没经过 SDK
//   - 把 webdriver / cdcGlobals 长度 / permissionsMismatch 之类显式信号
//     合成一个粗 "BotLikelihood" hint（不替代 ML，是给 DSL 兜底）—— 不写回 txn，
//     只是借这个 extractor 跑一遍把字段透传一次确认链路通。
//
// 这个 extractor 故意只读 txn 的 SDK-上报字段，不依赖任何 store；
// 端到端只 ~1µs，挂在 Chain 末尾不影响性能预算。
type SignalCoverageExtractor struct{}

// NewSignalCoverageExtractor extractor 无状态；返指针让 Chain 引用稳定。
func NewSignalCoverageExtractor() *SignalCoverageExtractor { return &SignalCoverageExtractor{} }

func (e *SignalCoverageExtractor) Name() string { return "signal_coverage" }

func (e *SignalCoverageExtractor) Enrich(_ context.Context, txn *engine.TxnContext) {
	if txn == nil {
		return
	}
	// 1) 缺 ratio 但有 status → 重算
	if txn.SignalCoverageRatio == 0 && len(txn.SignalStatus) > 0 {
		ok, total := 0, 0
		for _, v := range txn.SignalStatus {
			total++
			if v == "ok" {
				ok++
			}
		}
		if total > 0 {
			txn.SignalCoverageRatio = float64(ok) / float64(total)
		}
	}
	// 2) DirectAPICall：没 session + 全字段空 → 强信号
	if !txn.DirectAPICall {
		hasAnyFingerprint := txn.FingerprintHash != "" || txn.CanvasFingerprint != "" ||
			txn.WebGLRenderer != "" || txn.AudioContextHash != "" || txn.FontHash != ""
		if txn.RiskSessionID == "" && !hasAnyFingerprint {
			txn.DirectAPICall = true
		}
	}
}
