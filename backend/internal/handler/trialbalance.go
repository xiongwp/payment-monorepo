package handler

import (
	"net/http"

	accountingv1 "github.com/xiongwp/accounting-grpc-api/gen/accounting/v1"
)

// TrialBalanceHandler handles trial balance endpoints.
type TrialBalanceHandler struct {
	client accountingv1.AccountingServiceClient
}

func NewTrialBalanceHandler(client accountingv1.AccountingServiceClient) *TrialBalanceHandler {
	return &TrialBalanceHandler{client: client}
}

// RunTrialBalance POST /v1/trial-balance
// Body: {"snapshot_date": "2026-04-24", "currency": "PHP"}
// currency 必填——试算平衡按币种独立执行。
func (h *TrialBalanceHandler) RunTrialBalance(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SnapshotDate string `json:"snapshot_date"`
		Currency     string `json:"currency"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, 400, "invalid request: "+err.Error())
		return
	}
	if req.SnapshotDate == "" {
		writeError(w, 400, "snapshot_date is required")
		return
	}
	if req.Currency == "" {
		writeError(w, 400, "currency 必填：试算平衡按币种独立执行")
		return
	}

	resp, err := h.client.RunTrialBalance(r.Context(), &accountingv1.RunTrialBalanceRequest{
		SnapshotDate: req.SnapshotDate,
		Currency:     req.Currency,
	})
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if resp.Code != 0 {
		writeError(w, int(resp.Code), resp.Message)
		return
	}

	type summarySer struct {
		Category     string `json:"category"`
		Type         int32  `json:"type"`
		AccountCount int64  `json:"account_count"`
		SumBeginning string `json:"sum_beginning"`
		SumEnding    string `json:"sum_ending"`
		SumDebit     string `json:"sum_debit"`
		SumCredit    string `json:"sum_credit"`
	}

	summaries := make([]summarySer, 0, len(resp.Summaries))
	for _, s := range resp.Summaries {
		summaries = append(summaries, summarySer{
			Category:     s.Category,
			Type:         s.Type,
			AccountCount: s.AccountCount,
			SumBeginning: s.SumBeginning,
			SumEnding:    s.SumEnding,
			SumDebit:     s.SumDebit,
			SumCredit:    s.SumCredit,
		})
	}

	writeJSON(w, map[string]interface{}{
		"snapshot_date":            resp.SnapshotDate,
		"currency":                 req.Currency,
		"total_debit":              resp.TotalDebit,
		"total_credit":             resp.TotalCredit,
		"is_balanced":              resp.IsBalanced,
		"imbalance":                resp.Imbalance,
		"asset_ending_balance":     resp.AssetEndingBalance,
		"liability_ending_balance": resp.LiabilityEndingBalance,
		"equity_ending_balance":    resp.EquityEndingBalance,
		"revenue_ending_balance":   resp.RevenueEndingBalance,
		"expense_ending_balance":   resp.ExpenseEndingBalance,
		"is_equation_valid":        resp.IsEquationValid,
		"equation_diff":            resp.EquationDiff,
		"summaries":                summaries,
	})
}

// ListSnapshotDates GET /v1/trial-balance/dates
func (h *TrialBalanceHandler) ListSnapshotDates(w http.ResponseWriter, r *http.Request) {
	resp, err := h.client.ListSnapshotDates(r.Context(), &accountingv1.ListSnapshotDatesRequest{})
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if resp.Code != 0 {
		writeError(w, int(resp.Code), resp.Message)
		return
	}
	writeJSON(w, map[string]interface{}{"dates": resp.Dates})
}
