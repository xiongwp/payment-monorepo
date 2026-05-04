package mockserver

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

func (s *Server) registerXendit() {
	s.mux.HandleFunc("/v2/invoices", s.xenditCreateInvoice)
	s.mux.HandleFunc("/v2/invoices/", s.xenditGetInvoice)
	s.mux.HandleFunc("/refunds", s.xenditRefund)
}

type xenditInvoiceReq struct {
	ExternalID  string `json:"external_id"`
	Amount      int64  `json:"amount"`
	Currency    string `json:"currency"`
	Description string `json:"description"`
	SuccessURL  string `json:"success_redirect_url"`
	FailureURL  string `json:"failure_redirect_url"`
}

func (s *Server) xenditCreateInvoice(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	var req xenditInvoiceReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if existing, ok := s.store.loadByReq("xendit", req.ExternalID); ok {
		writeJSON(w, xenditInvoiceEnvelope(existing))
		return
	}
	scn := pickScenario(r.Header.Get("X-Mock-Scenario"), req.Amount)
	p := &Payment{
		Channel:   "xendit",
		PaymentID: randomRef("inv_"),
		RequestID: req.ExternalID,
		Amount:    req.Amount,
		Currency:  req.Currency,
		Scenario:  scn,
		Status:    xenditStatus(scn),
		CreatedAt: time.Now().UTC(),
		Extra: map[string]string{
			"success_url": req.SuccessURL,
			"failure_url": req.FailureURL,
		},
	}
	s.store.save(p)
	writeJSON(w, xenditInvoiceEnvelope(p))
}

func xenditStatus(scn Scenario) string {
	switch scn {
	case ScenarioSuccess:
		return "PAID"
	case ScenarioRequiresAction, ScenarioAsyncSuccess, ScenarioAsyncFail:
		return "PENDING"
	default:
		return "EXPIRED"
	}
}

func xenditInvoiceEnvelope(p *Payment) map[string]any {
	return map[string]any{
		"id":          p.PaymentID,
		"external_id": p.RequestID,
		"status":      p.Status,
		"amount":      p.Amount,
		"currency":    p.Currency,
		"invoice_url": "https://checkout-sandbox.xendit.co/web/" + p.PaymentID,
	}
}

func (s *Server) xenditGetInvoice(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v2/invoices/")
	p, ok := s.store.loadByPay("xendit", id)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, xenditInvoiceEnvelope(p))
}

func (s *Server) xenditRefund(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		InvoiceID string `json:"invoice_id"`
		Amount    int64  `json:"amount"`
		Reason    string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	p, ok := s.store.loadByPay("xendit", req.InvoiceID)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if p.RefundedAmount+req.Amount > p.Amount {
		http.Error(w, `{"error":"exceeded"}`, http.StatusUnprocessableEntity)
		return
	}
	s.store.update("xendit", req.InvoiceID, func(q *Payment) { q.RefundedAmount += req.Amount })
	writeJSON(w, map[string]any{"id": randomRef("rfd_"), "status": "SUCCEEDED", "amount": req.Amount})
}
