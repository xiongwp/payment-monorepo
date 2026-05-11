// dispute-service HTTP server.

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

	"reconcile-system/packages/dispute-service/internal/domain"
	"reconcile-system/packages/dispute-service/internal/repository"
	"reconcile-system/packages/dispute-service/internal/workflow"
)

type Server struct {
	svc  *workflow.Service
	repo *repository.MemoryRepo
	log  *zap.Logger
}

func New(svc *workflow.Service, repo *repository.MemoryRepo, log *zap.Logger) *Server {
	return &Server{svc: svc, repo: repo, log: log}
}

func (s *Server) Mount(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/disputes/webhook", s.networkWebhook)
	mux.HandleFunc("/api/v1/disputes", s.listDisputes)
	mux.HandleFunc("/api/v1/disputes/", s.disputeByID)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
}

// networkWebhook POST /api/v1/disputes/webhook — 卡组 chargeback notification 入口。
func (s *Server) networkWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var d domain.Dispute
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	out, err := s.svc.IngestNetworkWebhook(ctx, d)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// listDisputes GET /api/v1/disputes?merchant_id=&status=&limit=
func (s *Server) listDisputes(w http.ResponseWriter, r *http.Request) {
	mid := r.URL.Query().Get("merchant_id")
	if mid == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("merchant_id required"))
		return
	}
	status := domain.DisputeStatus(r.URL.Query().Get("status"))
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		fmt.Sscanf(v, "%d", &limit)
	}
	out, err := s.repo.ListByMerchant(r.Context(), mid, status, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"disputes": out})
}

// disputeByID /api/v1/disputes/<id>[/evidence|/finalize|/ruling]
func (s *Server) disputeByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/disputes/")
	parts := strings.SplitN(rest, "/", 2)
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid id"))
		return
	}
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	switch {
	case action == "" && r.Method == http.MethodGet:
		d, _ := s.repo.Get(r.Context(), id)
		if d == nil {
			writeErr(w, http.StatusNotFound, fmt.Errorf("not found"))
			return
		}
		writeJSON(w, http.StatusOK, d)
	case action == "evidence" && r.Method == http.MethodPost:
		var ev domain.Evidence
		if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		eid, err := s.svc.SubmitEvidence(r.Context(), id, ev)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"evidence_id": eid})
	case action == "evidence" && r.Method == http.MethodGet:
		evs, _ := s.repo.ListEvidence(r.Context(), id)
		writeJSON(w, http.StatusOK, map[string]any{"evidence": evs})
	case action == "finalize" && r.Method == http.MethodPost:
		by := r.Header.Get("X-Merchant-User")
		if by == "" {
			by = "merchant"
		}
		if err := s.svc.FinalizeMerchantResponse(r.Context(), id, by); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"finalized": true})
	case action == "ruling" && r.Method == http.MethodPost:
		var body struct {
			Outcome string `json:"outcome"`
			Details string `json:"details"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		d, _ := s.repo.Get(r.Context(), id)
		if d == nil {
			writeErr(w, http.StatusNotFound, fmt.Errorf("dispute not found"))
			return
		}
		if err := s.svc.IngestRuling(r.Context(), d.ExternalID, body.Outcome, body.Details); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ruled": body.Outcome})
	default:
		http.Error(w, "not found", http.StatusNotFound)
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
