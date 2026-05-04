package handler

import (
	"context"
	"net/http"
	"strconv"
	"time"

	usermerchantv1 "github.com/xiongwp/user-merchant-core/api/proto/usermerchant/v1"

	"github.com/xiongwp/payment-admin-web/backend/internal/clients"
)

// UserMerchantAuditHandler 暴露 user-merchant-core 的 admin_audit_log 给 admin UI。
// 每条 mutation（Create/Rotate/SubmitKyc/...）都会自动写一条；这里只读回放。
type UserMerchantAuditHandler struct {
	deps clients.Deps
}

// NewUserMerchantAuditHandler 构造
func NewUserMerchantAuditHandler(d clients.Deps) *UserMerchantAuditHandler {
	return &UserMerchantAuditHandler{deps: d}
}

// List GET /api/user-merchant/audits?actor=&target=&limit=&offset=
// target 通常是 merchant_id；actor 是发起变更的 admin 用户 id / bearer 前 8 字符。
func (h *UserMerchantAuditHandler) List(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 {
		limit = 50
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := h.deps.UserMerchantAudit.List(ctx, &usermerchantv1.ListAuditLogsRequest{
		Actor:  r.URL.Query().Get("actor"),
		Target: r.URL.Query().Get("target"),
		Limit:  int32(limit),
		Offset: int32(offset),
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]any{"entries": resp.GetEntries()})
}
