// Package adminhttp — Money Flow Graph CRUD + dry-run HTTP API.
//
// Endpoints:
//   GET    /api/moneyflow/graphs              列所有 graph (admin SPA)
//   GET    /api/moneyflow/graphs/{key}        拿单个
//   POST   /api/moneyflow/graphs              创建/更新 (key 不变升版本)
//   DELETE /api/moneyflow/graphs/{key}        归档 (status=archived, 不真删)
//   POST   /api/moneyflow/dry-run             {graph, event} → 返预演的 RunPlan, 不下账
//   POST   /api/moneyflow/runs/search         查执行历史
//
// 跟前端 SPA (moneyflow-designer.html) 配对。

package adminhttp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"reconcile-system/packages/split-payment/internal/domain"
	"reconcile-system/packages/split-payment/internal/workflow"

	"go.uber.org/zap"
)

// GraphRepo adminhttp 抽象 (MemoryGraphRepo / MySQLGraphRepo 都满足).
type GraphRepo interface {
	Save(ctx context.Context, g *domain.Graph) (int64, error)
	GetByKey(ctx context.Context, key string) (*domain.Graph, error)
	List(ctx context.Context, status string) ([]*domain.Graph, error)
}

// RunRepo adminhttp 抽象.
type RunRepo interface {
	Search(ctx context.Context, eventLike string, limit int) ([]*domain.RunPlan, error)
}

// Server 注册路由。
type Server struct {
	Graphs GraphRepo
	Runs   RunRepo
	Log    *zap.Logger
}

// Register 把 endpoint 挂到 mux。
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/moneyflow/graphs", s.handleGraphs)
	mux.HandleFunc("/api/moneyflow/graphs/", s.handleGraphByKey)
	mux.HandleFunc("/api/moneyflow/dry-run", s.handleDryRun)
	mux.HandleFunc("/api/moneyflow/runs/search", s.handleRunsSearch)
}

// ─── /graphs (list, create/update) ─────────────────────────────────────

func (s *Server) handleGraphs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		status := r.URL.Query().Get("status")
		if status == "" {
			status = "all"
		}
		list, err := s.Graphs.List(r.Context(), status)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": list})
	case http.MethodPost:
		var g domain.Graph
		if err := json.NewDecoder(r.Body).Decode(&g); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if g.Key == "" {
			writeErr(w, http.StatusBadRequest, errString("graph.key required"))
			return
		}
		if g.Status == "" {
			g.Status = "draft"
		}
		id, err := s.Graphs.Save(r.Context(), &g)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		g.ID = id
		writeJSON(w, http.StatusCreated, g)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// ─── /graphs/{key} (get / archive) ─────────────────────────────────────

func (s *Server) handleGraphByKey(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/api/moneyflow/graphs/")
	if key == "" {
		writeErr(w, http.StatusBadRequest, errString("key required"))
		return
	}
	g, err := s.Graphs.GetByKey(r.Context(), key)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, g)
	case http.MethodDelete:
		g.Status = "archived"
		_, _ = s.Graphs.Save(r.Context(), g)
		writeJSON(w, http.StatusOK, map[string]any{"archived": true})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// ─── /dry-run — 预演 graph (不调 accounting, 不落账) ──────────────────

func (s *Server) handleDryRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Graph domain.Graph              `json:"graph"`
		Event workflow.BusinessEvent    `json:"event"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	tc := workflow.TriggerContext{
		Event: req.Event.Event, ChargeID: req.Event.ChargeID,
		MerchantID: req.Event.MerchantID, AmountMinor: req.Event.AmountMinor,
		Currency: req.Event.Currency, Attributes: req.Event.Attributes,
	}
	plan, err := workflow.Translate(&req.Graph, tc)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": err.Error(),
			"hint":  "translation failed; 检查 placeholder / guard / 金额",
		})
		return
	}
	// 在预演里附 sum 字段方便人工核对
	var sum int64
	skip := 0
	for _, m := range plan.Movements {
		if m.Status == "pending" {
			sum += m.AmountMinor
		} else {
			skip++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"plan":          plan,
		"movements_sum": sum,
		"sum_matches":   sum == req.Event.AmountMinor,
		"skipped":       skip,
	})
}

// ─── /runs/search — 查执行历史 ────────────────────────────────────────

func (s *Server) handleRunsSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	limit := 50
	list, err := s.Runs.Search(r.Context(), q.Get("event"), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": list})
}

// ─── helpers ──────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]any{"error": err.Error()})
}

type errStr string

func (e errStr) Error() string { return string(e) }
func errString(s string) error { return errStr(s) }
