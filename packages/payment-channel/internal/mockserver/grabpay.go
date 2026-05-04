package mockserver

import (
	"encoding/json"
	"net/http"
	"time"
)

func (s *Server) registerGrabPay() {
	s.mux.HandleFunc("/grabpay/partner/v2/charge/init", s.grabChargeInit)
	s.mux.HandleFunc("/grabpay/partner/v2/refund", s.grabRefund)
	s.mux.HandleFunc("/grabpay/partner/v2/charge/status", s.grabQuery)
}

type grabInitReq struct {
	PartnerTxID string `json:"partnerTxID"`
	Amount      int64  `json:"amount"`
	Currency    string `json:"currency"`
	Description string `json:"description"`
	MerchantID  string `json:"merchantID"`
}

func (s *Server) grabChargeInit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	var req grabInitReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if existing, ok := s.store.loadByReq("grabpay", req.PartnerTxID); ok {
		writeJSON(w, grabInitResp(existing))
		return
	}
	scn := pickScenario(r.Header.Get("X-Mock-Scenario"), req.Amount)
	p := &Payment{
		Channel:   "grabpay",
		PaymentID: randomRef("grb_"),
		RequestID: req.PartnerTxID,
		Amount:    req.Amount,
		Currency:  req.Currency,
		Scenario:  scn,
		Status:    grabStatus(scn),
		CreatedAt: time.Now().UTC(),
	}
	s.store.save(p)
	writeJSON(w, grabInitResp(p))
}

func grabStatus(scn Scenario) string {
	switch scn {
	case ScenarioSuccess:
		return "success"
	case ScenarioRequiresAction, ScenarioAsyncSuccess, ScenarioAsyncFail:
		return "pending"
	default:
		return "failed"
	}
}

func grabInitResp(p *Payment) map[string]any {
	return map[string]any{
		"txID":     p.PaymentID,
		"status":   p.Status,
		"code":     "",
		"redirect": "https://pay-sandbox.grab.com/checkout?id=" + p.PaymentID,
	}
}

func (s *Server) grabRefund(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PartnerTxID    string `json:"partnerTxID"`
		PartnerGroupTx string `json:"partnerGroupTxID"`
		OriginTxID     string `json:"originTxID"`
		Amount         int64  `json:"amount"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	p, ok := s.store.loadByPay("grabpay", req.OriginTxID)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if p.RefundedAmount+req.Amount > p.Amount {
		writeJSON(w, map[string]any{"status": "failed", "code": "refund_exceeded"})
		return
	}
	s.store.update("grabpay", req.OriginTxID, func(q *Payment) { q.RefundedAmount += req.Amount })
	writeJSON(w, map[string]any{"txID": randomRef("grb_r_"), "status": "success"})
}

func (s *Server) grabQuery(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("txID")
	if id == "" {
		id = r.URL.Query().Get("partnerTxID")
	}
	p, ok := s.store.loadByPay("grabpay", id)
	if !ok {
		p, ok = s.store.loadByReq("grabpay", id)
	}
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{
		"txID":   p.PaymentID,
		"status": p.Status,
		"amount": p.Amount,
	})
}
