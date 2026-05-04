package mockserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// registerSimple wires the other 9 PH channels (coinsph, instapay, pesonet,
// bdo, bpi, metrobank, landbank, shopeepay, billease) with a generic
// handler. These adapters use common patterns (OAuth2 token then a REST
// resource); the mock returns scenario-driven status values without trying
// to mimic every quirk of each vendor's schema.
func (s *Server) registerSimple() {
	// OAuth2 token endpoints — return a bearer regardless of credentials.
	for _, p := range []string{
		"/partners/sb/v1/oauth2/token",
		"/gateway/auth/oauth2/v1/token",
		"/oauth/v1/token",
		"/oauth2/v1/token",
		"/api/v1/oauth/token",
	} {
		s.mux.HandleFunc(p, s.issueToken)
	}

	// CoinsPH
	s.mux.HandleFunc("/pay/api/v3/invoices/", s.simpleCoinsPH)

	// InstaPay / PESONet / banks — transfers + queries. One catch-all.
	s.mux.HandleFunc("/partners/v1/instapay/transfer", s.simpleCreate("instapay"))
	s.mux.HandleFunc("/partners/v1/instapay/transfer/", s.simpleGet("instapay"))
	s.mux.HandleFunc("/partners/v1/instapay/refund", s.simpleRefund("instapay"))

	s.mux.HandleFunc("/partners/v1/pesonet/transfer", s.simpleCreate("pesonet"))
	s.mux.HandleFunc("/partners/v1/pesonet/transfer/", s.simpleGet("pesonet"))

	s.mux.HandleFunc("/gateway/payments/v1/directdebit", s.simpleCreate("bdo"))
	s.mux.HandleFunc("/gateway/payments/v1/directdebit/", s.simpleGet("bdo"))
	s.mux.HandleFunc("/gateway/payments/v1/refund", s.simpleRefund("bdo"))

	s.mux.HandleFunc("/open/v1/fund-transfer", s.simpleCreate("bpi"))
	s.mux.HandleFunc("/open/v1/fund-transfer/", s.simpleGet("bpi"))
	s.mux.HandleFunc("/open/v1/fund-transfer/refund", s.simpleRefund("bpi"))

	s.mux.HandleFunc("/partners/v1/payments/debit", s.simpleCreate("metrobank"))
	s.mux.HandleFunc("/partners/v1/payments/refund", s.simpleRefund("metrobank"))
	s.mux.HandleFunc("/partners/v1/payments/", s.simpleGet("metrobank"))

	s.mux.HandleFunc("/payment/v2/create", s.simpleCreate("landbank"))
	s.mux.HandleFunc("/payment/v2/status/", s.simpleGet("landbank"))
	s.mux.HandleFunc("/payment/v2/refund", s.simpleRefund("landbank"))

	// Shopeepay — all under /v3/merchant-host/...
	s.mux.HandleFunc("/v3/merchant-host/order/create", s.simpleCreate("shopeepay"))
	s.mux.HandleFunc("/v3/merchant-host/order/query", s.simpleQueryByBody("shopeepay"))
	s.mux.HandleFunc("/v3/merchant-host/refund/create", s.simpleRefund("shopeepay"))

	// BillEase
	s.mux.HandleFunc("/api/v2/checkout/create", s.simpleCreate("billease"))
	s.mux.HandleFunc("/api/v2/checkout/", s.billeaseCheckoutOps)
}

func (s *Server) issueToken(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"access_token": "mock_token_" + randomRef(""),
		"token_type":   "Bearer",
		"expires_in":   3600,
	})
}

type simpleCreateBody struct {
	// Tolerant to different schemas; we pull what we can.
	ReferenceID   string  `json:"reference_id"`
	RequestID     string  `json:"request_id"`
	MerchantRef   string  `json:"merchant_reference_id"`
	PartnerRefNo  string  `json:"partner_reference_no"`
	ExternalID    string  `json:"external_id"`
	ClientRef     string  `json:"client_reference_id"`
	Amount        int64   `json:"amount"`
	Value         float64 `json:"value"`
	Currency      string  `json:"currency"`
	CallbackURL   string  `json:"callback_url"`
	NotificationURL string `json:"notification_url"`
	NotifyURL     string  `json:"notify_url"`
}

func (b simpleCreateBody) reqID() string {
	for _, s := range []string{b.ReferenceID, b.RequestID, b.MerchantRef, b.PartnerRefNo, b.ExternalID, b.ClientRef} {
		if s != "" {
			return s
		}
	}
	return ""
}

func (b simpleCreateBody) amt() int64 {
	if b.Amount > 0 {
		return b.Amount
	}
	return int64(b.Value * 100)
}

func (b simpleCreateBody) notifyURL() string {
	for _, s := range []string{b.CallbackURL, b.NotificationURL, b.NotifyURL} {
		if s != "" {
			return s
		}
	}
	return ""
}

