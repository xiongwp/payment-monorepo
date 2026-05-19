package handler

import (
	"net/http"
	"strconv"

	"github.com/gorilla/mux"
	accountingv1 "reconcile-system/packages/accounting-system/kitex_gen/accounting/v1"
)

// AdminHandler handles buffer account admin endpoints via gRPC.
type AdminHandler struct {
	admin accountingadminservice.Client
}

func NewAdminHandler(admin accountingadminservice.Client) *AdminHandler {
	return &AdminHandler{admin: admin}
}

// ─── Buffer account handlers ──────────────────────────────────────────────────

// ListBufferAccounts GET /v1/buffer-accounts
func (h *AdminHandler) ListBufferAccounts(w http.ResponseWriter, r *http.Request) {
	resp, err := h.admin.ListBufferAccounts(r.Context(), &accountingv1.ListBufferAccountsRequest{})
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if resp.Code != 0 {
		writeError(w, int(resp.Code), resp.Message)
		return
	}
	writeJSON(w, resp.Items)
}

// CreateBufferAccount POST /v1/buffer-accounts
func (h *AdminHandler) CreateBufferAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccountNo          string `json:"account_no"`
		FlushIntervalLevel int32  `json:"flush_interval_level"`
		Description        string `json:"description"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, 400, "invalid request body")
		return
	}
	resp, err := h.admin.CreateBufferAccount(r.Context(), &accountingv1.CreateBufferAccountRequest{
		AccountNo:          req.AccountNo,
		FlushIntervalLevel: req.FlushIntervalLevel,
		Description:        req.Description,
	})
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if resp.Code != 0 {
		writeError(w, int(resp.Code), resp.Message)
		return
	}
	writeJSON(w, resp.Item)
}

// UpdateBufferAccount PUT /v1/buffer-accounts/{id}
func (h *AdminHandler) UpdateBufferAccount(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(mux.Vars(r)["id"], 10, 64)
	if err != nil {
		writeError(w, 400, "invalid id")
		return
	}
	var req struct {
		Enabled            bool   `json:"enabled"`
		FlushIntervalLevel int32  `json:"flush_interval_level"`
		Description        string `json:"description"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, 400, "invalid request body")
		return
	}
	resp, err := h.admin.UpdateBufferAccount(r.Context(), &accountingv1.UpdateBufferAccountRequest{
		Id:                 id,
		Enabled:            req.Enabled,
		FlushIntervalLevel: req.FlushIntervalLevel,
		Description:        req.Description,
	})
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if resp.Code != 0 {
		writeError(w, int(resp.Code), resp.Message)
		return
	}
	writeJSON(w, map[string]string{"message": "updated"})
}

// DeleteBufferAccount DELETE /v1/buffer-accounts/{id}
func (h *AdminHandler) DeleteBufferAccount(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(mux.Vars(r)["id"], 10, 64)
	if err != nil {
		writeError(w, 400, "invalid id")
		return
	}
	resp, err := h.admin.DeleteBufferAccount(r.Context(), &accountingv1.DeleteBufferAccountRequest{Id: id})
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if resp.Code != 0 {
		writeError(w, int(resp.Code), resp.Message)
		return
	}
	writeJSON(w, map[string]string{"message": "deleted"})
}
