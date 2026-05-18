// Package domain — split-payment 领域类型。
//
// SP-AC-7: 老 Rule / RuleItem / Plan / PlanItem / LegacyReversal / ReversalItem
// 类型已删除 — 它们是 SP-1 时代 Plan-based 拆分模型的产物, 在 SP-AC-3/7 引入
// `RunPlan + TransactionRequest + multi-leg` 之后就不再使用.
//
// 当前领域类型见:
//   - graph.go        : Graph / GraphSpec / Node / Edge / RunPlan / Movement
//   - transaction_request.go : TransactionRequest / TxnLeg (multi-leg)
//   - transfer.go     : ConnectedAccount / Transfer / ApplicationFee / Payout / Reversal (Stripe-style)
//   - account.go      : 银行账户绑定 / Payout destination token ref
package domain
