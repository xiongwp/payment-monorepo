package mockserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// registerGCash wires the three endpoints the GCash adapter hits against the
// Alipay+ mPaaS backbone.
func (s *Server) registerGCash() {
	s.mux.HandleFunc("/v1/payments/pay", s.gcashPay)
	s.mux.HandleFunc("/v1/payments/refund", s.gcashRefund)
	s.mux.HandleFunc("/v1/payments/inquiryPayment", s.gcashInquiry)
}

type gcashAmount struct {
	Currency string `json:"currency"`
	Value    string `json:"value"`
}

type gcashPayReq struct {
	PartnerID          string      `json:"partnerId"`
	PaymentRequestID   string      `json:"paymentRequestId"`
	PaymentAmount      gcashAmount `json:"paymentAmount"`
	PaymentNotifyURL   string      `json:"paymentNotifyUrl"`
	PaymentRedirectURL string      `json:"paymentRedirectUrl"`
	Order              *struct {
		ReferenceOrderID string `json:"referenceOrderId"`
		OrderDescription string `json:"orderDescription"`
	} `json:"order"`
}

type gcashResult struct {
	ResultCode    string `json:"resultCode"`
	ResultStatus  string `json:"resultStatus"`
	ResultMessage string `json:"resultMessage,omitempty"`
}

type gcashPayResp struct {
	Result           gcashResult `json:"result"`
	PaymentRequestID string      `json:"paymentRequestId,omitempty"`
	PaymentID        string      `json:"paymentId,omitempty"`
	PaymentTime      string      `json:"paymentTime,omitempty"`
	NormalURL        string      `json:"normalUrl,omitempty"`
	SchemeURL        string      `json:"schemeUrl,omitempty"`
	ApplinkURL       string      `json:"applinkUrl,omitempty"`
}

func (s *Server) gcashPay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	var req gcashPayReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	amt, _ := strconv.ParseInt(req.PaymentAmount.Value, 10, 64)

	// Idempotent replay.
	if existing, ok := s.store.loadByReq("gcash", req.PaymentRequestID); ok {
		writeJSON(w, buildGCashPayResp(existing, req.PaymentRedirectURL))
		return
	}

	scn := pickScenario(r.Header.Get("X-Mock-Scenario"), amt)
	p := &Payment{
		Channel:   "gcash",
		PaymentID: randomRef("2024033100000001"),
		RequestID: req.PaymentRequestID,
		Amount:    amt,
		Currency:  req.PaymentAmount.Currency,
		Scenario:  scn,
		NotifyURL: req.PaymentNotifyURL,
		CreatedAt: time.Now().UTC(),
		Extra: map[string]string{
			"partner_id":   req.PartnerID,
			"redirect_url": req.PaymentRedirectURL,
		},
	}

	switch scn {
	case ScenarioSuccess:
		p.Status = "SUCCESS"
	case ScenarioRequiresAction:
		p.Status = "PROCESSING"
	case ScenarioFailCardDeclined:
		p.Status = "FAIL"
		p.FailureCode = "USER_PAYMENT_VERIFICATION_FAILED"
	case ScenarioFailInsufficientFunds:
		p.Status = "FAIL"
		p.FailureCode = "USER_BALANCE_NOT_ENOUGH"
	case ScenarioFailRiskBlocked:
		p.Status = "FAIL"
		p.FailureCode = "RISK_REJECT"
	case ScenarioFailChannelUnavailable:
		p.Status = "FAIL"
		p.FailureCode = "SYSTEM_ERROR"
	case ScenarioAsyncSuccess, ScenarioAsyncFail:
		p.Status = "PROCESSING"
	case ScenarioTimeout:
		time.Sleep(30 * time.Second) // adapter will give up
		return
	}

	s.store.save(p)
	writeJSON(w, buildGCashPayResp(p, req.PaymentRedirectURL))

	// Fire async webhook for async scenarios.
	if scn == ScenarioAsyncSuccess || scn == ScenarioAsyncFail {
		go s.fireGCashWebhook(p, scn == ScenarioAsyncSuccess)
	}
}

