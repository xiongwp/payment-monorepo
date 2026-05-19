package server

import (
	"context"
	"time"

	orderv1 "reconcile-system/packages/order-core/kitex_gen/order/v1"
	"github.com/xiongwp/order-core/internal/domain"
	"github.com/xiongwp/order-core/internal/service"
)

// LedgerServer adapts service.LedgerService onto the gRPC surface.
type LedgerServer struct {
	svc service.LedgerService
}

// NewLedgerServer constructs the adapter.
func NewLedgerServer(s service.LedgerService) *LedgerServer { return &LedgerServer{svc: s} }

// ─── read ────────────────────────────────────────────────────────────────────

func (l *LedgerServer) GetAccount(ctx context.Context, req *orderv1.GetLedgerAccountRequest) (*orderv1.GetLedgerAccountResponse, error) {
	a, err := l.svc.GetAccount(ctx, req.GetId())
	if err != nil {
		return nil, grpcErr(err)
	}
	return &orderv1.GetLedgerAccountResponse{Account: pbGLAccount(a)}, nil
}

func (l *LedgerServer) ListAccounts(ctx context.Context, req *orderv1.ListLedgerAccountsRequest) (*orderv1.ListLedgerAccountsResponse, error) {
	out, total, err := l.svc.ListAccounts(ctx,
		domain.AccountOwnerType(req.GetOwnerType()), req.GetOwnerId(),
		int(req.GetLimit()), int(req.GetOffset()))
	if err != nil {
		return nil, grpcErr(err)
	}
	resp := &orderv1.ListLedgerAccountsResponse{Total: total, Accounts: make([]*orderv1.GLAccount, 0, len(out))}
	for _, a := range out {
		resp.Accounts = append(resp.Accounts, pbGLAccount(a))
	}
	return resp, nil
}

func (l *LedgerServer) ListEntries(ctx context.Context, req *orderv1.ListLedgerEntriesRequest) (*orderv1.ListLedgerEntriesResponse, error) {
	var since, until *time.Time
	if v := req.GetSinceMs(); v > 0 {
		t := time.UnixMilli(v); since = &t
	}
	if v := req.GetUntilMs(); v > 0 {
		t := time.UnixMilli(v); until = &t
	}
	out, total, err := l.svc.ListEntries(ctx, req.GetAccountId(), since, until, int(req.GetLimit()), int(req.GetOffset()))
	if err != nil {
		return nil, grpcErr(err)
	}
	resp := &orderv1.ListLedgerEntriesResponse{Total: total, Entries: make([]*orderv1.GLEntry, 0, len(out))}
	for _, e := range out {
		resp.Entries = append(resp.Entries, pbGLEntry(e))
	}
	return resp, nil
}

func (l *LedgerServer) ListTransactions(ctx context.Context, req *orderv1.ListLedgerTransactionsRequest) (*orderv1.ListLedgerTransactionsResponse, error) {
	out, total, err := l.svc.ListTransactions(ctx, req.GetEventType(), req.GetRefType(), req.GetRefId(), int(req.GetLimit()), int(req.GetOffset()))
	if err != nil {
		return nil, grpcErr(err)
	}
	resp := &orderv1.ListLedgerTransactionsResponse{Total: total, Transactions: make([]*orderv1.GLTransaction, 0, len(out))}
	for _, t := range out {
		resp.Transactions = append(resp.Transactions, pbGLTxn(t, nil))
	}
	return resp, nil
}

func (l *LedgerServer) GetTransaction(ctx context.Context, req *orderv1.GetLedgerTransactionRequest) (*orderv1.GetLedgerTransactionResponse, error) {
	t, es, err := l.svc.GetTransaction(ctx, req.GetId())
	if err != nil {
		return nil, grpcErr(err)
	}
	return &orderv1.GetLedgerTransactionResponse{Transaction: pbGLTxn(t, es)}, nil
}

// ─── write ───────────────────────────────────────────────────────────────────

func (l *LedgerServer) PostCharge(ctx context.Context, req *orderv1.PostChargeRequest) (*orderv1.PostChargeResponse, error) {
	t, err := l.svc.PostCharge(ctx, req.GetMerchantId(), req.GetChannel(), req.GetRefType(), req.GetRefId(), req.GetGrossMinor(), req.GetFeeMinor())
	if err != nil {
		return nil, grpcErr(err)
	}
	return &orderv1.PostChargeResponse{Transaction: pbGLTxn(t, nil)}, nil
}

