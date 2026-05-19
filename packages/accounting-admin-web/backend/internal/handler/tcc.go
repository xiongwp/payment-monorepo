package handler

import (
	"net/http"
	"strconv"

	"github.com/gorilla/mux"
	accountingv1 "reconcile-system/packages/accounting-system/kitex_gen/accounting/v1"
)

// TccHandler handles TCC maintenance endpoints.
type TccHandler struct {
	client accountingservice.Client
}

func NewTccHandler(client accountingservice.Client) *TccHandler {
	return &TccHandler{client: client}
}

func protoTccBranch(b *accountingv1.TccBranchInfo) map[string]interface{} {
	createdAt, updatedAt := "", ""
	if b.CreatedAt != nil {
		createdAt = b.CreatedAt.AsTime().Format("2006-01-02T15:04:05Z07:00")
	}
	if b.UpdatedAt != nil {
		updatedAt = b.UpdatedAt.AsTime().Format("2006-01-02T15:04:05Z07:00")
	}
	return map[string]interface{}{
		"tcc_id":        b.TccId,
		"branch_id":     b.BranchId,
		"account_no":    b.AccountNo,
		"balance_delta": b.BalanceDelta,
		"frozen_amount": b.FrozenAmount,
		"status":        b.Status,
		"db_index":      b.DbIndex,
		"table_index":   b.TableIndex,
		"created_at":    createdAt,
		"updated_at":    updatedAt,
	}
}

// GetTccStatus GET /v1/tcc/{tccId}
func (h *TccHandler) GetTccStatus(w http.ResponseWriter, r *http.Request) {
	tccID := mux.Vars(r)["tccId"]
	resp, err := h.client.GetTccStatus(r.Context(), &accountingv1.GetTccStatusRequest{TccId: tccID})
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if resp.Code != 0 {
		writeError(w, int(resp.Code), resp.Message)
		return
	}
	branches := make([]map[string]interface{}, len(resp.Branches))
	for i, b := range resp.Branches {
		branches[i] = protoTccBranch(b)
	}
	writeJSON(w, map[string]interface{}{
		"tcc_id":         resp.TccId,
		"overall_status": resp.OverallStatus,
		"branch_count":   resp.BranchCount,
		"branches":       branches,
	})
}

// ListStuckTcc GET /v1/tcc/stuck
func (h *TccHandler) ListStuckTcc(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	timeoutMinutes := int32(5)
	limit := int32(50)
	if v := q.Get("timeout_minutes"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			timeoutMinutes = int32(n)
		}
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = int32(n)
		}
	}
	resp, err := h.client.ListStuckTcc(r.Context(), &accountingv1.ListStuckTccRequest{
		TimeoutMinutes: timeoutMinutes,
		Limit:          limit,
	})
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if resp.Code != 0 {
		writeError(w, int(resp.Code), resp.Message)
		return
	}
	branches := make([]map[string]interface{}, len(resp.Branches))
	for i, b := range resp.Branches {
		branches[i] = protoTccBranch(b)
	}
	writeJSON(w, map[string]interface{}{
		"count":    resp.Count,
		"branches": branches,
	})
}

// CancelTcc POST /v1/tcc/{tccId}/cancel
func (h *TccHandler) CancelTcc(w http.ResponseWriter, r *http.Request) {
	tccID := mux.Vars(r)["tccId"]
	resp, err := h.client.CancelTcc(r.Context(), &accountingv1.CancelTccRequest{TccId: tccID})
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if resp.Code != 0 {
		writeError(w, int(resp.Code), resp.Message)
		return
	}
	writeJSON(w, map[string]interface{}{
		"tcc_id": resp.TccId,
		"result": resp.Result,
	})
}

// CancelTccBranch POST /v1/tcc/branches/{branchId}/cancel
func (h *TccHandler) CancelTccBranch(w http.ResponseWriter, r *http.Request) {
	branchID := mux.Vars(r)["branchId"]
	accountNo := r.URL.Query().Get("account_no")
	if accountNo == "" {
		writeError(w, 400, "account_no required")
		return
	}
	resp, err := h.client.CancelTccBranch(r.Context(), &accountingv1.CancelTccBranchRequest{
		BranchId:  branchID,
		AccountNo: accountNo,
	})
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if resp.Code != 0 {
		writeError(w, int(resp.Code), resp.Message)
		return
	}
	writeJSON(w, map[string]interface{}{
		"branch_id": resp.BranchId,
		"result":    resp.Result,
	})
}