func buildGCashPayResp(p *Payment, redirect string) *gcashPayResp {
	switch p.Status {
	case "SUCCESS":
		return &gcashPayResp{
			Result:           gcashResult{ResultCode: "SUCCESS", ResultStatus: "S"},
			PaymentRequestID: p.RequestID,
			PaymentID:        p.PaymentID,
			PaymentTime:      nowRFC3339(),
		}
	case "PROCESSING":
		checkout := fmt.Sprintf("gcash://checkout?order=%s", p.PaymentID)
		appLink := fmt.Sprintf("https://m.gcash.com/app/checkout?order=%s", p.PaymentID)
		return &gcashPayResp{
			Result:           gcashResult{ResultCode: "PAYMENT_IN_PROCESS", ResultStatus: "U", ResultMessage: "payment pending user action"},
			PaymentRequestID: p.RequestID,
			PaymentID:        p.PaymentID,
			NormalURL:        redirect,
			SchemeURL:        checkout,
			ApplinkURL:       appLink,
		}
	default: // FAIL
		return &gcashPayResp{
			Result:           gcashResult{ResultCode: p.FailureCode, ResultStatus: "F", ResultMessage: "payment failed"},
			PaymentRequestID: p.RequestID,
		}
	}
}

func (s *Server) gcashRefund(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PartnerID       string      `json:"partnerId"`
		PaymentID       string      `json:"paymentId"`
		RefundRequestID string      `json:"refundRequestId"`
		RefundAmount    gcashAmount `json:"refundAmount"`
		RefundReason    string      `json:"refundReason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	p, ok := s.store.loadByPay("gcash", req.PaymentID)
	if !ok {
		writeJSON(w, map[string]any{"result": gcashResult{ResultCode: "PAYMENT_NOT_FOUND", ResultStatus: "F"}})
		return
	}
	if p.Status != "SUCCESS" {
		writeJSON(w, map[string]any{"result": gcashResult{ResultCode: "ORDER_STATUS_INVALID", ResultStatus: "F"}})
		return
	}
	amt, _ := strconv.ParseInt(req.RefundAmount.Value, 10, 64)
	if p.RefundedAmount+amt > p.Amount {
		writeJSON(w, map[string]any{"result": gcashResult{ResultCode: "REFUND_AMOUNT_EXCEED", ResultStatus: "F"}})
		return
	}
	s.store.update("gcash", req.PaymentID, func(q *Payment) { q.RefundedAmount += amt })
	writeJSON(w, map[string]any{
		"result":   gcashResult{ResultCode: "SUCCESS", ResultStatus: "S"},
		"refundId": randomRef("2024040100000001r"),
	})
}

func (s *Server) gcashInquiry(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PartnerID        string `json:"partnerId"`
		PaymentRequestID string `json:"paymentRequestId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	// The adapter passes PiID as the RequestID here — but some integrations
	// use paymentId. Try both.
	p, ok := s.store.loadByReq("gcash", req.PaymentRequestID)
	if !ok {
		p, ok = s.store.loadByPay("gcash", req.PaymentRequestID)
	}
	if !ok {
		writeJSON(w, map[string]any{"result": gcashResult{ResultCode: "PAYMENT_NOT_FOUND", ResultStatus: "F"}})
		return
	}
	out := map[string]any{
		"result":        gcashResult{ResultCode: "SUCCESS", ResultStatus: "S"},
		"paymentId":     p.PaymentID,
		"paymentStatus": p.Status,
		"paymentAmount": gcashAmount{Currency: p.Currency, Value: strconv.FormatInt(p.Amount, 10)},
	}
	writeJSON(w, out)
}

// fireGCashWebhook constructs and dispatches a realistic GCash notify payload,
// including the `Signature` header the adapter's ParseWebhook expects.
func (s *Server) fireGCashWebhook(p *Payment, success bool) {
	status := "SUCCESS"
	if !success {
		status = "FAIL"
	}
	s.store.update("gcash", p.PaymentID, func(q *Payment) { q.Status = status })
	body := map[string]any{
		"paymentRequestId": p.RequestID,
		"paymentId":        p.PaymentID,
		"paymentStatus":    status,
		"paymentAmount":    gcashAmount{Currency: p.Currency, Value: strconv.FormatInt(p.Amount, 10)},
		"paymentTime":      nowRFC3339(),
	}
	s.disp.fire(
		withDispatcherCtx(),
		p.NotifyURL,
		body,
		func(req *http.Request, raw []byte) {
			sig, err := rsaSignSHA256B64(s.gcashPriv, raw)
			if err != nil {
				return
			}
			req.Header.Set("Signature", sig)
			req.Header.Set("Request-Time", nowRFC3339())
			req.Header.Set("client-id", p.Extra["partner_id"])
		},
	)
}

// writeJSON is a small helper — every mock channel returns JSON.
func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// header lookup with case tolerance; GCash mixes casing.
func headerAny(h http.Header, keys ...string) string {
	for _, k := range keys {
		if v := h.Get(k); v != "" {
			return v
		}
		if v := h.Get(strings.ToLower(k)); v != "" {
			return v
		}
	}
	return ""
}
