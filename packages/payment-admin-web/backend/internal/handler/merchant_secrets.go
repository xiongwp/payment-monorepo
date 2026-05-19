package handler

import (
	"context"
	"net/http"
	"time"

	"github.com/gorilla/mux"

	usermerchantv1 "github.com/xiongwp/user-merchant-core/kitex_gen/usermerchant/v1"
	merchantsecretservice "github.com/xiongwp/user-merchant-core/kitex_gen/usermerchant/v1/merchantsecretservice"

	"github.com/xiongwp/payment-admin-web/backend/internal/clients"
)

// MerchantSecretHandler thin BFF over order-core MerchantSecretService.
// Plaintext is only accepted inbound (Put); List only ever returns masked
// hints. Deletes require the full (merchant, channel, field) triple in the
// URL so it's hard to accidentally wipe the wrong merchant.
type MerchantSecretHandler struct {
	deps clients.Deps
	cli  merchantsecretservice.Client
}

func NewMerchantSecretHandler(d clients.Deps, cli merchantsecretservice.Client) *MerchantSecretHandler {
	return &MerchantSecretHandler{deps: d, cli: cli}
}

// GET /api/merchants/{id}/secrets?channel=gcash
func (h *MerchantSecretHandler) List(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	ch := r.URL.Query().Get("channel")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := h.cli.List(ctx, &usermerchantv1.ListMerchantSecretsRequest{
		MerchantId: id, Channel: ch,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]any{"secrets": resp.GetSecrets()})
}

// POST /api/merchants/{id}/secrets  {channel, field_name, plaintext, actor}
func (h *MerchantSecretHandler) Put(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	var body struct {
		Channel   string `json:"channel"`
		FieldName string `json:"field_name"`
		Plaintext string `json:"plaintext"`
		Actor     string `json:"actor"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Plaintext == "" {
		writeError(w, http.StatusBadRequest, "plaintext required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := h.cli.Put(ctx, &usermerchantv1.PutMerchantSecretRequest{
		MerchantId: id,
		Channel:    body.Channel,
		FieldName:  body.FieldName,
		Plaintext:  body.Plaintext,
		Actor:      body.Actor,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]any{"secret": resp.GetSecret()})
}

// DELETE /api/merchants/{id}/secrets/{channel}/{field_name}
func (h *MerchantSecretHandler) Delete(w http.ResponseWriter, r *http.Request) {
	v := mux.Vars(r)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	_, err := h.cli.Delete(ctx, &usermerchantv1.DeleteMerchantSecretRequest{
		MerchantId: v["id"], Channel: v["channel"], FieldName: v["field_name"],
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
