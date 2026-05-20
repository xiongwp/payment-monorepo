package handler

import (
	"net/http"

	"github.com/gorilla/mux"
	accountingv1 "github.com/xiongwp/accounting-system/kitex_gen/accounting/v1"
	accountingservice "github.com/xiongwp/accounting-system/kitex_gen/accounting/v1/accountingservice"
)

// SnapshotHandler handles balance snapshot endpoints.
type SnapshotHandler struct {
	client accountingservice.Client
}

func NewSnapshotHandler(client accountingservice.Client) *SnapshotHandler {
	return &SnapshotHandler{client: client}
}

// GetBalanceSnapshot GET /v1/snapshots/{accountNo}
func (h *SnapshotHandler) GetBalanceSnapshot(w http.ResponseWriter, r *http.Request) {
	accountNo := mux.Vars(r)["accountNo"]
	date := r.URL.Query().Get("date")

	resp, err := h.client.GetBalanceSnapshot(r.Context(), &accountingv1.GetBalanceSnapshotRequest{
		AccountNo:    accountNo,
		SnapshotDate: date,
	})
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if resp.Code != 0 {
		writeError(w, int(resp.Code), resp.Message)
		return
	}
	s := resp.Snapshot
	if s == nil {
		writeError(w, 404, "snapshot not found")
		return
	}

	// BalanceSnapshot proto doesn't carry currency; fetch the account so the
	// frontend can format the decimal balance strings with the right symbol.
	// GetAccount failure here is non-fatal: we still return the snapshot, just
	// without currency — frontend will render without symbol rather than crash.
	currency := ""
	if acctResp, acctErr := h.client.GetAccount(r.Context(), &accountingv1.GetAccountRequest{
		Identifier: &accountingv1.GetAccountRequest_AccountNo{AccountNo: accountNo},
	}); acctErr == nil && acctResp.Code == 0 && acctResp.Account != nil {
		currency = acctResp.Account.Currency
	}

	writeJSON(w, map[string]interface{}{
		"account_no":        s.AccountNo,
		"snapshot_date":     s.SnapshotDate,
		"beginning_balance": s.BeginningBalance,
		"ending_balance":    s.EndingBalance,
		"total_debit":       s.TotalDebit,
		"total_credit":      s.TotalCredit,
		"transaction_count": s.TransactionCount,
		"currency":          currency,
	})
}
