// Package adminhttp — 商户后台 REST API + 内部 ops 端点。
//
// 路由清单:
//
//	商户:
//	  POST /api/v1/fee/calc                 dry-run 算 fee（不落库）
//	  GET  /api/v1/statements?merchant_id=  列商户账单
//	  GET  /api/v1/statements/<id>          单期账单详情
//	  GET  /api/v1/statements/<id>/events   账单明细 fee_event 列表
//	  GET  /api/v1/statements/<id>/pdf      下载账单 PDF
//
//	ops:
//	  POST /api/v1/fee/events               外部直接喂事件（test / backfill 用）
//	  POST /api/v1/aggregate                即时触发某 period 聚合
//	  POST /api/v1/rules                    新增 fee_rule（无 UI，纯 API）
//	  GET  /api/v1/rules                    列所有 rule

package adminhttp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"reconcile-system/packages/billing-system/internal/domain"
	"reconcile-system/packages/billing-system/internal/feecalc"
	"reconcile-system/packages/billing-system/internal/feerule"
	"reconcile-system/packages/billing-system/internal/repository"
	"reconcile-system/packages/billing-system/internal/statement"
)

// Server 主对外 HTTP server。
type Server struct {
	repo       *repository.MemoryRepo // dev / mvp 直接用 memory；生产换 MySQLRepo
	feecalcSvc *feecalc.Service
	aggregator *statement.Aggregator
	engine     *feerule.Engine
	rules      []domain.FeeRule
	log        *zap.Logger
}

// New 构造。
func New(repo *repository.MemoryRepo, feecalcSvc *feecalc.Service,
	aggregator *statement.Aggregator, rules []domain.FeeRule, log *zap.Logger) *Server {
	return &Server{
		repo:       repo,
		feecalcSvc: feecalcSvc,
		aggregator: aggregator,
		engine:     feerule.New(rules),
		rules:      rules,
		log:        log,
	}
}

// Mount 注册路由。
func (s *Server) Mount(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/fee/calc", s.feeCalc)
	mux.HandleFunc("/api/v1/fee/events", s.feeEventsIngest)
	mux.HandleFunc("/api/v1/statements", s.statementsList)
	mux.HandleFunc("/api/v1/statements/", s.statementByID)
	mux.HandleFunc("/api/v1/aggregate", s.aggregateNow)
	mux.HandleFunc("/api/v1/rules", s.rulesHandler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
}

// feeCalc POST /api/v1/fee/calc — dry-run（不落库）算 fee 给 caller 预览。
func (s *Server) feeCalc(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var in domain.TransactionInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if in.OccurredAt.IsZero() {
		in.OccurredAt = time.Now().UTC()
	}
	rule := s.engine.Pick(in.OccurredAt, in)
	fee, fxMarkup, err := feerule.Compute(rule, in, "USD")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	resp := map[string]any{
		"input":            in,
		"fee_minor":        fee,
		"fx_markup_minor":  fxMarkup,
		"total_fee_minor":  fee + fxMarkup,
	}
	if rule != nil {
		resp["rule_id"] = rule.ID
		resp["rule_name"] = rule.Name
		resp["rule_percent_bps"] = rule.PercentBPS
		resp["rule_fixed_minor"] = rule.FixedMinor
	} else {
		resp["rule_id"] = 0
		resp["rule_name"] = "no_match"
	}
	writeJSON(w, http.StatusOK, resp)
}

// feeEventsIngest POST /api/v1/fee/events — 喂事件落 fee_event（test / backfill）。
func (s *Server) feeEventsIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var in domain.TransactionInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	ev, err := s.feecalcSvc.Process(ctx, in)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if ev == nil {
		writeJSON(w, http.StatusOK, map[string]any{"idempotent": true, "skipped": true})
		return
	}
	writeJSON(w, http.StatusCreated, ev)
}

