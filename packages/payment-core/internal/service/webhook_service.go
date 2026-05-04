package service

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/payment-core/internal/channel"
)

// WebhookService 把 order-core 转来的原始 headers/body 做规范化。
//
// 约束：
//   - payment-core 不自己落库，规范化后的 WebhookEvent 直接返回给 order-core
//     由 order-core 驱动状态机。
//   - 当 adapter 字段缺失时，按 headers 里的渠道标识做兜底识别（常见于
//     Maya / Grab 这类在 URL path 里带 adapter 的场景）。
type WebhookService struct {
	logger *zap.Logger
}

func NewWebhookService(logger *zap.Logger) *WebhookService {
	return &WebhookService{logger: logger}
}

// Parse 从 adapter + headers + body 拿规范化 WebhookEvent。
// 为了保持 payment-core 的无状态 + 无第三方凭据（adapter 的签名校验放在
// payment-channel 侧），这里只做 JSON 结构归一 + event_type 翻译。
func (s *WebhookService) Parse(ctx context.Context, adapter string, headers map[string]string, body []byte) (*channel.WebhookEvent, error) {
	// 通用 JSON 形状：尽量解出关键字段，用不上的进 raw_payload。
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	evt := &channel.WebhookEvent{
		Timestamp:  time.Now(),
		RawPayload: flattenStrings(raw),
	}
	// pi_id：payment-core 约定 adapter 把 idempotency_key = sha256(pi_id:action) 放进
	// 某个字段；order-core 侧真正把 pi_id 关联回来的是 external_ref_no → charge
	// 的反查链路。故这里我们只把渠道 ref / pi_id 提取出来，不强依赖字段名。
	evt.PaymentIntentID = firstString(raw, "pi_id", "paymentRequestId", "senderRefId",
		"requestReferenceNumber", "external_transaction_id", "partnerTxID")
	evt.ExternalRefNo = firstString(raw, "paymentId", "referenceNumber", "txID",
		"invoice_id", "id", "external_ref_no")
	evt.EventID = firstString(raw, "event_id", "eventId", "id")
	if evt.EventID == "" {
		evt.EventID = evt.ExternalRefNo
	}

	evt.EventType = mapEventType(adapter, raw)
	s.logger.Debug("webhook parsed",
		zap.String("adapter", adapter),
		zap.String("event_type", evt.EventType),
		zap.String("pi_id", evt.PaymentIntentID))
	return evt, nil
}

func mapEventType(adapter string, raw map[string]any) string {
	// 先尝试标准字段 event_type / type
	if t := firstString(raw, "event_type", "eventType", "type"); t != "" {
		return canonicalEvent(t)
	}
	// 否则看 status / paymentStatus
	st := firstString(raw, "status", "paymentStatus", "state")
	return statusToEvent(adapter, st)
}

// canonicalEventWhitelist 等值匹配（大写归一）→ 规范化事件类型。
//
// P2-2 资损保护：原实现用 strings.Contains 做模糊匹配，会把任意子串包含
// 这些片段的字段映射到强语义事件——例如某些渠道在原始 type 字段里返回
// "REFUND_FAILED_NOTIFICATION_TO_MERCHANT_OK" 含 "REFUND_FAIL"，会被错误识别为
// refund.failed，可能触发 order-core 的"退款失败"补偿逻辑（实际是退款成功）。
// 改为白名单等值匹配；未知事件类型透传 raw，由 order-core 兜底（落 audit log /
// dead-letter 队列）。
var canonicalEventWhitelist = map[string]string{
	// charge succeeded
	"CHARGE_SUCCESS":     "charge.succeeded",
	"CHARGE_SUCCEEDED":   "charge.succeeded",
	"PAYMENT_SUCCESS":    "charge.succeeded",
	"PAYMENT_SUCCEEDED":  "charge.succeeded",
	"CHECKOUT_SUCCESS":   "charge.succeeded",
	"CHECKOUT_SUCCEEDED": "charge.succeeded",
	// charge failed
	"CHARGE_FAIL":      "charge.failed",
	"CHARGE_FAILED":    "charge.failed",
	"PAYMENT_FAIL":     "charge.failed",
	"PAYMENT_FAILED":   "charge.failed",
	"CHECKOUT_FAILURE": "charge.failed",
	"CHECKOUT_FAILED":  "charge.failed",
	// refund
	"REFUND_SUCCESS":   "refund.succeeded",
	"REFUND_SUCCEEDED": "refund.succeeded",
	"REFUND_FAIL":      "refund.failed",
	"REFUND_FAILED":    "refund.failed",
	// 已经规范的型——直接放行
	"CHARGE.SUCCEEDED": "charge.succeeded",
	"CHARGE.FAILED":    "charge.failed",
	"REFUND.SUCCEEDED": "refund.succeeded",
	"REFUND.FAILED":    "refund.failed",
}

func canonicalEvent(raw string) string {
	if mapped, ok := canonicalEventWhitelist[strings.ToUpper(strings.TrimSpace(raw))]; ok {
		return mapped
	}
	// 未知事件类型透传 raw —— order-core 自己的事件分发兜底（落 audit / DLQ）。
	// 不再做 Contains 模糊匹配，避免子串误中。
	return raw
}

func statusToEvent(_ string, status string) string {
	switch strings.ToUpper(status) {
	case "SUCCESS", "SUCCEEDED", "PAID", "PAYMENT_SUCCESS", "CHECKOUT_SUCCESS", "SETTLED":
		return "charge.succeeded"
	case "FAIL", "FAILED", "CHECKOUT_FAILURE", "REJECTED", "RETURNED", "CANCELED", "EXPIRED":
		return "charge.failed"
	default:
		return "payment_intent.requires_action"
	}
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch t := v.(type) {
			case string:
				if t != "" {
					return t
				}
			case float64:
				// 某些字段是数字 id
				return jsonNumber(t)
			}
		}
	}
	return ""
}

func jsonNumber(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

func flattenStrings(m map[string]any) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		switch t := v.(type) {
		case string:
			out[k] = t
		case float64:
			out[k] = jsonNumber(t)
		case bool:
			if t {
				out[k] = "true"
			} else {
				out[k] = "false"
			}
		default:
			b, _ := json.Marshal(v)
			out[k] = string(b)
		}
	}
	return out
}
