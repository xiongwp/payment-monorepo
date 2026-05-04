package rules

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/xiongwp/risk-manage/internal/engine"
)

func TestEmailRep_NoEndpoint_NoOp(t *testing.T) {
	r, err := EmailReputationFactory()("er", "email_rep", true, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	hit := r.Evaluate(context.Background(), &engine.TxnContext{
		Metadata: map[string]string{"email": "x@y.com"},
	})
	if hit != nil {
		t.Fatal("no endpoint configured → must no-op")
	}
}

func TestEmailRep_LowScoreHits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"score":10,"suspicious":false,"disposable":false}`))
	}))
	defer srv.Close()
	r, err := EmailReputationFactory()("er", "email_rep", true, json.RawMessage(
		`{"endpoint":"`+srv.URL+`","min_score":30,"decision":"deny"}`))
	if err != nil {
		t.Fatal(err)
	}
	hit := r.Evaluate(context.Background(), &engine.TxnContext{
		Metadata: map[string]string{"email": "low@score.com"},
	})
	if hit == nil || hit.Decision != engine.Deny {
		t.Fatalf("expected DENY hit, got %+v", hit)
	}
}

func TestEmailRep_GoodScoreNoHit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"score":90,"suspicious":false,"disposable":false}`))
	}))
	defer srv.Close()
	r, err := EmailReputationFactory()("er", "email_rep", true, json.RawMessage(
		`{"endpoint":"`+srv.URL+`","min_score":30}`))
	if err != nil {
		t.Fatal(err)
	}
	hit := r.Evaluate(context.Background(), &engine.TxnContext{
		Metadata: map[string]string{"email": "good@score.com"},
	})
	if hit != nil {
		t.Fatalf("good email should not hit; got %+v", hit)
	}
}

func TestEmailRep_DisposableHits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"score":80,"disposable":true}`))
	}))
	defer srv.Close()
	r, _ := EmailReputationFactory()("er", "email_rep", true, json.RawMessage(
		`{"endpoint":"`+srv.URL+`","min_score":30}`))
	hit := r.Evaluate(context.Background(), &engine.TxnContext{
		Metadata: map[string]string{"email": "x@temp.io"},
	})
	if hit == nil {
		t.Fatal("disposable=true should hit even with high score")
	}
}

func TestEmailRep_FailOpen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", 500)
	}))
	defer srv.Close()
	// fail_open=true → 外部失败不命中
	r, _ := EmailReputationFactory()("er", "email_rep", true, json.RawMessage(
		`{"endpoint":"`+srv.URL+`","fail_open":true}`))
	hit := r.Evaluate(context.Background(), &engine.TxnContext{
		Metadata: map[string]string{"email": "x@y.com"},
	})
	if hit != nil {
		t.Fatalf("fail_open should yield no hit on upstream error; got %+v", hit)
	}
}

func TestEmailRep_Caches(t *testing.T) {
	calls := atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"score":90}`))
	}))
	defer srv.Close()
	r, _ := EmailReputationFactory()("er", "email_rep", true, json.RawMessage(
		`{"endpoint":"`+srv.URL+`","cache_sec":3600}`))
	for i := 0; i < 5; i++ {
		_ = r.Evaluate(context.Background(), &engine.TxnContext{
			Metadata: map[string]string{"email": "same@example.com"},
		})
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("expected 1 upstream call (cache hit 4x), got %d", got)
	}
}

func TestEmailRep_NoEmailMetadata_Skips(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"score":1}`))
	}))
	defer srv.Close()
	r, _ := EmailReputationFactory()("er", "email_rep", true, json.RawMessage(
		`{"endpoint":"`+srv.URL+`"}`))
	hit := r.Evaluate(context.Background(), &engine.TxnContext{Metadata: map[string]string{}})
	if hit != nil {
		t.Fatal("no email in metadata → no-op")
	}
}
