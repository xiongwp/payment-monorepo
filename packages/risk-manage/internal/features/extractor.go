// Package features 风控引擎的特征提取层。
//
// 商业风控引擎（Sift / Forter / Stripe Radar）的标准架构：
//
//	[业务侧调用 Screen with thin txn]
//	          ↓
//	[FeatureExtractor chain] ← 时间 / 币种 / 卡 / 客户历史 / Geo / device 唯一性
//	          ↓
//	[engine.Evaluate with enriched txn]
//
// 把"算特征"和"用特征"分离让：
//   - 规则只读 typed 字段，不知道数据怎么来的（解耦）
//   - 新加 extractor 不改规则代码
//   - extractor 失败 fail-open（拿不到特征 → 0/空，规则按"无信号"行为）
//   - 新模型训练复用同一套 extractor，避免训练 / 在线 skew
//
// 每个 extractor < 1ms（多数纯计算 + 内存 lookup）；并发跑成本小。
//
// 当前注册的 extractor（按典型 Chain 顺序）：
//   TimeExtractor          时间特征（HourOfDay / IsBusinessHours 等）
//   CurrencyConvertExtractor   币种归一（AmountUSD）
//   CardFeatureExtractor   卡 BIN → CardBrand / CardFunding / CardCountry
//   CustomerHistoryExtractor   客户聚合（LinkStore + Counter）
//   SignalCoverageExtractor    web-sdk v0.2 信号覆盖率 + DirectAPICall 推断
//
// SDK v0.2.0 引入 25+ 信号 + SignalCoverage，TxnContext 上对应字段（FontHash /
// WebRTCLocalIPs / BatteryPresent / DeviceMemory / PixelRatio / ColorDepth /
// TouchSupport / Webdriver / CDCGlobals / ChromeRuntime / PermissionsMismatch /
// MouseTrajectory 派生 / Keystroke biometrics）由 service.fillSessionFields
// 在 Screen 入口透传，extractor 这一层只补 SignalCoverageRatio 重算 +
// DirectAPICall 推断；不重复 SDK 已经算好的字段。
package features

import (
	"context"

	"github.com/xiongwp/risk-manage/internal/engine"
)

// Extractor 把 thin TxnContext 富化（写回更多字段）。
//
// 实现合约：
//   - 必须无副作用（不写 LinkStore / Counter / Audit；那些是 service 层职责）
//   - 失败必须 fail-open（log warning + 不报错），不阻塞 Screen 主路径
//   - 不修改入参的非自家字段（避免上下游 extractor 互相覆盖）
type Extractor interface {
	Name() string
	Enrich(ctx context.Context, txn *engine.TxnContext)
}

// Chain 按顺序串多个 extractor。每个独立失败不影响后续。
type Chain []Extractor

// Enrich 顺序执行；ctx 透传共享（OTel span 链可见）。
func (c Chain) Enrich(ctx context.Context, txn *engine.TxnContext) {
	for _, e := range c {
		e.Enrich(ctx, txn)
	}
}
