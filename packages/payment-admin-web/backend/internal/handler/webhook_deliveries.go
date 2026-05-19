package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/mux"

	orderv1 "github.com/xiongwp/order-core/kitex_gen/order/v1"

	"github.com/xiongwp/payment-admin-web/backend/internal/clients"
)

// WebhookDeliveryHandler thin BFF over order-core's WebhookDeliveryService.
type WebhookDeliveryHandler struct {
	deps clients.Deps
	wd   webhookdeliveryservice.Client
}

// NewWebhookDeliveryHandler constructs the handler.
func NewWebhookDeliveryHandler(d clients.Deps, wd webhookdeliveryservice.Client) *WebhookDeliveryHandler {
	return &WebhookDeliveryHandler{deps: d, wd: wd}
}

// List GET /api/webhooks/deliveries
func (h *WebhookDeliveryHandler) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := h.wd.List(ctx, &orderv1.ListWebhookDeliveriesRequest{
		MerchantId: q.Get("merchant_id"),
		Status:     q.Get("status"),
		Limit:      int32(limit),
		Offset:     int32(offset),
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]any{"items": resp.GetItems(), "total": resp.GetTotal()})
}

// Retry POST /api/webhooks/deliveries/{id}/retry
func (h *WebhookDeliveryHandler) Retry(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(mux.Vars(r)["id"], 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad id")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := h.wd.Retry(ctx, &orderv1.RetryWebhookDeliveryRequest{Id: id})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]any{"delivery": resp.GetDelivery()})
}

// TestSend POST /api/webhooks/test-send
func (h *WebhookDeliveryHandler) TestSend(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MerchantID string          `json:"merchant_id"`
		EventType  string          `json:"event_type"`
		Payload    json.RawMessage `json:"payload"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	req := &orderv1.TestWebhookDeliveryRequest{
		MerchantId: body.MerchantID,
		EventType:  body.EventType,
		Payload:    string(body.Payload),
	}
	resp, err := h.wd.TestSend(ctx, req)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]any{"delivery": resp.GetDelivery()})
}
