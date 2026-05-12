// Package outboxhook — payment-core 接 outbox 的桥.
//
// 用法同 clearing-settlement / refund-engine. payment-core 的 event types:
//   "ChargeCreated"     -- intent 创建
//   "ChargeAuthorized"  -- 卡组授权成功
//   "ChargeCaptured"    -- 扣款完成
//   "ChargeFailed"      -- 各种失败
//   "ChargeDeclined"    -- issuer 拒
//   "ChargeRefunded"    -- 实际是 refund-engine 触发, 这里跟一份
//
// 接 outbox 后, downstream 服务可订阅:
//   - accounting     (出会计分录)
//   - merchant-webhook (推商户)
//   - split-payment  (分账)
//   - tax-reporting  (年度汇集)
//   - risk-manage    (实时风控更新)

package outboxhook
