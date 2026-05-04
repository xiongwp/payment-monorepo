package mockserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func (s *Server) registerMaya() {
	s.mux.HandleFunc("/checkout/v1/checkouts", s.mayaCreateCheckout)
	// /checkout/v1/checkouts/{id} - GET
	s.mux.HandleFunc("/checkout/v1/checkouts/", s.mayaGetCheckout)
	// /payments/v1/payments/{id}/refunds
	s.mux.HandleFunc("/payments/v1/payments/", s.mayaPaymentOps)
}

type mayaCheckoutReq struct {
	TotalAmount struct {
		Value    float64 `json:"value"`
		Currency string  `json:"currency"`
	} `json:"totalAmount"`
	RedirectURL            map[string]string `json:"redirectUrl"`
	RequestReferenceNumber string            `json:"requestReferenceNumber"`
	Metadata               map[string]any    `json:"metadata"`
}

func (s *Server) mayaCreateCheckout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	var req mayaCheckoutReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	amt := int64(req.TotalAmount.Value * 100)
	if existing, ok := s.store.loadByReq("maya", req.RequestReferenceNumber); ok {
		writeJSON(w, map[string]any{
			"checkoutId":  existing.PaymentID,
			"redirectUrl": existing.Extra["redirect_url"],
		})
		return
	}
	scn := pickScenario(r.Header.Get("X-Mock-Scenario"), amt)
	p := &Payment{
		Channel:   "maya",
		PaymentID: randomRef("chk_"),
		RequestID: req.RequestReferenceNumber,
		Amount:    amt,
		Currency:  req.TotalAmount.Currency,
		Status:    "CREATED",
		Scenario:  scn,
		CreatedAt: time.Now().UTC(),
		Extra:     map[string]string{},
	}
	// Maya checkout always returns a redirect; scenario applies to later Query.
	redirect := fmt.Sprintf("https://payments-sandbox.paymaya.com/checkout?id=%s&scn=%s",
		p.PaymentID, scn.String())
	p.Extra["redirect_url"] = redirect
	// Seed final status that Query + webhook will see.
	switch scn {
	case ScenarioSuccess, ScenarioAsyncSuccess:
		p.Status = "PAYMENT_SUCCESS"
	case ScenarioFailCardDeclined, ScenarioFailInsufficientFunds,
		ScenarioFailRiskBlocked, ScenarioFailChannelUnavailable, ScenarioAsyncFail:
		p.Status = "PAYMENT_FAILED"
		p.FailureCode = mayaFailureCode(scn)
	case ScenarioRequiresAction:
		p.Status = "CREATED"
	}
	s.store.save(p)
	writeJSON(w, map[string]any{"checkoutId": p.PaymentID, "redirectUrl": redirect})
}

func mayaFailureCode(scn Scenario) string {
	switch scn {
	case ScenarioFailCardDeclined:
		return "PY0007"
	case ScenarioFailInsufficientFunds:
		return "PY0023"
	case ScenarioFailRiskBlocked:
		return "PY0039"
	case ScenarioFailChannelUnavailable:
		return "PY0085"
	}
	return "PY9999"
}

func (s *Server) mayaGetCheckout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/checkout/v1/checkouts/")
	p, ok := s.store.loadByPay("maya", id)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{
		"id":     id,
		"status": p.Status,
		"totalAmount": map[string]any{
			"value":    float64(p.Amount) / 100.0,
			"currency": p.Currency,
		},
	})
}

// mayaPaymentOps handles both /payments/v1/payments/{id}/refunds and /voids.
func (s *Server) mayaPaymentOps(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/payments/v1/payments/"), "/")
	if len(parts) < 2 {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	id, op := parts[0], parts[1]
	p, ok := s.store.loadByPay("maya", id)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	switch op {
	case "refunds":
		var req struct {
			TotalAmount            struct{ Value float64 } `json:"totalAmount"`
			RequestReferenceNumber string                  `json:"requestReferenceNumber"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		amt := int64(req.TotalAmount.Value * 100)
		if p.RefundedAmount+amt > p.Amount {
			http.Error(w, `{"error":"amount exceeded"}`, http.StatusUnprocessableEntity)
			return
		}
		s.store.update("maya", id, func(q *Payment) { q.RefundedAmount += amt })
		writeJSON(w, map[string]any{"id": randomRef("rfd_"), "status": "SUCCESS"})
	case "voids":
		s.store.update("maya", id, func(q *Payment) { q.Status = "VOIDED" })
		writeJSON(w, map[string]any{"id": randomRef("vd_"), "status": "VOIDED"})
	default:
		http.Error(w, "op", http.StatusBadRequest)
	}
}
