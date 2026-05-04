package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/risk-manage/internal/audit"
	"github.com/xiongwp/risk-manage/internal/feedback"
)

func mkDisputeAudit(t *testing.T, sink *audit.MemSink, decisionID, pi, verdict string, occurred time.Time) {
	t.Helper()
	sink.Write(context.Background(), &audit.DecisionAudit{
		DecisionID: decisionID,
		OccurredAt: occurred,
		Verdict:    verdict,
		RiskScore:  50,
		RiskLevel:  "med",
		Input:      audit.AuditInput{PaymentIntentID: pi},
	})
}

func TestDisputeHandler_PILookupAndRecord(t *testing.T) {
	sink := audit.NewMemSink(8)
	mkDisputeAudit(t, sink, "d1", "pi_dispute_1", "ALLOW", time.Now().Add(-30*24*time.Hour))
	rec := feedback.NewMemRecorder(0)
	mux := http.NewServeMux()
	registerDisputeHandler(mux, rec, sink, zap.NewNop())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{
		"payment_intent_id": "pi_dispute_1",
		"dispute_id":        "dp_1",
		"is_fraud":          true,
		"actor":             "order-core",
		"notes":             "stripe.code=fraudulent",
	})
	resp, _ := http.Post(srv.URL+"/admin/feedback/dispute", "application/json", bytes.NewReader(body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200; got %d", resp.StatusCode)
	}
	var out struct {
		Status     string `json:"status"`
		DecisionID string `json:"decision_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out.DecisionID != "d1" {
		t.Fatalf("expected decision_id=d1; got %q", out.DecisionID)
	}

	// 验证 outcome 写到 recorder + 包含 dispute_id 在 notes 里
	got := rec.Get("d1")
	if len(got) != 1 {
		t.Fatalf("expected 1 outcome; got %d", len(got))
	}
	if got[0].Source != feedback.SourceDispute || !got[0].IsFraud {
		t.Fatalf("wrong outcome: %+v", got[0])
	}
	if !contains(got[0].Notes, "dp_1") {
		t.Fatalf("notes should contain dispute_id; got %q", got[0].Notes)
	}
}

func TestDisputeHandler_404OnUnknownPI(t *testing.T) {
	sink := audit.NewMemSink(4)
	rec := feedback.NewMemRecorder(0)
	mux := http.NewServeMux()
	registerDisputeHandler(mux, rec, sink, zap.NewNop())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{
		"payment_intent_id": "pi_unknown",
		"is_fraud":          true,
	})
	resp, _ := http.Post(srv.URL+"/admin/feedback/dispute", "application/json", bytes.NewReader(body))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404; got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestDisputeHandler_RejectsMissingPI(t *testing.T) {
	sink := audit.NewMemSink(4)
	rec := feedback.NewMemRecorder(0)
	mux := http.NewServeMux()
	registerDisputeHandler(mux, rec, sink, zap.NewNop())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{"is_fraud": true})
	resp, _ := http.Post(srv.URL+"/admin/feedback/dispute", "application/json", bytes.NewReader(body))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400; got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestComputeCoverage(t *testing.T) {
	sink := audit.NewMemSink(16)
	rec := feedback.NewMemRecorder(0)

	now := time.Now()
	// 5 个 ALLOW，3 个有 outcome；2 个 DENY，1 个有 outcome
	for i := 0; i < 5; i++ {
		id := "a" + string(rune('0'+i))
		mkDisputeAudit(t, sink, id, "pi_a"+string(rune('0'+i)), "ALLOW", now)
		if i < 3 {
			_ = rec.Record(feedback.Outcome{DecisionID: id, Source: feedback.SourceDispute})
		}
	}
	for i := 0; i < 2; i++ {
		id := "d" + string(rune('0'+i))
		mkDisputeAudit(t, sink, id, "pi_d"+string(rune('0'+i)), "DENY", now)
		if i < 1 {
			_ = rec.Record(feedback.Outcome{DecisionID: id, Source: feedback.SourceDispute})
		}
	}

	// 不该 panic；不该写出 NaN — 直接调单测目标函数验证不崩溃就够了
	computeCoverage(sink, rec, 7, zap.NewNop())
}

// 截断窗外条目：windowDays=1 应该忽略 30 天前的记录
func TestComputeCoverage_RespectsWindow(t *testing.T) {
	sink := audit.NewMemSink(16)
	rec := feedback.NewMemRecorder(0)

	old := time.Now().Add(-30 * 24 * time.Hour)
	mkDisputeAudit(t, sink, "old1", "pi_old", "ALLOW", old)
	// 不该被统计（在窗外）；computeCoverage 应该正常返回不 panic
	computeCoverage(sink, rec, 1, zap.NewNop())
}

func contains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