func (s *Server) simpleCreate(channelName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body simpleCreateBody
		raw, _ := readAllAndRestore(r)
		_ = json.Unmarshal(raw, &body)

		reqID := body.reqID()
		if reqID == "" {
			reqID = randomRef("req_")
		}
		if existing, ok := s.store.loadByReq(channelName, reqID); ok {
			writeJSON(w, simpleEnvelope(existing))
			return
		}
		scn := pickScenario(r.Header.Get("X-Mock-Scenario"), body.amt())
		p := &Payment{
			Channel:   channelName,
			PaymentID: randomRef(channelName + "_"),
			RequestID: reqID,
			Amount:    body.amt(),
			Currency:  firstNonEmpty(body.Currency, "PHP"),
			Scenario:  scn,
			Status:    simpleStatus(scn),
			NotifyURL: body.notifyURL(),
			CreatedAt: time.Now().UTC(),
		}
		s.store.save(p)
		writeJSON(w, simpleEnvelope(p))
	}
}

func (s *Server) simpleGet(channelName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Path
		if i := strings.LastIndex(id, "/"); i >= 0 {
			id = id[i+1:]
		}
		if id == "" {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		p, ok := s.store.loadByPay(channelName, id)
		if !ok {
			p, ok = s.store.loadByReq(channelName, id)
		}
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, simpleEnvelope(p))
	}
}

func (s *Server) simpleRefund(channelName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			OriginID       string  `json:"origin_id"`
			OriginalRefNo  string  `json:"original_ref_no"`
			PaymentID      string  `json:"payment_id"`
			TransactionID  string  `json:"transaction_id"`
			Amount         int64   `json:"amount"`
			Value          float64 `json:"value"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		id := firstNonEmpty(body.OriginID, body.OriginalRefNo, body.PaymentID, body.TransactionID)
		amt := body.Amount
		if amt == 0 {
			amt = int64(body.Value * 100)
		}
		p, ok := s.store.loadByPay(channelName, id)
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if p.RefundedAmount+amt > p.Amount {
			writeJSON(w, map[string]any{"status": "failed", "code": "refund_exceeded"})
			return
		}
		s.store.update(channelName, id, func(q *Payment) { q.RefundedAmount += amt })
		writeJSON(w, map[string]any{
			"id":     randomRef(channelName + "_r_"),
			"status": "SUCCESS",
			"amount": amt,
		})
	}
}

func (s *Server) simpleQueryByBody(channelName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			MerchantExtID string `json:"merchant_ext_id"`
			OrderID       string `json:"order_id"`
			ReferenceID   string `json:"reference_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		id := firstNonEmpty(body.OrderID, body.ReferenceID)
		p, ok := s.store.loadByPay(channelName, id)
		if !ok {
			p, ok = s.store.loadByReq(channelName, id)
		}
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, simpleEnvelope(p))
	}
}

// billeaseCheckoutOps routes GET /api/v2/checkout/{id} and POST /api/v2/checkout/{id}/refund.
func (s *Server) billeaseCheckoutOps(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v2/checkout/")
	seg := strings.SplitN(path, "/", 2)
	id := seg[0]
	if len(seg) == 2 && seg[1] == "refund" {
		s.simpleRefund("billease").ServeHTTP(w, r)
		return
	}
	p, ok := s.store.loadByPay("billease", id)
	if !ok {
		p, ok = s.store.loadByReq("billease", id)
	}
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, simpleEnvelope(p))
}

// simpleCoinsPH routes POST /pay/api/v3/invoices/ (create), GET /pay/api/v3/invoices/{id} (query),
// POST /pay/api/v3/invoices/{id}/refunds (refund).
func (s *Server) simpleCoinsPH(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/pay/api/v3/invoices/")
	seg := strings.SplitN(path, "/", 2)
	if path == "" || seg[0] == "" {
		if r.Method == http.MethodPost {
			s.simpleCreate("coinsph").ServeHTTP(w, r)
			return
		}
	}
	if len(seg) == 2 && seg[1] == "refunds" {
		s.simpleRefund("coinsph").ServeHTTP(w, r)
		return
	}
	s.simpleGet("coinsph").ServeHTTP(w, r)
}

func simpleStatus(scn Scenario) string {
	switch scn {
	case ScenarioSuccess:
		return "SUCCESS"
	case ScenarioRequiresAction, ScenarioAsyncSuccess, ScenarioAsyncFail:
		return "PENDING"
	default:
		return "FAILED"
	}
}

func simpleEnvelope(p *Payment) map[string]any {
	// A union of the fields real channels return, so each adapter's JSON
	// decoder finds what it needs without choking on extras.
	return map[string]any{
		"id":              p.PaymentID,
		"reference_id":    p.RequestID,
		"external_id":     p.RequestID,
		"invoice_id":      p.PaymentID,
		"transaction_id":  p.PaymentID,
		"order_id":        p.PaymentID,
		"status":          p.Status,
		"state":           p.Status,
		"amount":          p.Amount,
		"value":           float64(p.Amount) / 100,
		"currency":        p.Currency,
		"checkout_url":    fmt.Sprintf("https://mock.local/checkout/%s/%s", p.Channel, p.PaymentID),
		"redirect_url":    fmt.Sprintf("https://mock.local/redirect/%s/%s", p.Channel, p.PaymentID),
		"payment_url":     fmt.Sprintf("https://mock.local/pay/%s/%s", p.Channel, p.PaymentID),
		"refunded_amount": p.RefundedAmount,
	}
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// readAllAndRestore reads req.Body, resets it to be re-readable, and returns
// the raw bytes. Useful when the body is consumed twice.
func readAllAndRestore(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	buf, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(buf))
	return buf, err
}
