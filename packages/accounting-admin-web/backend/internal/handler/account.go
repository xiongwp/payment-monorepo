package handler

import (
	"net/http"
	"strconv"

	"github.com/gorilla/mux"
	accountingv1 "github.com/xiongwp/accounting-system/kitex_gen/accounting/v1"
	accountingservice "github.com/xiongwp/accounting-system/kitex_gen/accounting/v1/accountingservice"
)

// accountJSON matches the frontend Account interface
type accountJSON struct {
	AccountNo           string `json:"account_no"`
	UserID              int64  `json:"user_id"`
	AccountType         int32  `json:"account_type"`
	AccountCategory     int32  `json:"account_category"`
	AccountBusinessType int32  `json:"account_business_type"`
	Currency            string `json:"currency"`
	Balance             string `json:"balance"`
	FrozenBalance       string `json:"frozen_balance"`
	AvailableBalance    string `json:"available_balance"`
	// Frontend enum: DISABLED=0, ACTIVE=1, FROZEN=2; proto enum: DISABLED=1, ACTIVE=2, FROZEN=3
	Status    int32  `json:"status"`
	Version   int64  `json:"version"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func protoAccountToJSON(a *accountingv1.Account) *accountJSON {
	if a == nil {
		return nil
	}
	status := int32(a.Status) - 1 // map proto 1/2/3 → frontend 0/1/2
	if status < 0 {
		status = 0
	}
	j := &accountJSON{
		AccountNo:           a.AccountNo,
		UserID:              a.UserId,
		AccountType:         int32(a.AccountType),
		AccountCategory:     int32(a.Category),
		AccountBusinessType: int32(a.AccountBusinessType),
		Currency:            a.Currency,
		Balance:             a.Balance,
		FrozenBalance:       a.FrozenBalance,
		AvailableBalance:    a.AvailableBalance,
		Status:              status,
		Version:             a.Version,
	}
	if a.CreatedAt != nil {
		j.CreatedAt = a.CreatedAt.AsTime().Format("2006-01-02T15:04:05Z07:00")
	}
	if a.UpdatedAt != nil {
		j.UpdatedAt = a.UpdatedAt.AsTime().Format("2006-01-02T15:04:05Z07:00")
	}
	return j
}

// AccountHandler handles account-related endpoints.
type AccountHandler struct {
	client accountingservice.Client
}

func NewAccountHandler(client accountingservice.Client) *AccountHandler {
	return &AccountHandler{client: client}
}

// CreateAccount POST /v1/accounts
func (h *AccountHandler) CreateAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UserID              int64  `json:"user_id"`
		AccountType         int32  `json:"account_type"`
		Category            int32  `json:"category"`
		AccountBusinessType int32  `json:"account_business_type"`
		Currency            string `json:"currency"`
		Description         string `json:"description"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, 400, "invalid request: "+err.Error())
		return
	}
	resp, err := h.client.CreateAccount(r.Context(), &accountingv1.CreateAccountRequest{
		UserId:              req.UserID,
		AccountType:         accountingv1.AccountType(req.AccountType),
		Category:            accountingv1.AccountCategory(req.Category),
		Currency:            req.Currency,
		Description:         req.Description,
		AccountBusinessType: accountingv1.AccountBusinessType(req.AccountBusinessType),
	})
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if resp.Code != 0 {
		writeError(w, int(resp.Code), resp.Message)
		return
	}
	writeJSON(w, protoAccountToJSON(resp.Account))
}

// GetAccountByNo GET /v1/accounts/{accountNo}
func (h *AccountHandler) GetAccountByNo(w http.ResponseWriter, r *http.Request) {
	accountNo := mux.Vars(r)["accountNo"]
	resp, err := h.client.GetAccount(r.Context(), &accountingv1.GetAccountRequest{
		Identifier: &accountingv1.GetAccountRequest_AccountNo{AccountNo: accountNo},
	})
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if resp.Code != 0 {
		writeError(w, int(resp.Code), resp.Message)
		return
	}
	writeJSON(w, protoAccountToJSON(resp.Account))
}

// GetAccount GET /v1/accounts  (query params: account_no OR user_id+account_business_type [+currency])
//
// Returns:
//   - account_no given → single Account object
//   - user_id + account_business_type given → { "accounts": [...] } (multi-currency aware)
//     optional currency param narrows to a single row
//
// The shape branch is intentional: (user_id, business_type) is NOT a unique
// key because the same pair can hold balances in multiple currencies. Earlier
// versions used GetAccount with a oneof user_id_and_business_type identifier
// which returned .First() — admin-web would see a random currency's row and
// no way to tell the other rows existed. The list endpoint fixes that.
func (h *AccountHandler) GetAccount(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	accountNo := q.Get("account_no")
	userIDStr := q.Get("user_id")
	businessTypeStr := q.Get("account_business_type")
	currencyFilter := q.Get("currency")

	if accountNo != "" {
		resp, err := h.client.GetAccount(r.Context(), &accountingv1.GetAccountRequest{
			Identifier: &accountingv1.GetAccountRequest_AccountNo{AccountNo: accountNo},
		})
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		if resp.Code != 0 {
			writeError(w, int(resp.Code), resp.Message)
			return
		}
		writeJSON(w, protoAccountToJSON(resp.Account))
		return
	}

	if userIDStr == "" || businessTypeStr == "" {
		writeError(w, 400, "account_no or (user_id + account_business_type) required")
		return
	}
	userID, _ := strconv.ParseInt(userIDStr, 10, 64)
	businessType, _ := strconv.Atoi(businessTypeStr)
	listResp, err := h.client.ListAccountsByUserAndBusinessType(r.Context(), &accountingv1.ListAccountsByUserAndBusinessTypeRequest{
		UserId:              userID,
		AccountBusinessType: accountingv1.AccountBusinessType(businessType),
		Currency:            currencyFilter,
	})
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if listResp.Code != 0 {
		writeError(w, int(listResp.Code), listResp.Message)
		return
	}
	out := make([]*accountJSON, 0, len(listResp.Accounts))
	for _, a := range listResp.Accounts {
		out = append(out, protoAccountToJSON(a))
	}
	writeJSON(w, map[string]interface{}{"accounts": out, "count": len(out)})
}
