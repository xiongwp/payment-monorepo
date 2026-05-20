package handler

import (
	"net/http"

	accountingv1 "github.com/xiongwp/accounting-system/kitex_gen/accounting/v1"
	accountingservice "github.com/xiongwp/accounting-system/kitex_gen/accounting/v1/accountingservice"
)

// BookingHandler handles double-entry booking endpoints.
type BookingHandler struct {
	client accountingservice.Client
}

func NewBookingHandler(client accountingservice.Client) *BookingHandler {
	return &BookingHandler{client: client}
}

// DoubleEntryBooking POST /v1/bookings
//
// 幂等性：调用方必须在 HTTP 头中提供 X-Request-ID，透传为 proto 字段 request_id。
// 同一 X-Request-ID 重复提交时，服务端返回首次成功的结果，不会重复记账。
func (h *BookingHandler) DoubleEntryBooking(w http.ResponseWriter, r *http.Request) {
	// 幂等键：从 HTTP 请求头读取，放入 proto request_id 字段
	requestID := r.Header.Get("X-Request-ID")
	if requestID == "" {
		writeError(w, 400, "X-Request-ID header is required for idempotency")
		return
	}

	var req struct {
		BusinessNo   string `json:"business_no"`
		BusinessType int32  `json:"business_type"`
		Currency     string `json:"currency"`
		Description  string `json:"description"`
		Entries      []struct {
			AccountNo    string `json:"account_no"`
			DebitAmount  string `json:"debit_amount"`
			CreditAmount string `json:"credit_amount"`
			Description  string `json:"description"`
		} `json:"entries"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, 400, "invalid request: "+err.Error())
		return
	}

	entries := make([]*accountingv1.AccountingEntry, len(req.Entries))
	for i, e := range req.Entries {
		entries[i] = &accountingv1.AccountingEntry{
			AccountNo:    e.AccountNo,
			DebitAmount:  e.DebitAmount,
			CreditAmount: e.CreditAmount,
			Description:  e.Description,
		}
	}

	resp, err := h.client.DoubleEntryBooking(r.Context(), &accountingv1.DoubleEntryBookingRequest{
		RequestId:    requestID,
		BusinessNo:   req.BusinessNo,
		BusinessType: accountingv1.BusinessType(req.BusinessType),
		Currency:     req.Currency,
		Description:  req.Description,
		Entries:      entries,
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
		"voucher_no":      resp.VoucherNo,
		"transaction_ids": resp.TransactionIds,
	})
}