func (l *LedgerServer) PostRefund(ctx context.Context, req *orderv1.PostRefundRequest) (*orderv1.PostRefundResponse, error) {
	t, err := l.svc.PostRefund(ctx, req.GetMerchantId(), req.GetChannel(), req.GetRefType(), req.GetRefId(), req.GetGrossMinor(), req.GetFeeRebateMinor())
	if err != nil {
		return nil, grpcErr(err)
	}
	return &orderv1.PostRefundResponse{Transaction: pbGLTxn(t, nil)}, nil
}

func (l *LedgerServer) PostSettlementPayout(ctx context.Context, req *orderv1.PostSettlementPayoutRequest) (*orderv1.PostSettlementPayoutResponse, error) {
	t, err := l.svc.PostSettlementPayout(ctx, req.GetMerchantId(), req.GetRefId(), req.GetAmountMinor())
	if err != nil {
		return nil, grpcErr(err)
	}
	return &orderv1.PostSettlementPayoutResponse{Transaction: pbGLTxn(t, nil)}, nil
}

func (l *LedgerServer) PostChannelSettlement(ctx context.Context, req *orderv1.PostChannelSettlementRequest) (*orderv1.PostChannelSettlementResponse, error) {
	t, err := l.svc.PostChannelSettlement(ctx, req.GetChannel(), req.GetRefId(), req.GetAmountMinor())
	if err != nil {
		return nil, grpcErr(err)
	}
	return &orderv1.PostChannelSettlementResponse{Transaction: pbGLTxn(t, nil)}, nil
}

func (l *LedgerServer) PostAdjustment(ctx context.Context, req *orderv1.PostAdjustmentRequest) (*orderv1.PostAdjustmentResponse, error) {
	lines := make([]domain.PostingLine, 0, len(req.GetLines()))
	for _, pl := range req.GetLines() {
		lines = append(lines, domain.PostingLine{
			AccountID: pl.GetAccountId(),
			Debit:     pl.GetDebit(),
			Credit:    pl.GetCredit(),
			Memo:      pl.GetMemo(),
		})
	}
	t, err := l.svc.PostAdjustment(ctx, req.GetMemo(), req.GetActor(), lines)
	if err != nil {
		return nil, grpcErr(err)
	}
	return &orderv1.PostAdjustmentResponse{Transaction: pbGLTxn(t, nil)}, nil
}

// ─── conversions ─────────────────────────────────────────────────────────────

func pbGLAccount(a *domain.GLAccount) *orderv1.GLAccount {
	return &orderv1.GLAccount{
		Id:            a.ID,
		Name:          a.Name,
		Type:          string(a.Type),
		OwnerType:     string(a.OwnerType),
		OwnerId:       a.OwnerID,
		Currency:      a.Currency,
		DebitBalance:  a.DebitBalance,
		CreditBalance: a.CreditBalance,
		NetBalance:    a.NetBalance(),
		Version:       a.Version,
		Status:        a.Status,
		CreatedMs:     a.Created.UnixMilli(),
		UpdatedMs:     a.Updated.UnixMilli(),
	}
}

func pbGLEntry(e *domain.GLEntry) *orderv1.GLEntry {
	return &orderv1.GLEntry{
		Id:           e.ID,
		TxnId:        e.TxnID,
		AccountId:    e.AccountID,
		DebitAmount:  e.DebitAmount,
		CreditAmount: e.CreditAmount,
		Currency:     e.Currency,
		Memo:         e.Memo,
		CreatedMs:    e.Created.UnixMilli(),
	}
}

func pbGLTxn(t *domain.GLTransaction, entries []*domain.GLEntry) *orderv1.GLTransaction {
	out := &orderv1.GLTransaction{
		Id:          t.ID,
		EventType:   t.EventType,
		RefType:     t.RefType,
		RefId:       t.RefID,
		TotalDebit:  t.TotalDebit,
		TotalCredit: t.TotalCredit,
		Memo:        t.Memo,
		Reverses:    t.Reverses,
		CreatedMs:   t.Created.UnixMilli(),
	}
	for _, e := range entries {
		out.Entries = append(out.Entries, pbGLEntry(e))
	}
	return out
}
