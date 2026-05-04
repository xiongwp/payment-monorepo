package domain

import (
	"errors"
	"testing"
)

func TestPostingRequest_Validate(t *testing.T) {
	cases := []struct {
		name    string
		req     *PostingRequest
		wantErr error
	}{
		{
			name: "balanced two-leg",
			req: &PostingRequest{Lines: []PostingLine{
				{AccountID: "a", Debit: 100},
				{AccountID: "b", Credit: 100},
			}},
		},
		{
			name: "balanced three-leg (charge w/ fee)",
			req: &PostingRequest{Lines: []PostingLine{
				{AccountID: "ch", Debit: 1000},
				{AccountID: "mch", Credit: 950},
				{AccountID: "fee", Credit: 50},
			}},
		},
		{
			name:    "unbalanced",
			req:     &PostingRequest{Lines: []PostingLine{{AccountID: "a", Debit: 100}, {AccountID: "b", Credit: 99}}},
			wantErr: ErrLedgerUnbalanced,
		},
		{
			name:    "single leg",
			req:     &PostingRequest{Lines: []PostingLine{{AccountID: "a", Debit: 100}}},
			wantErr: ErrLedgerUnbalanced,
		},
		{
			name:    "both sides on one line",
			req:     &PostingRequest{Lines: []PostingLine{{AccountID: "a", Debit: 100, Credit: 100}, {AccountID: "b", Credit: 100}}},
			wantErr: ErrValidation,
		},
		{
			name:    "negative amount",
			req:     &PostingRequest{Lines: []PostingLine{{AccountID: "a", Debit: -1}, {AccountID: "b", Credit: -1}}},
			wantErr: ErrValidation,
		},
		{
			name:    "missing account id",
			req:     &PostingRequest{Lines: []PostingLine{{Debit: 100}, {AccountID: "b", Credit: 100}}},
			wantErr: ErrValidation,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.req.Validate()
			if tc.wantErr == nil && err != nil {
				t.Fatalf("want no error, got %v", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("want %v, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestGLAccount_NetBalance(t *testing.T) {
	cases := []struct {
		typ  AccountType
		d, c int64
		want int64
	}{
		{AccountAsset, 1000, 300, 700},           // net asset 700
		{AccountExpense, 500, 0, 500},            // expense balance
		{AccountLiability, 100, 1000, 900},       // net payable 900
		{AccountRevenue, 50, 500, 450},           // net income 450
		{AccountEquity, 0, 1000, 1000},           // net equity 1000
	}
	for _, tc := range cases {
		a := &GLAccount{Type: tc.typ, DebitBalance: tc.d, CreditBalance: tc.c}
		if got := a.NetBalance(); got != tc.want {
			t.Fatalf("type=%s d=%d c=%d: want %d, got %d", tc.typ, tc.d, tc.c, tc.want, got)
		}
	}
}