// statementsList GET /api/v1/statements?merchant_id=...&limit=...
func (s *Server) statementsList(w http.ResponseWriter, r *http.Request) {
	mid := r.URL.Query().Get("merchant_id")
	if mid == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("merchant_id required"))
		return
	}
	limit := 12
	if v := r.URL.Query().Get("limit"); v != "" {
		fmt.Sscanf(v, "%d", &limit)
	}
	stmts, err := s.repo.ListStatementsByMerchant(r.Context(), mid, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"statements": stmts})
}

// statementByID GET /api/v1/statements/<id> | /events | /pdf
func (s *Server) statementByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/statements/")
	parts := strings.SplitN(rest, "/", 2)
	idStr := parts[0]
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid id"))
		return
	}
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	switch action {
	case "", "":
		stmt, err := s.repo.GetStatement(r.Context(), id)
		if err != nil || stmt == nil {
			writeErr(w, http.StatusNotFound, fmt.Errorf("statement not found"))
			return
		}
		writeJSON(w, http.StatusOK, stmt)
	case "events":
		evs, err := s.repo.ListStatementEvents(r.Context(), id)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"events": evs, "count": len(evs)})
	case "pdf":
		// MVP：返简单 HTML / text 占位。生产换 pdf/render.go
		stmt, err := s.repo.GetStatement(r.Context(), id)
		if err != nil || stmt == nil {
			writeErr(w, http.StatusNotFound, fmt.Errorf("statement not found"))
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "STATEMENT #%d\n", stmt.ID)
		fmt.Fprintf(w, "Merchant: %s\n", stmt.MerchantID)
		fmt.Fprintf(w, "Period:   %s - %s\n", stmt.PeriodStart.Format("2006-01-02"), stmt.PeriodEnd.Format("2006-01-02"))
		fmt.Fprintf(w, "Currency: %s\n", stmt.Currency)
		fmt.Fprintf(w, "Events:   %d\n", stmt.EventCount)
		fmt.Fprintf(w, "Gross:    %d %s\n", stmt.TotalGrossMinor, stmt.Currency)
		fmt.Fprintf(w, "Fee:      %d %s\n", stmt.TotalFeeMinor, stmt.Currency)
		fmt.Fprintf(w, "Refund:   %d %s\n", stmt.TotalRefundMinor, stmt.Currency)
		fmt.Fprintf(w, "Chargeback: %d %s\n", stmt.TotalChargebackMinor, stmt.Currency)
		fmt.Fprintf(w, "Net Payout: %d %s\n", stmt.NetPayoutMinor, stmt.Currency)
		fmt.Fprintf(w, "Status:   %s\n", stmt.Status)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// aggregateNow POST /api/v1/aggregate body={from, to, final}
func (s *Server) aggregateNow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		From  string `json:"from"`
		To    string `json:"to"`
		Final bool   `json:"final"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	from, err := time.Parse(time.RFC3339, body.From)
	if err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid from: %w", err))
		return
	}
	to, err := time.Parse(time.RFC3339, body.To)
	if err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid to: %w", err))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	n, err := s.aggregator.RunPeriod(ctx, from, to, body.Final)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"statements_created": n,
		"from":               from.Format(time.RFC3339),
		"to":                 to.Format(time.RFC3339),
		"final":              body.Final,
	})
}

// rulesHandler GET/POST /api/v1/rules
func (s *Server) rulesHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"rules": s.rules})
	case http.MethodPost:
		var nr domain.FeeRule
		if err := json.NewDecoder(r.Body).Decode(&nr); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		nr.ID = int64(len(s.rules) + 1)
		nr.CreatedAt = time.Now().UTC()
		nr.UpdatedAt = nr.CreatedAt
		if !nr.Active {
			nr.Active = true
		}
		s.rules = append(s.rules, nr)
		s.engine = feerule.New(s.rules) // 重建以保 priority 排序
		writeJSON(w, http.StatusCreated, nr)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]any{"error": err.Error()})
}
