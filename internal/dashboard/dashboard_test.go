package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"github.com/xiongwp/risk-manage/internal/audit"
)

func TestSummary_AllSourcesPresent(t *testing.T) {
	mux := http.NewServeMux()
	RegisterHandler(mux, Sources{
		RuleCount: func() int { return 80 },
		GetQueueCounts: func() QueueCounts {
			return QueueCounts{Pending: 5, InReview: 2, Overdue: 1}
		},
		ChampionModel:    func() string { return "logistic-v1" },
		ChallengerModels: func() []string { return []string{"logistic-v2"} },
		RecentAudits: func(_ int) []*audit.DecisionAudit {
			return []*audit.DecisionAudit{
				{Verdict: "ALLOW"}, {Verdict: "ALLOW"},
				{Verdict: "REVIEW"}, {Verdict: "DENY"},
			}
		},
	}, zap.NewNop())

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/dashboard/summary", nil)
	mux.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var s Summary
	if err := json.Unmarshal(rr.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	if s.RuleCount != 80 {
		t.Errorf("rule_count: %d", s.RuleCount)
	}
	if s.Queue.Pending != 5 || s.Queue.Overdue != 1 {
		t.Errorf("queue: %+v", s.Queue)
	}
	if s.ChampionModel != "logistic-v1" {
		t.Errorf("champion: %s", s.ChampionModel)
	}
	if s.DecisionsByVerdict["ALLOW"] != 2 || s.DecisionsByVerdict["REVIEW"] != 1 {
		t.Errorf("verdict dist: %+v", s.DecisionsByVerdict)
	}
	if s.DecisionsSampleSize != 4 {
		t.Errorf("sample_size: %d", s.DecisionsSampleSize)
	}
}

func TestSummary_NilSourcesAreSafe(t *testing.T) {
	mux := http.NewServeMux()
	RegisterHandler(mux, Sources{}, zap.NewNop())
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/dashboard/summary", nil)
	mux.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("nil sources should produce empty 200, got %d", rr.Code)
	}
}

func TestSummary_OnlyGET(t *testing.T) {
	mux := http.NewServeMux()
	RegisterHandler(mux, Sources{}, zap.NewNop())
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/dashboard/summary", nil)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for POST, got %d", rr.Code)
	}
}
