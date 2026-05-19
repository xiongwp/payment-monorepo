package handler

import (
	"net/http"

	accountingv1 "github.com/xiongwp/accounting-system/kitex_gen/accounting/v1"
	accountingservice "github.com/xiongwp/accounting-system/kitex_gen/accounting/v1/accountingservice"
)

// DayCutHandler handles day-cut endpoints.
type DayCutHandler struct {
	client accountingservice.Client
}

func NewDayCutHandler(client accountingservice.Client) *DayCutHandler {
	return &DayCutHandler{client: client}
}

// TriggerDayCut POST /v1/day-cut
// Body: {"cut_date": "2026-04-24", "currency": "PHP"}
// currency 必填——日切按币种独立执行，不接受跨币种汇总。
func (h *DayCutHandler) TriggerDayCut(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CutDate  string `json:"cut_date"`
		Currency string `json:"currency"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, 400, "invalid request: "+err.Error())
		return
	}
	if req.Currency == "" {
		writeError(w, 400, "currency 必填：日切按币种独立执行")
		return
	}
	resp, err := h.client.TriggerDayCut(r.Context(), &accountingv1.TriggerDayCutRequest{
		CutDate:  req.CutDate,
		Currency: req.Currency,
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
		"cut_date": req.CutDate,
		"currency": req.Currency,
		"status":   "ok",
	})
}

// GetDayCutHistory GET /v1/day-cut/history
// ListDayCutHistory RPC 在 proto 精简时砍掉, 临时降级为空 entries 列表.
func (h *DayCutHandler) GetDayCutHistory(w http.ResponseWriter, r *http.Request) {
	_ = r
	writeJSON(w, map[string]interface{}{"entries": []map[string]interface{}{}, "stub": true})
}
