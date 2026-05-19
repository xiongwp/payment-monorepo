package handler

import (
	"context"
	"net/http"
	"time"

	paymentcorev1 "reconcile-system/packages/payment-core/kitex_gen/paymentcore/v1"

	"github.com/xiongwp/payment-admin-web/backend/internal/clients"
)

// ChannelHandler 暴露运维视角的渠道/路由工具。
//
// payment-core 目前是无状态的路由层，路由规则完全由它自己的 yaml 配置决定。
// 本 admin 提供两个运维能力（不做路由规则的增删改，因为那是配置面，应该靠配置中心）：
//
//   1. Probe：给定一个假 Charge 请求参数，问 payment-core 最终会路由到哪个 adapter？
//      （通过发一次 Charge 然后观察哪个 adapter 被命中，或返回 no_match）
//   2. WebhookTest：粘一段 webhook 原文 + headers，让 payment-core 做签名校验 + 归一化
//      解析，返回解析后的 WebhookEvent，便于渠道接入阶段快速 debug。
type ChannelHandler struct{ deps clients.Deps }

func NewChannelHandler(d clients.Deps) *ChannelHandler { return &ChannelHandler{deps: d} }

// POST /api/channels/routes/probe
// body: {country, payment_method, amount, currency, merchant}
// 返回: payment-core 路由到的 adapter；失败（no_match）时返回错误
func (h *ChannelHandler) ProbeRoute(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Country       string `json:"country"`
		PaymentMethod string `json:"payment_method"`
		Amount        int64  `json:"amount"`
		Currency      string `json:"currency"`
		Merchant      string `json:"merchant"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// 用一个带 "probe: true" 标记的假 Charge 来让 payment-core 暴露路由决策。
	// 真实 payment-channel 侧的 scripted adapter 在 admin 环境应当拒绝 probe 流量（靠 extra 标记）。
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := h.deps.PCore.Charge(ctx, &paymentcorev1.ChargeRequest{
		PaymentIntentId: "pi_admin_probe_" + time.Now().Format("20060102150405"),
		Amount:          body.Amount,
		Currency:        nonEmpty(body.Currency, "PHP"),
		Country:         body.Country,
		PaymentMethod:   body.PaymentMethod,
		Extra: map[string]string{
			"probe": "true",
		},
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{
		"result_type":     resp.GetResultType(),
		"external_ref_no": resp.GetExternalRefNo(),
		"failure_code":    resp.GetFailureCode(),
		"failure_message": resp.GetFailureMessage(),
	})
}

// POST /api/channels/webhook-test
// body: {adapter, headers: {}, body: "raw"}
func (h *ChannelHandler) WebhookTest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Adapter string            `json:"adapter"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Headers == nil {
		body.Headers = map[string]string{}
	}
	if body.Adapter != "" {
		body.Headers["X-Adapter"] = body.Adapter
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := h.deps.PCore.ParseWebhook(ctx, &paymentcorev1.ParseWebhookRequest{
		Adapter: body.Adapter,
		Headers: body.Headers,
		Body:    []byte(body.Body),
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{
		"event_id":          resp.GetEventId(),
		"event_type":        resp.GetEventType(),
		"payment_intent_id": resp.GetPaymentIntentId(),
		"charge_id":         resp.GetChargeId(),
		"refund_id":         resp.GetRefundId(),
		"external_ref_no":   resp.GetExternalRefNo(),
		"amount":            resp.GetAmount(),
		"timestamp":         resp.GetTimestamp(),
	})
}

func nonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
