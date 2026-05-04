package mockserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func (s *Server) registerDragonpay() {
	s.mux.HandleFunc("/api/collect/v1/", s.dragonpayCollect)
	s.mux.HandleFunc("/api/refund/v1/post", s.dragonpayRefund)
}

// dragonpayCollect routes both POST /api/collect/v1/{txnid}/post (charge) and
// GET /api/collect/v1/{txnid} (query).
func (s *Server) dragonpayCollect(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/collect/v1/")
	seg := strings.Split(path, "/")
	if len(seg) == 0 || seg[0] == "" {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	txnid, _ := url.PathUnescape(seg[0])

	if r.Method == http.MethodPost && len(seg) >= 2 && seg[1] == "post" {
		s.dragonpayPost(w, r, txnid)
		return
	}
	if r.Method == http.MethodGet {
		s.dragonpayQuery(w, txnid)
		return
	}
	http.Error(w, "method", http.StatusMethodNotAllowed)
}

func (s *Server) dragonpayPost(w http.ResponseWriter, r *http.Request, txnid string) {
	var req struct {
		Amount      string `json:"Amount"`
		Currency    string `json:"Currency"`
		Description string `json:"Description"`
		ProcID      string `json:"ProcId"`
		Param1      string `json:"Param1"` // piID
		Param2      string `json:"Param2"` // notify_url
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	amtFloat, _ := strconv.ParseFloat(req.Amount, 64)
	amt := int64(amtFloat * 100)
	if existing, ok := s.store.loadByReq("dragonpay", txnid); ok {
		writeJSON(w, dragonpayRespFor(existing))
		return
	}
	scn := pickScenario(r.Header.Get("X-Mock-Scenario"), amt)
	p := &Payment{
		Channel:   "dragonpay",
		PaymentID: randomRef("dp_"),
		RequestID: txnid,
		Amount:    amt,
		Currency:  req.Currency,
		Scenario:  scn,
		NotifyURL: req.Param2,
		CreatedAt: time.Now().UTC(),
		Extra: map[string]string{
			"pi_id":   req.Param1,
			"proc_id": req.ProcID,
		},
	}
	switch scn {
	case ScenarioSuccess:
		p.Status = "S"
	case ScenarioRequiresAction, ScenarioAsyncSuccess, ScenarioAsyncFail:
		p.Status = "P"
	default:
		p.Status = "F"
		p.FailureCode = "card_declined"
	}
	s.store.save(p)
	writeJSON(w, dragonpayRespFor(p))

	if scn == ScenarioAsyncSuccess || scn == ScenarioAsyncFail {
		go s.fireDragonpayWebhook(p, scn == ScenarioAsyncSuccess)
	}
}

func dragonpayRespFor(p *Payment) map[string]any {
	out := map[string]any{
		"Status":  p.Status,
		"RefNo":   p.PaymentID,
		"Message": "",
	}
	if p.Status == "P" {
		out["Url"] = fmt.Sprintf("https://test.dragonpay.ph/Pay.aspx?ref=%s", p.PaymentID)
	}
	if p.Status == "F" {
		out["Message"] = p.FailureCode
	}
	return out
}

func (s *Server) dragonpayQuery(w http.ResponseWriter, txnid string) {
	p, ok := s.store.loadByReq("dragonpay", txnid)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{
		"Status": p.Status,
		"RefNo":  p.PaymentID,
	})
}

func (s *Server) dragonpayRefund(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TxnID  string `json:"TxnId"`
		Amount string `json:"Amount"`
		Reason string `json:"Reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	p, ok := s.store.loadByReq("dragonpay", req.TxnID)
	if !ok {
		p, ok = s.store.loadByPay("dragonpay", req.TxnID)
	}
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	amtF, _ := strconv.ParseFloat(req.Amount, 64)
	amt := int64(amtF * 100)
	if p.RefundedAmount+amt > p.Amount {
		writeJSON(w, map[string]any{"Status": "F", "Message": "amount exceeded"})
		return
	}
	s.store.update("dragonpay", p.PaymentID, func(q *Payment) { q.RefundedAmount += amt })
	writeJSON(w, map[string]any{"Status": "S", "RefNo": randomRef("dp_r_")})
}

func (s *Server) fireDragonpayWebhook(p *Payment, success bool) {
	status := "S"
	if !success {
		status = "F"
	}
	s.store.update("dragonpay", p.PaymentID, func(q *Payment) { q.Status = status })
	// Dragonpay posts form-encoded fields; we emit JSON because the adapter's
	// webhook parser is lenient. (Real Dragonpay → form; we can extend later.)
	body := map[string]any{
		"txnid":   p.RequestID,
		"refno":   p.PaymentID,
		"status":  status,
		"message": "scenario " + p.Scenario.String(),
	}
	s.disp.fire(withDispatcherCtx(), p.NotifyURL, body, nil)
}
