package handler

import (
	"net/http"
	"strconv"

	accountingv1 "github.com/xiongwp/accounting-system/kitex_gen/accounting/v1"
)

// TransactionHandler handles account transaction query endpoints.
type TransactionHandler struct {
	client accountingservice.Client
}

func NewTransactionHandler(client accountingservice.Client) *TransactionHandler {
	return &TransactionHandler{client: client}
}

// transactionJSON matches the frontend AccountTransaction interface.
type transactionJSON struct {
	TransactionID       string `json:"transaction_id"`
	ParentTransactionID string `json:"parent_transaction_id,omitempty"`
	AccountNo           string `json:"account_no"`
	BusinessNo          string `json:"business_no"`
	BusinessType        int32  `json:"business_type"`
	DebitAmount         string `json:"debit_amount"`
	CreditAmount        string `json:"credit_amount"`
	BalanceBefore       string `json:"balance_before"`
	BalanceAfter        string `json:"balance_after"`
	Currency            string `json:"currency"`
	TransactionDate     string `json:"transaction_date"`
	TransactionTime     string `json:"transaction_time"`
	Description         string `json:"description,omitempty"`
	Status              int32  `json:"status"`
	BookingType         int32  `json:"booking_type"` // 1=实时记账 2=缓冲记账
}

// ListTransactions GET /v1/transactions
//
// Query params:
//
//	account_no   — filter by account number
//	business_no  — filter by business order number
//	transaction_id — filter by transaction ID
//	start_date   — YYYY-MM-DD
//	end_date     — YYYY-MM-DD
//	page         — 1-based page number (default 1)
//	page_size    — rows per page (default 20, max 100)
func (h *TransactionHandler) ListTransactions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	pageNum, _ := strconv.Atoi(q.Get("page"))
	if pageNum <= 0 {
		pageNum = 1
	}
	pageSize, _ := strconv.Atoi(q.Get("page_size"))
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}

	req := &accountingv1.GetTransactionRequest{
		TransactionId: q.Get("transaction_id"),
		BusinessNo:    q.Get("business_no"),
		AccountNo:     q.Get("account_no"),
		StartDate:     q.Get("start_date"),
		EndDate:       q.Get("end_date"),
		PageNum:       int32(pageNum),
		PageSize:      int32(pageSize),
	}

	resp, err := h.client.GetTransaction(r.Context(), req)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if resp.Code != 0 {
		writeError(w, int(resp.Code), resp.Message)
		return
	}

	rows := make([]*transactionJSON, 0, len(resp.Transactions))
	for _, tx := range resp.Transactions {
		row := &transactionJSON{
			TransactionID:       tx.TransactionId,
			ParentTransactionID: tx.ParentTransactionId,
			AccountNo:           tx.AccountNo,
			BusinessNo:          tx.BusinessNo,
			BusinessType:        int32(tx.BusinessType),
			DebitAmount:         tx.DebitAmount,
			CreditAmount:        tx.CreditAmount,
			BalanceBefore:       tx.BalanceBefore,
			BalanceAfter:        tx.BalanceAfter,
			Currency:            tx.Currency,
			TransactionDate:     tx.TransactionDate,
			Description:         tx.Description,
			Status:              tx.Status,
			BookingType:         tx.BookingType,
		}
		if tx.TransactionTime != nil {
			row.TransactionTime = tx.TransactionTime.AsTime().Format("2006-01-02T15:04:05Z07:00")
		}
		rows = append(rows, row)
	}

	writeJSON(w, map[string]interface{}{
		"list":      rows,
		"total":     resp.Total,
		"page":      pageNum,
		"page_size": pageSize,
	})
}
