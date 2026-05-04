// handler/app.go —— "模拟商户 App" 后端：把一次典型的 Stripe-style 下单支付
// 链路三步串起来给前端 App 页面调用。
//
//	POST /api/app/create-intent   商户创建订单   → order-core.PaymentIntentService.Create
//	POST /api/app/confirm         用户选支付方式 → order-core.PaymentIntentService.Confirm
//	                                                （订单侧会 → payment-core → payment-channel）
//	GET  /api/app/intent/{id}     查订单当前状态 → order-core.PaymentIntentService.Retrieve
//
// 不包括真实商户鉴权 / 用户登录；仅作联调 / 演示。
package handler

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"

	orderv1 "github.com/xiongwp/order-core/api/proto/order/v1"

	"github.com/xiongwp/payment-admin-web/backend/internal/clients"
)

type AppHandler struct{ deps clients.Deps }

func NewAppHandler(d clients.Deps) *AppHandler { return &AppHandler{deps: d} }

// defaultMaxAppAmount is the upper bound (in storage units = minor × 100) on
// the mock-merchant create-intent endpoint when no override is configured.
// 1_000_00 minor × 100 = 100_000_00 ≈ ¥10,000 — large enough for staging
// flows, small enough that an accidental real-world misuse can't drain a big
// merchant float.
const defaultMaxAppAmount int64 = 1_000_00 * 100

// maxAppAmount returns the configured ceiling for /api/app/create-intent
// (env: MAX_APP_AMOUNT, storage units). Falls back to defaultMaxAppAmount.
func maxAppAmount() int64 {
	if s := os.Getenv("MAX_APP_AMOUNT"); s != "" {
		if v, err := strconv.ParseInt(s, 10, 64); err == nil && v > 0 {
			return v
		}
	}
	return defaultMaxAppAmount
}

// POST /api/app/create-intent
// body: { mch_id, mch_order_no, amount, currency, description, payment_method_types[], country, ... }
//
// PROD GATING: the route is **not registered** in APP_ENV=prod (see cmd/server)
// because this endpoint is a developer-facing mock that creates *real* PIs in
// payment-core. This handler additionally guards with a 404 short-circuit so a
// route-table mistake does not become a money mover.
func (h *AppHandler) CreateIntent(w http.ResponseWriter, r *http.Request) {
	if IsProdEnv() {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	var body struct {
		MchID              string            `json:"mch_id"`
		MchOrderNo         string            `json:"mch_order_no"`
		Amount             int64             `json:"amount"`
		Currency           string            `json:"currency"`
		Country            string            `json:"country"`
		Description        string            `json:"description"`
		PaymentMethodTypes []string          `json:"payment_method_types"`
		BusinessID         string            `json:"business_id"`
		CustomerID         string            `json:"customer_id"`
		ReturnURL          string            `json:"return_url"`
		NotifyURL          string            `json:"notify_url"`
		IdempotencyKey     string            `json:"idempotency_key"`
		Metadata           map[string]string `json:"metadata"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.MchID == "" {
		writeError(w, http.StatusBadRequest, "mch_id required")
		return
	}
	if body.Amount <= 0 {
		writeError(w, http.StatusBadRequest, "amount must be > 0")
		return
	}
	if maxAmt := maxAppAmount(); body.Amount > maxAmt {
		writeError(w, http.StatusBadRequest, "amount exceeds configured maximum")
		return
	}
	// Idempotency at the BFF tier — keyed off the same idempotency_key we will
	// pass through to order-core. Acts as a fast-path to suppress accidental
	// double-submissions before they fan out to gRPC. Final guarantee remains
	// payment-core's responsibility.
	if body.IdempotencyKey != "" {
		if cached, ok := idempotencyCache.Lookup("app:create-intent:" + body.IdempotencyKey); ok {
			writeJSON(w, cached)
			return
		}
	}
	if body.Currency == "" {
		body.Currency = "PHP"
	}
	if body.Country == "" {
		body.Country = "PH" // 联调默认值，便于命中 PH adapters
	}
	if body.BusinessID == "" {
		body.BusinessID = body.MchID // 分片 fallback：用 mch_id 作为 business_id
	}
	// 把 country 注入 metadata —— payment-core 的路由按 metadata.country 决定 adapter
	if body.Metadata == nil {
		body.Metadata = map[string]string{}
	}
	if body.Metadata["country"] == "" {
		body.Metadata["country"] = body.Country
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	resp, err := h.deps.PI.Create(ctx, &orderv1.CreatePaymentIntentRequest{
		Amount:             body.Amount,
		Currency:           strings.ToUpper(body.Currency),
		CustomerId:         body.CustomerID,
		Description:        body.Description,
		MchId:              body.MchID,
		MchOrderNo:         body.MchOrderNo,
		BusinessId:         body.BusinessID,
		IdempotencyKey:     body.IdempotencyKey,
		PaymentMethodTypes: body.PaymentMethodTypes,
		ReturnUrl:          body.ReturnURL,
		NotifyUrl:          body.NotifyURL,
		Metadata:           body.Metadata,
		ConfirmationMethod: orderv1.ConfirmationMethod_CONFIRMATION_METHOD_MANUAL,
		CaptureMethod:      orderv1.CaptureMethod_CAPTURE_METHOD_AUTOMATIC,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	out := piDetail(resp.GetPaymentIntent())
	if body.IdempotencyKey != "" {
		idempotencyCache.Store("app:create-intent:"+body.IdempotencyKey, out)
	}
	writeJSON(w, out)
}

// POST /api/app/confirm
// body: { id, payment_method }
// 等价于收银台选了某个支付方式去确认。
func (h *AppHandler) Confirm(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID            string `json:"id"`
		PaymentMethod string `json:"payment_method"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.ID == "" || body.PaymentMethod == "" {
		writeError(w, http.StatusBadRequest, "id and payment_method required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	resp, err := h.deps.PI.Confirm(ctx, &orderv1.ConfirmPaymentIntentRequest{
		Id:            body.ID,
		PaymentMethod: strings.ToUpper(body.PaymentMethod),
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	out := map[string]interface{}{
		"payment_intent": piDetail(resp.GetPaymentIntent()),
	}
	if c := resp.GetCharge(); c != nil {
		out["charge"] = chargeSummary(c)
	}
	if na := resp.GetNextAction(); na != nil {
		expires := int64(0)
		if ts := na.GetExpiresAt(); ts != nil {
			expires = ts.GetSeconds()
		}
		out["next_action"] = map[string]interface{}{
			"id":                na.GetId(),
			"action_type":       na.GetActionType(),
			"payment_intent_id": na.GetPaymentIntentId(),
			"charge_id":         na.GetChargeId(),
			"status":            na.GetStatus().String(),
			"payload":           na.GetPayload(),
			"expires_at":        expires,
		}
	}
	writeJSON(w, out)
}

// GET /api/app/intent/{id}
func (h *AppHandler) RetrieveIntent(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := h.deps.PI.Retrieve(ctx, &orderv1.RetrievePaymentIntentRequest{Id: id})
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, piDetail(resp.GetPaymentIntent()))
}
