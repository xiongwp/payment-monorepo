package handler

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/mux"

	orderv1 "reconcile-system/packages/order-core/kitex_gen/order/v1"

	"github.com/xiongwp/payment-admin-web/backend/internal/clients"
)

type LedgerHandler struct {
	deps clients.Deps
	cli  ledgerservice.Client
}

func NewLedgerHandler(d clients.Deps, cli ledgerservice.Client) *LedgerHandler {
	return &LedgerHandler{deps: d, cli: cli}
}

func (h *LedgerHandler) ListAccounts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := h.cli.ListAccounts(ctx, &orderv1.ListLedgerAccountsRequest{
		OwnerType: q.Get("owner_type"),
		OwnerId:   q.Get("owner_id"),
		Limit:     int32(limit),
		Offset:    int32(offset),
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]any{"accounts": resp.GetAccounts(), "total": resp.GetTotal()})
}

func (h *LedgerHandler) GetAccount(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := h.cli.GetAccount(ctx, &orderv1.GetLedgerAccountRequest{Id: id})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, resp.GetAccount())
}

func (h *LedgerHandler) ListEntries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	var sinceMs, untilMs int64
	if s := q.Get("since_ms"); s != "" {
		sinceMs, _ = strconv.ParseInt(s, 10, 64)
	}
	if s := q.Get("until_ms"); s != "" {
		untilMs, _ = strconv.ParseInt(s, 10, 64)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := h.cli.ListEntries(ctx, &orderv1.ListLedgerEntriesRequest{
		AccountId: q.Get("account_id"),
		SinceMs:   sinceMs, UntilMs: untilMs,
		Limit: int32(limit), Offset: int32(offset),
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]any{"entries": resp.GetEntries(), "total": resp.GetTotal()})
}

func (h *LedgerHandler) ListTransactions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := h.cli.ListTransactions(ctx, &orderv1.ListLedgerTransactionsRequest{
		EventType: q.Get("event_type"),
		RefType:   q.Get("ref_type"),
		RefId:     q.Get("ref_id"),
		Limit:     int32(limit), Offset: int32(offset),
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]any{"transactions": resp.GetTransactions(), "total": resp.GetTotal()})
}

func (h *LedgerHandler) GetTransaction(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := h.cli.GetTransaction(ctx, &orderv1.GetLedgerTransactionRequest{Id: id})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, resp.GetTransaction())
}
