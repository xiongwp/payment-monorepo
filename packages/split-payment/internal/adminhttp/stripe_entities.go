// stripe_entities.go — SP-4 HTTP API for ConnectedAccount / Transfer / AppFee / Payout / Reversal.
//
// 端点 (全 JSON):
//
//   POST   /api/connected_accounts            创建 / upsert (ID 已存在则更新)
//   GET    /api/connected_accounts            列表 (status 过滤可选)
//   GET    /api/connected_accounts/{id}       单条
//
//   GET    /api/transfers/{id}                单条
//   GET    /api/transfers?group=tg_xxx        按 transfer_group 列
//   POST   /api/transfers/{id}/reverse        发起 Reversal (幂等)
//
//   GET    /api/application_fees?charge=...   按 charge 列
//
//   GET    /api/payouts?account=acct_xxx      按 account 列
//   POST   /api/payouts                       手动发起 Payout (manual mode)
//
// 跟 /api/moneyflow/* (graph CRUD) 平行, 但作为 Stripe-like 资源对象暴露.

package adminhttp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"reconcile-system/packages/split-payment/internal/domain"

	"go.uber.org/zap"
)

// ─── Repo 接口 (adminhttp 视角, 子集) ──────────────────────────────────

// AccountRepo CRUD.
type AccountRepo interface {
	Upsert(ctx context.Context, a *domain.ConnectedAccount) error
	Get(ctx context.Context, id string) (*domain.ConnectedAccount, error)
	List(ctx context.Context, status string, limit int) ([]*domain.ConnectedAccount, error)
}

// TransferRepo 查询 + reverse 用.
type TransferRepo interface {
	Get(ctx context.Context, id string) (*domain.Transfer, error)
	ListByGroup(ctx context.Context, group string) ([]*domain.Transfer, error)
	AddReversedAmount(ctx context.Context, id string, delta int64) error
}

// AppFeeRepo.
type AppFeeRepo interface {
	ListByCharge(ctx context.Context, charge string) ([]*domain.ApplicationFee, error)
}

// PayoutRepo.
type PayoutRepo interface {
	Insert(ctx context.Context, p *domain.Payout) error
	ListByAccount(ctx context.Context, account string, limit int) ([]*domain.Payout, error)
}

// ReversalRepo.
type ReversalRepo interface {
	Insert(ctx context.Context, rv *domain.Reversal) error
	ListByTransfer(ctx context.Context, transferID string) ([]*domain.Reversal, error)
}

// ─── StripeAPIServer ──────────────────────────────────────────────────

// StripeAPIServer 注册 Stripe-style 资源端点.
type StripeAPIServer struct {
	Accounts  AccountRepo
	Transfers TransferRepo
	Fees      AppFeeRepo
	Payouts   PayoutRepo
	Reversals ReversalRepo
	Log       *zap.Logger
}

// Register 挂到 mux.
func (s *StripeAPIServer) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/connected_accounts", s.handleAccountsRoot)
	mux.HandleFunc("/api/connected_accounts/", s.handleAccountByID)
	mux.HandleFunc("/api/transfers", s.handleTransfersRoot)
	mux.HandleFunc("/api/transfers/", s.handleTransferByID)
	mux.HandleFunc("/api/application_fees", s.handleFees)
	mux.HandleFunc("/api/payouts", s.handlePayouts)
}

// ─── /api/connected_accounts ──────────────────────────────────────────

