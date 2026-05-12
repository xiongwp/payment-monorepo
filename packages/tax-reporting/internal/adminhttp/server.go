// Package adminhttp — tax-reporting HTTP API.
//
// 内部:
//   POST /v1/payouts            — 接 clearing/billing 推 PayoutEvent (idempotent)
//   POST /v1/profile            — upsert MerchantTaxProfile (W-9/W-8 收件后)
//   GET  /v1/aggregate/{merchant_id}/{year}/{jurisdiction}
//   POST /v1/forms/1099k/{merchant_id}/{year}   — 生成
//   POST /v1/forms/{form_id}/file               — e-file 提交
//
// Merchant 自助:
//   GET  /v1/me/forms             — 商户拉自己的全部 form (年度税表)
//   GET  /v1/me/forms/{form_id}   — 下载 (返回 pdf_ref)
//
// Admin (X-Admin-Token):
//   GET  /admin/eligible/{year}   — 触发申报阈值的商户列表
//   POST /admin/bulk-generate/{year} — 批量生成 (cron 调用)

package adminhttp

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"reconcile-system/packages/tax-reporting/internal/aggregator"
	"reconcile-system/packages/tax-reporting/internal/domain"
	"reconcile-system/packages/tax-reporting/internal/efile"
	"reconcile-system/packages/tax-reporting/internal/forms"
	"reconcile-system/packages/tax-reporting/internal/metrics"
	"reconcile-system/packages/tax-reporting/internal/store"
)

type Server struct {
	Store      store.Store
	Agg        *aggregator.Aggregator
	Filer      forms.Filer
	Submitter  efile.Submitter
	Thresholds []domain.FilingThreshold
	AdminToken string
	Log        *zap.Logger
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/v1/payouts", s.handlePayouts)
	mux.HandleFunc("/v1/profile", s.handleProfile)
	mux.HandleFunc("/v1/aggregate/", s.handleAggregate)
	mux.HandleFunc("/v1/forms/", s.handleForms)
	mux.HandleFunc("/v1/me/forms", s.handleMeForms)
	mux.HandleFunc("/v1/me/forms/", s.handleMeFormDetail)
	mux.HandleFunc("/admin/eligible/", s.requireAdmin(s.handleEligible))
	mux.HandleFunc("/admin/bulk-generate/", s.requireAdmin(s.handleBulkGenerate))
	return mux
}

// ── /v1/payouts ──

func (s *Server) handlePayouts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST only")
		return
	}
	var e domain.PayoutEvent
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if e.EventID == "" || e.MerchantID == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "event_id and merchant_id required")
		return
	}
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now().UTC()
	}
	if e.TxnCount == 0 {
		e.TxnCount = 1
	}
	if err := s.Store.AppendPayout(e); err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if err := s.Agg.Incremental(e); err != nil {
		s.Log.Warn("aggregate failed", zap.Error(err))
	}
	metrics.PayoutsIngested.WithLabelValues(e.Jurisdiction, e.Source).Inc()
	writeJSON(w, http.StatusOK, map[string]string{"status": "accepted", "event_id": e.EventID})
}

// ── /v1/profile ──

