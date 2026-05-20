package handler

import (
	"net/http"
	"strconv"

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

// GetDayCutHistory GET /v1/day-cut/history?from=YYYY-MM-DD&to=YYYY-MM-DD&limit=N
//
// TECH-DEBT-1 已把 server 端 RPC 接到 dayCutSvc.ListDayCutHistory; 这里走 Kitex
// 调真后端, 失败 → 5xx, 不再静默返空.
func (h *DayCutHandler) GetDayCutHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from := q.Get("from")
	to := q.Get("to")
	limit := int32(0)
	if s := q.Get("limit"); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			limit = int32(v)
		}
	}
	resp, err := h.client.ListDayCutHistory(r.Context(), &accountingv1.ListDayCutHistoryRequest{
		FromDate: from,
		ToDate:   to,
		Limit:    limit,
	})
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if resp.Code != 0 {
		writeError(w, int(resp.Code), resp.Message)
		return
	}
	type entryJSON struct {
		CutDate     string `json:"cut_date"`
		RunID       int32  `json:"run_id"`
		Currency    string `json:"currency"`
		TotalShards int32  `json:"total_shards"`
		Pending     int32  `json:"pending"`
		Processing  int32  `json:"processing"`
		Completed   int32  `json:"completed"`
		Failed      int32  `json:"failed"`
	}
	out := make([]entryJSON, 0, len(resp.Entries))
	for _, e := range resp.Entries {
		out = append(out, entryJSON{
			CutDate:     e.CutDate,
			RunID:       e.RunId,
			Currency:    e.Currency,
			TotalShards: e.TotalShards,
			Pending:     e.Pending,
			Processing:  e.Processing,
			Completed:   e.Completed,
			Failed:      e.Failed,
		})
	}
	writeJSON(w, map[string]interface{}{"entries": out, "count": len(out)})
}
