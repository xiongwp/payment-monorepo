package handler

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/mux"

	orderv1 "github.com/xiongwp/order-core/kitex_gen/order/v1"

	"github.com/xiongwp/payment-admin-web/backend/internal/clients"
)

type OrderHandler struct{ deps clients.Deps }

func NewOrderHandler(d clients.Deps) *OrderHandler { return &OrderHandler{deps: d} }

// GET /api/orders?mch_id=&page=&page_size=
func (h *OrderHandler) List(w http.ResponseWriter, r *http.Request) {
	mchID := r.URL.Query().Get("mch_id")
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 200 {
		pageSize = 20
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := h.deps.PI.List(ctx, &orderv1.ListPaymentIntentsRequest{
		MchId:    mchID,
		Page:     int32(page),
		PageSize: int32(pageSize),
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	out := make([]map[string]interface{}, 0, len(resp.GetPaymentIntents()))
	for _, pi := range resp.GetPaymentIntents() {
		out = append(out, piSummary(pi))
	}
	writeJSON(w, map[string]interface{}{
		"items": out,
		"total": resp.GetTotal(),
		"page":  page, "page_size": pageSize,
	})
}

// GET /api/orders/{id}
func (h *OrderHandler) Retrieve(w http.ResponseWriter, r *http.Request) {
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

// GET /api/orders/{id}/charges
func (h *OrderHandler) ListCharges(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := h.deps.Charge.List(ctx, &orderv1.ListChargesRequest{PaymentIntentId: id})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	out := make([]map[string]interface{}, 0, len(resp.GetCharges()))
	for _, c := range resp.GetCharges() {
		out = append(out, chargeSummary(c))
	}
	writeJSON(w, map[string]interface{}{"items": out})
}

// GET /api/orders/{id}/refunds
func (h *OrderHandler) ListRefunds(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := h.deps.Refund.List(ctx, &orderv1.ListRefundsRequest{PaymentIntentId: id})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	out := make([]map[string]interface{}, 0, len(resp.GetRefunds()))
	for _, rfd := range resp.GetRefunds() {
		out = append(out, refundSummary(rfd))
	}
	writeJSON(w, map[string]interface{}{"items": out})
}

// ─── field projections ─────────────────────────────────────────────

func piSummary(pi *orderv1.PaymentIntent) map[string]interface{} {
	if pi == nil {
		return nil
	}
	return map[string]interface{}{
		"id":              pi.GetId(),
		"amount":          pi.GetAmount(),
		"currency":        pi.GetCurrency(),
		"status":          pi.GetStatus().String(),
		"mch_id":          pi.GetMchId(),
		"mch_order_no":    pi.GetMchOrderNo(),
		"business_id":     pi.GetBusinessId(),
		"description":     pi.GetDescription(),
		"capture_method":  pi.GetCaptureMethod().String(),
		"created":         tsUnix(pi.GetCreated()),
	}
}

func piDetail(pi *orderv1.PaymentIntent) map[string]interface{} {
	if pi == nil {
		return nil
	}
	m := piSummary(pi)
	m["amount_subtotal"] = pi.GetAmountSubtotal()
	m["amount_coupon"] = pi.GetAmountCoupon()
	m["amount_points"] = pi.GetAmountPoints()
	m["customer_id"] = pi.GetCustomerId()
	m["idempotency_key"] = pi.GetIdempotencyKey()
	m["confirmation_method"] = pi.GetConfirmationMethod().String()
	m["metadata"] = pi.GetMetadata()
	return m
}

func chargeSummary(c *orderv1.Charge) map[string]interface{} {
	if c == nil {
		return nil
	}
	return map[string]interface{}{
		"id":                c.GetId(),
		"payment_intent_id": c.GetPaymentIntentId(),
		"amount":            c.GetAmount(),
		"amount_captured":   c.GetAmountCaptured(),
		"amount_refunded":   c.GetAmountRefunded(),
		"currency":          c.GetCurrency(),
		"status":            c.GetStatus().String(),
		"payment_method":    c.GetPaymentMethod(),
		"created":           tsUnix(c.GetCreated()),
	}
}

func refundSummary(r *orderv1.Refund) map[string]interface{} {
	if r == nil {
		return nil
	}
	return map[string]interface{}{
		"id":                r.GetId(),
		"payment_intent_id": r.GetPaymentIntentId(),
		"charge_id":         r.GetChargeId(),
		"amount":            r.GetAmount(),
		"currency":          r.GetCurrency(),
		"status":            r.GetStatus().String(),
		"reason":            r.GetReason().String(),
		"created":           tsUnix(r.GetCreated()),
	}
}

func tsUnix(ts interface{ GetSeconds() int64 }) int64 {
	if ts == nil {
		return 0
	}
	return ts.GetSeconds()
}