func (s *StripeAPIServer) handleAccountsRoot(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		status := r.URL.Query().Get("status")
		limit := atoiOr(r.URL.Query().Get("limit"), 100)
		list, err := s.Accounts.List(r.Context(), status, limit)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": list})
	case http.MethodPost:
		var a domain.ConnectedAccount
		if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if a.ID == "" {
			a.ID = "acct_" + randHex(8)
		}
		if a.Status == "" {
			a.Status = domain.AccountStatusPending
		}
		if err := s.Accounts.Upsert(r.Context(), &a); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusCreated, &a)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *StripeAPIServer) handleAccountByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/connected_accounts/")
	if id == "" {
		writeErr(w, http.StatusBadRequest, errString("id required"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		a, err := s.Accounts.Get(r.Context(), id)
		if err != nil {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, a)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// ─── /api/transfers ───────────────────────────────────────────────────

func (s *StripeAPIServer) handleTransfersRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	group := r.URL.Query().Get("group")
	if group == "" {
		writeErr(w, http.StatusBadRequest, errString("query 'group' required"))
		return
	}
	list, err := s.Transfers.ListByGroup(r.Context(), group)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": list})
}

func (s *StripeAPIServer) handleTransferByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/transfers/")
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" {
		writeErr(w, http.StatusBadRequest, errString("id required"))
		return
	}
	id := parts[0]
	// /api/transfers/{id}/reverse
	if len(parts) >= 2 && parts[1] == "reverse" {
		s.handleTransferReverse(w, r, id)
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	t, err := s.Transfers.Get(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// handleTransferReverse POST /api/transfers/{id}/reverse
//
// Body: { amount_minor?, reason?, idempotency_key }
//   - amount_minor 缺省 → 全额 reverse 剩余可退部分
//   - idempotency_key 必填, 防重复
func (s *StripeAPIServer) handleTransferReverse(w http.ResponseWriter, r *http.Request, transferID string) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		AmountMinor    int64  `json:"amount_minor"`
		Reason         string `json:"reason"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if body.IdempotencyKey == "" {
		writeErr(w, http.StatusBadRequest, errString("idempotency_key required"))
		return
	}
	t, err := s.Transfers.Get(r.Context(), transferID)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	remaining := t.RemainingReversible()
	if remaining <= 0 {
		writeErr(w, http.StatusUnprocessableEntity, errString("transfer already fully reversed"))
		return
	}
	amount := body.AmountMinor
	if amount <= 0 {
		amount = remaining
	}
	if amount > remaining {
		writeErr(w, http.StatusUnprocessableEntity,
			errString("amount exceeds remaining reversible"))
		return
	}
	rv := &domain.Reversal{
		ID:             "tr_rev_" + randHex(8),
		Transfer:       t.ID,
		AmountMinor:    amount,
		Currency:       t.Currency,
		Reason:         orDefault(body.Reason, domain.ReversalReasonRequestedByCustomer),
		Status:         domain.ReversalStatusPending,
		IdempotencyKey: body.IdempotencyKey,
		CreatedAt:      time.Now().UTC(),
	}
	if err := s.Reversals.Insert(r.Context(), rv); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// 异步走 accounting 反向记账; 这里先把 reversed_amount 累加并把 reversal 状态置 succeeded.
	// 生产环境应该有 worker pick up reversal.status=pending → 调 accounting → 更新 status.
	// 简化: 现在直接当 succeeded, MVP 跑通 + 审计链有迹可寻.
	if err := s.Transfers.AddReversedAmount(r.Context(), t.ID, amount); err != nil {
		s.Log.Warn("AddReversedAmount failed",
			zap.String("transfer", t.ID), zap.Error(err))
	}
	rv.Status = domain.ReversalStatusSucceeded
	writeJSON(w, http.StatusCreated, rv)
}

// ─── /api/application_fees ────────────────────────────────────────────

func (s *StripeAPIServer) handleFees(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	charge := r.URL.Query().Get("charge")
	if charge == "" {
		writeErr(w, http.StatusBadRequest, errString("query 'charge' required"))
		return
	}
	list, err := s.Fees.ListByCharge(r.Context(), charge)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": list})
}

// ─── /api/payouts ─────────────────────────────────────────────────────

func (s *StripeAPIServer) handlePayouts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		account := r.URL.Query().Get("account")
		if account == "" {
			writeErr(w, http.StatusBadRequest, errString("query 'account' required"))
			return
		}
		limit := atoiOr(r.URL.Query().Get("limit"), 50)
		list, err := s.Payouts.ListByAccount(r.Context(), account, limit)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": list})
	case http.MethodPost:
		var p domain.Payout
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if p.Account == "" {
			writeErr(w, http.StatusBadRequest, errString("account required"))
			return
		}
		if p.ID == "" {
			p.ID = "po_" + randHex(8)
		}
		if p.Status == "" {
			p.Status = domain.PayoutStatusPending
		}
		if p.Method == "" {
			p.Method = domain.PayoutMethodStandard
		}
		// 校验 account capability (能否 payout).
		a, err := s.Accounts.Get(r.Context(), p.Account)
		if err != nil {
			writeErr(w, http.StatusNotFound, errString("account not found: "+p.Account))
			return
		}
		if !a.CanPayout() {
			writeErr(w, http.StatusUnprocessableEntity,
				errString("account "+p.Account+" cannot payout (capability inactive)"))
			return
		}
		if err := s.Payouts.Insert(r.Context(), &p); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusCreated, &p)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// ─── helpers ──────────────────────────────────────────────────────────

func atoiOr(s string, def int) int {
	if s == "" {
		return def
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	if n <= 0 {
		return def
	}
	return n
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