func (s *Server) handleProfile(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		mid := r.URL.Query().Get("merchant_id")
		p, err := s.Store.GetProfile(mid)
		if err != nil {
			writeErr(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		// 不返 TIN 明文
		p.TIN = ""
		writeJSON(w, http.StatusOK, p)
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET/POST")
		return
	}
	var req struct {
		MerchantID  string          `json:"merchant_id"`
		LegalName   string          `json:"legal_name"`
		Country     string          `json:"country"`
		BusinessAddr domain.Address `json:"business_address"`
		TaxClass    domain.TaxClass `json:"tax_class"`
		W9          *struct {
			TIN     string         `json:"tin"`
			TINType domain.TINType `json:"tin_type"`
		} `json:"w9,omitempty"`
		W8          *struct {
			W8Type string `json:"w8_type"` // W-8BEN / W-8BEN-E
		} `json:"w8,omitempty"`
		VATNumber string `json:"vat_number,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if req.MerchantID == "" || req.LegalName == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "merchant_id + legal_name required")
		return
	}
	p, _ := s.Store.GetProfile(req.MerchantID)
	p.MerchantID = req.MerchantID
	p.LegalName = req.LegalName
	p.Country = req.Country
	p.BusinessAddr = req.BusinessAddr
	if req.TaxClass != "" {
		p.TaxClass = req.TaxClass
	}
	p.VAT_Number = req.VATNumber

	if req.W9 != nil {
		if err := forms.W9Submit(&p, req.W9.TIN, req.W9.TINType); err != nil {
			writeErr(w, http.StatusBadRequest, "w9_invalid", err.Error())
			return
		}
	}
	if req.W8 != nil {
		if err := forms.W8Submit(&p, req.W8.W8Type, req.Country); err != nil {
			writeErr(w, http.StatusBadRequest, "w8_invalid", err.Error())
			return
		}
	}
	if err := s.Store.UpsertProfile(p); err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":       "ok",
		"merchant_id":  p.MerchantID,
		"w9_submitted": p.W9_Submitted,
		"w8_submitted": p.W8_Submitted,
	})
}

// ── /v1/aggregate/{mid}/{year}/{jurisdiction} ──

func (s *Server) handleAggregate(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/aggregate/"), "/")
	if len(parts) != 3 {
		writeErr(w, http.StatusBadRequest, "bad_path", "expect /v1/aggregate/{merchant_id}/{year}/{jurisdiction}")
		return
	}
	mid := parts[0]
	year, _ := strconv.Atoi(parts[1])
	juris := parts[2]
	agg, err := s.Store.GetAggregate(mid, year, juris)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, agg)
}

// ── /v1/forms/* ──

func (s *Server) handleForms(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v1/forms/")
	// 两个路由:
	//   POST /v1/forms/1099k/{merchant_id}/{year}
	//   POST /v1/forms/{form_id}/file
	if strings.HasPrefix(path, "1099k/") && r.Method == http.MethodPost {
		s.generate1099K(w, r, strings.TrimPrefix(path, "1099k/"))
		return
	}
	if strings.HasSuffix(path, "/file") && r.Method == http.MethodPost {
		formID := strings.TrimSuffix(path, "/file")
		s.fileForm(w, r, formID)
		return
	}
	if r.Method == http.MethodGet {
		f, err := s.Store.GetForm(path)
		if err != nil {
			writeErr(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, f)
		return
	}
	writeErr(w, http.StatusBadRequest, "bad_path", path)
}

func (s *Server) generate1099K(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.Split(rest, "/")
	if len(parts) != 2 {
		writeErr(w, http.StatusBadRequest, "bad_path", "expect /v1/forms/1099k/{mid}/{year}")
		return
	}
	mid := parts[0]
	year, _ := strconv.Atoi(parts[1])
	profile, err := s.Store.GetProfile(mid)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "no_profile", "merchant must submit W-9 first")
		return
	}
	if !profile.W9_Submitted {
		writeErr(w, http.StatusBadRequest, "w9_required", "1099-K requires W-9 on file")
		return
	}
	agg, err := s.Store.GetAggregate(mid, year, "US")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "no_aggregate", "no US payouts for "+strconv.Itoa(year))
		return
	}
	if !aggregator.EligibleFor(agg, domain.Form1099K, s.Thresholds) {
		writeErr(w, http.StatusBadRequest, "below_threshold", "gross below 1099-K threshold")
		return
	}
	form := forms.Generate1099K(s.Filer, profile, agg)
	if err := s.Store.SaveForm(form); err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	metrics.FormsGenerated.WithLabelValues(string(domain.Form1099K)).Inc()
	writeJSON(w, http.StatusOK, form)
}

func (s *Server) fileForm(w http.ResponseWriter, r *http.Request, formID string) {
	f, err := s.Store.GetForm(formID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	res, err := s.Submitter.Submit(r.Context(), f)
	if err != nil {
		metrics.FormsFiled.WithLabelValues(string(f.FormType), "error").Inc()
		writeErr(w, http.StatusInternalServerError, "submit_failed", err.Error())
		return
	}
	f.Status = domain.FormFiled
	f.FiledAt = res.SubmittedAt
	f.EFileID = res.EFileID
	_ = s.Store.SaveForm(f)
	metrics.FormsFiled.WithLabelValues(string(f.FormType), "ok").Inc()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"form_id":   f.FormID,
		"efile_id":  res.EFileID,
		"receipt":   res.Receipt,
		"filed_at":  res.SubmittedAt,
	})
}

// ── /v1/me/* (merchant self-service) ──

func (s *Server) handleMeForms(w http.ResponseWriter, r *http.Request) {
	mid := r.Header.Get("X-Merchant-Id") // 真生产: oauth2 jwt 解析
	if mid == "" {
		mid = r.URL.Query().Get("merchant_id")
	}
	if mid == "" {
		writeErr(w, http.StatusBadRequest, "no_merchant", "X-Merchant-Id header required")
		return
	}
	list, err := s.Store.ListFormsByMerchant(mid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	// 不返 sensitive payload (TIN etc)
	out := make([]map[string]interface{}, 0, len(list))
	for _, f := range list {
		out = append(out, map[string]interface{}{
			"form_id":      f.FormID,
			"form_type":    f.FormType,
			"year":         f.Year,
			"jurisdiction": f.Jurisdiction,
			"gross_amount": f.GrossAmount,
			"currency":     f.Currency,
			"status":       f.Status,
			"generated_at": f.GeneratedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleMeFormDetail(w http.ResponseWriter, r *http.Request) {
	mid := r.Header.Get("X-Merchant-Id")
	if mid == "" {
		mid = r.URL.Query().Get("merchant_id")
	}
	formID := strings.TrimPrefix(r.URL.Path, "/v1/me/forms/")
	f, err := s.Store.GetForm(formID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if mid != "" && f.MerchantID != mid {
		writeErr(w, http.StatusForbidden, "forbidden", "form not owned by this merchant")
		return
	}
	writeJSON(w, http.StatusOK, f)
}

// ── admin ──

func (s *Server) handleEligible(w http.ResponseWriter, r *http.Request) {
	year, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/admin/eligible/"))
	aggs, _ := s.Store.ListAggregates(year, "")
	eligible := make([]map[string]interface{}, 0)
	for _, a := range aggs {
		if aggregator.EligibleFor(a, domain.Form1099K, s.Thresholds) {
			eligible = append(eligible, map[string]interface{}{
				"merchant_id":  a.MerchantID,
				"jurisdiction": a.Jurisdiction,
				"total_gross":  a.TotalGross,
				"total_count":  a.TotalCount,
			})
		}
	}
	metrics.EligibleMerchants.WithLabelValues(string(domain.Form1099K), strconv.Itoa(year)).Set(float64(len(eligible)))
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"year":     year,
		"count":    len(eligible),
		"eligible": eligible,
	})
}

func (s *Server) handleBulkGenerate(w http.ResponseWriter, r *http.Request) {
	year, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/admin/bulk-generate/"))
	aggs, _ := s.Store.ListAggregates(year, "US")
	generated := 0
	for _, a := range aggs {
		if !aggregator.EligibleFor(a, domain.Form1099K, s.Thresholds) {
			continue
		}
		profile, err := s.Store.GetProfile(a.MerchantID)
		if err != nil || !profile.W9_Submitted {
			continue
		}
		form := forms.Generate1099K(s.Filer, profile, a)
		_ = s.Store.SaveForm(form)
		generated++
		metrics.FormsGenerated.WithLabelValues(string(domain.Form1099K)).Inc()
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"year":       year,
		"generated":  generated,
	})
}

// ── middleware ──

func (s *Server) requireAdmin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.AdminToken == "" {
			h(w, r)
			return
		}
		if r.Header.Get("X-Admin-Token") != s.AdminToken {
			writeErr(w, http.StatusForbidden, "forbidden", "admin token required")
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, errCode, msg string) {
	writeJSON(w, code, map[string]string{"error": errCode, "message": msg})
}

var _ = context.Background
