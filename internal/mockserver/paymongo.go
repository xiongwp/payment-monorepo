package mockserver

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

func (s *Server) registerPayMongo() {
	s.mux.HandleFunc("/v1/sources", s.paymongoCreateSource)
	s.mux.HandleFunc("/v1/sources/", s.paymongoGetSource)
	s.mux.HandleFunc("/v1/refunds", s.paymongoRefund)
}

type pmSourceReq struct {
	Data struct {
		Attributes struct {
			Type     string            `json:"type"`
			Amount   int64             `json:"amount"`
			Currency string            `json:"currency"`
			Redirect map[string]string `json:"redirect"`
			Metadata map[string]string `json:"metadata"`
		} `json:"attributes"`
	} `json:"data"`
}

func (s *Server) paymongoCreateSource(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	var req pmSourceReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	idem := r.Header.Get("Idempotency-Key")
	if idem == "" {
		idem = randomRef("idem_")
	}
	if existing, ok := s.store.loadByReq("paymongo", idem); ok {
		writeJSON(w, pmSourceEnvelope(existing))
		return
	}
	scn := pickScenario(r.Header.Get("X-Mock-Scenario"), req.Data.Attributes.Amount)
	p := &Payment{
		Channel:   "paymongo",
		PaymentID: randomRef("src_"),
		RequestID: idem,
		Amount:    req.Data.Attributes.Amount,
		Currency:  req.Data.Attributes.Currency,
		Scenario:  scn,
		Status:    pmStatus(scn),
		CreatedAt: time.Now().UTC(),
		Extra: map[string]string{
			"type":         req.Data.Attributes.Type,
			"redirect_ok":  req.Data.Attributes.Redirect["success"],
			"redirect_fail": req.Data.Attributes.Redirect["failed"],
		},
	}
	s.store.save(p)
	writeJSON(w, pmSourceEnvelope(p))
}

func pmStatus(scn Scenario) string {
	switch scn {
	case ScenarioSuccess:
		return "chargeable"
	case ScenarioRequiresAction, ScenarioAsyncSuccess, ScenarioAsyncFail:
		return "pending"
	default:
		return "failed"
	}
}

func pmSourceEnvelope(p *Payment) map[string]any {
	attrs := map[string]any{
		"type":     p.Extra["type"],
		"status":   p.Status,
		"amount":   p.Amount,
		"currency": p.Currency,
		"redirect": map[string]any{
			"checkout_url": "https://sandbox.paymongo.com/redirect/" + p.PaymentID,
			"success":      p.Extra["redirect_ok"],
			"failed":       p.Extra["redirect_fail"],
		},
	}
	if p.Status == "failed" {
		attrs["last_payment_error"] = map[string]any{
			"code":    "payment_failed",
			"message": "scenario=" + p.Scenario.String(),
		}
	}
	return map[string]any{
		"data": map[string]any{
			"id":         p.PaymentID,
			"type":       "source",
			"attributes": attrs,
		},
	}
}

func (s *Server) paymongoGetSource(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/sources/")
	p, ok := s.store.loadByPay("paymongo", id)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, pmSourceEnvelope(p))
}

func (s *Server) paymongoRefund(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Data struct {
			Attributes struct {
				Amount    int64  `json:"amount"`
				PaymentID string `json:"payment_id"`
				Reason    string `json:"reason"`
			} `json:"attributes"`
		} `json:"data"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	p, ok := s.store.loadByPay("paymongo", req.Data.Attributes.PaymentID)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if p.RefundedAmount+req.Data.Attributes.Amount > p.Amount {
		http.Error(w, `{"error":"exceeded"}`, http.StatusUnprocessableEntity)
		return
	}
	s.store.update("paymongo", req.Data.Attributes.PaymentID, func(q *Payment) {
		q.RefundedAmount += req.Data.Attributes.Amount
	})
	writeJSON(w, map[string]any{
		"data": map[string]any{
			"id":   randomRef("rfd_"),
			"type": "refund",
			"attributes": map[string]any{
				"amount":     req.Data.Attributes.Amount,
				"status":     "succeeded",
				"payment_id": req.Data.Attributes.PaymentID,
			},
		},
	})
}
