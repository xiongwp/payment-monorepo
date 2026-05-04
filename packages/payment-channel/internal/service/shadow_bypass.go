// shadow_bypass: 影子流量在 AcquirerService 入口短路放行。
//
// 设计：压测 / shadow 流量绝不真打到外部支付渠道（GCash / Maya / GrabPay /
// ShopeePay / Coins.ph / InstaPay / Pesonet / PayMongo / Xendit / DragonPay /
// BillEase / BDO / Metrobank / Landbank 共 14 个渠道），否则会：
//
//   - 在外部渠道账上产生真实扣款（无法挽回）
//   - 触发外部渠道反欺诈 / 风控告警
//   - 浪费外部 API 配额（部分渠道按调用计费）
//   - 真给商户发短信 / 通知（GCash app push 等）
//
// 短路后行为：
//   - 不查 / 不写 acquirer_tx 表（避免污染主流量幂等表）
//   - 不调外部 HTTP / SDK
//   - 不刷 idem cache
//   - 返回 deterministic mock 结果（默认 succeeded）
//   - metric 标 result="shadow_dryrun" 让运营仪表盘可分流量观察
package service

import (
	"github.com/xiongwp/payment-channel/internal/channel"
	"github.com/xiongwp/payment-channel/internal/domain"
	"github.com/xiongwp/payment-channel/internal/metrics"
)

// shadowChargeResponse 构造 charge 的 shadow 短路响应。
//
// 默认返回 ResultSucceeded — 压测最常见关心的是「主链路能否扛 N qps」而不是
// 「不同 charge 结果分支」；如果某条压测用例需要测 RequiresAction / Failed 等
// 分支，可以根据 metadata 上的 shadow_scenario 字段在这里 switch（暂未实现）。
//
// ExternalRefNo 用 idempotencyKey 派生，保证幂等回放（同 idempotencyKey 多次
// 调用得到同一 ExternalRefNo）— 调用方仍可基于 ExternalRefNo 做 Query。
func shadowChargeResponse(adapter string, req *channel.ChargeRequest) *channel.ChargeResponse {
	metrics.AcquirerCallTotal.WithLabelValues(adapter, string(domain.ActionCharge), "shadow_dryrun").Inc()
	return &channel.ChargeResponse{
		Result:        channel.ResultSucceeded,
		ExternalRefNo: "shadow:" + req.IdempotencyKey,
		Raw:           map[string]string{"shadow": "dryrun"},
	}
}

// shadowOpResponse 给 Capture / Void / Refund 用的通用短路响应。
func shadowOpResponse(adapter string, action domain.AcquirerAction, idempotencyKey string) *channel.OpResponse {
	metrics.AcquirerCallTotal.WithLabelValues(adapter, string(action), "shadow_dryrun").Inc()
	return &channel.OpResponse{
		Result:        channel.ResultSucceeded,
		ExternalRefNo: "shadow:" + idempotencyKey,
	}
}

// shadowQueryResponse Query 的短路响应：影子流量永远报告 succeeded。
func shadowQueryResponse(adapter string, externalRefNo string) *channel.QueryResponse {
	metrics.AcquirerCallTotal.WithLabelValues(adapter, string(domain.ActionQuery), "shadow_dryrun").Inc()
	return &channel.QueryResponse{
		Result:         channel.ResultSucceeded,
		ExternalRefNo:  externalRefNo,
		AmountCaptured: 0,
		AmountRefunded: 0,
		Raw:            map[string]string{"shadow": "dryrun"},
	}
}
