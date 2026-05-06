// Command mock-network 启动一个本地 HTTPS server，模拟 5 个卡组织（Visa /
// Mastercard / JCB / AmEx / UnionPay）的 REST API，给 dev / e2e 测试用。
//
// **生产严禁部署**。assertProdSafety 在 card-payment main.go 里阻止 prod 指向
// 这个 endpoint —— 启动期 fail-fast。
//
// 启动：
//
//	go run ./cmd/mock-network --addr :9555 --cert tls.crt --key tls.key
//
// card-payment 配置指过来：
//
//	network.visa.endpoint = "https://localhost:9555/visa"
//	network.mastercard.endpoint = "https://localhost:9555/mastercard"
//	... etc
//	network.*.insecure_sandbox = true   # 自签证书
//
// Mock 决定逻辑（按 BIN 前 4 位）：
//
//	Visa:        4242 = approved (happy)；4000 = SOFT；4001 = HARD；4002 = pending
//	Mastercard:  5454 / 5555 = approved；5100 = SOFT；5101 = HARD
//	JCB:         3528 / 3589 = approved；3500 = SOFT；3501 = HARD
//	AmEx:        3782 / 3714 = approved；3700 = SOFT；3701 = HARD
//	UnionPay:    62xx = approved；6225 = SOFT (51 余额不足)；6226 = HARD (34 涉嫌欺诈)
package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	addr     = flag.String("addr", ":9555", "listen address")
	certPath = flag.String("cert", "", "TLS cert (omit → HTTP plain, dev only)")
	keyPath  = flag.String("key", "", "TLS key")
	verbose  = flag.Bool("v", false, "log every request body (CAUTION: PAN 会进 log，**仅 dev**)")

	// ── 故障注入开关（也可由请求 header 临时覆盖：X-Mock-Fault: timeout|500|429）──

	// 全局错误率 (0..1)：随机命中即返 500。card-payment 对 5xx 自动重试。
	faultRate = flag.Float64("fault-rate", 0, "0..1 random 500 rate (chaos test)")

	// 全局额外延迟（ms），叠加在每个请求上。模拟卡组织慢响应。
	latencyMs = flag.Int("latency-ms", 0, "extra latency per request (ms)")

	// 启动期 seed；让结果"随机但可重放"，CI 能稳定复现。
	rngSeed = flag.Int64("seed", 0, "rng seed (0 = use unix-nanos)")

	// ── 异步 webhook 回调 ─────────────────────────────────────────────

	// approved 之后 N 秒，mock 主动 POST 一条 settled 状态到商户配置的 URL。
	// 不为空时启用。商户 URL 由 Authorize 请求 header X-Mock-Webhook-URL 覆盖
	// （per-call 灵活），CLI flag 只是默认值。
	defaultWebhookURL = flag.String("webhook-url", "", "default merchant webhook URL (POST status callback ~3s after auth)")
	webhookDelay      = flag.Int("webhook-delay-ms", 3000, "delay before async webhook (ms)")
)

// 内存 store：mock 端记交易状态，让后续 Capture/Refund/Void/Inquiry 看到一致状态。
type txnState struct {
	mu     sync.Mutex
	byID   map[string]*txn // network_ref_no → record
	seq    atomic.Int64
}

type txn struct {
	NetworkRefNo string
	OrderID      string
	Network      string
	Amount       int64
	Currency     string
	Status       string // approved / declined / captured / voided / refunded / pending
	DeclineCode  string
	DeclineCategory string
	AVS          string
	CVV          string
	FraudScore   int
	BIN          string
	Created      time.Time
}

func (s *txnState) put(t *txn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID[t.NetworkRefNo] = t
}

func (s *txnState) get(id string) *txn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byID[id]
}

func (s *txnState) update(id string, f func(*txn)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.byID[id]; ok {
		f(t)
		return true
	}
	return false
}

var store = &txnState{byID: make(map[string]*txn)}

// asyncWebhookClient 给商户回调用，轻量复用 http.DefaultClient。
var asyncWebhookClient = &http.Client{Timeout: 5 * time.Second}

func main() {
	flag.Parse()
	if *rngSeed == 0 {
		*rngSeed = time.Now().UnixNano()
	}
	rand.New(rand.NewSource(*rngSeed))
	log.Printf("[mock-network] seed=%d fault_rate=%.2f latency=%dms webhook=%s",
		*rngSeed, *faultRate, *latencyMs, *defaultWebhookURL)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	// 给运维 / e2e 测试一个 admin 端点，看当前所有 txn
	mux.HandleFunc("/admin/txns", func(w http.ResponseWriter, _ *http.Request) {
		store.mu.Lock()
		defer store.mu.Unlock()
		writeJSON(w, 200, store.byID)
	})
	mux.Handle("/visa/", http.StripPrefix("/visa", visaMux()))
	mux.Handle("/mastercard/", http.StripPrefix("/mastercard", mcMux()))
	mux.Handle("/jcb/", http.StripPrefix("/jcb", jcbMux()))
	mux.Handle("/amex/", http.StripPrefix("/amex", amexMux()))
	mux.Handle("/unionpay/", http.StripPrefix("/unionpay", upMux()))

	srv := &http.Server{
		Addr:              *addr,
		Handler:           logMW(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}
	if *certPath != "" && *keyPath != "" {
		log.Printf("[mock-network] HTTPS listening on %s", *addr)
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		log.Fatal(srv.ListenAndServeTLS(*certPath, *keyPath))
	}
	log.Printf("[mock-network] HTTP (insecure) listening on %s — dev only", *addr)
	log.Fatal(srv.ListenAndServe())
}

// logMW 同时承担 (1) 请求日志 (2) 故障注入 (3) 额外延迟。
//
// 故障注入优先级：header X-Mock-Fault > flag --fault-rate
//
//   X-Mock-Fault=timeout: 直接 sleep 60s 模拟卡组织 hang
//   X-Mock-Fault=500:     直接 500 + 空 body
//   X-Mock-Fault=429:     直接 429 + Retry-After: 1
//   X-Mock-Fault=tls:     主动 hijack 关连接
func logMW(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// 全局延迟
		if *latencyMs > 0 {
			time.Sleep(time.Duration(*latencyMs) * time.Millisecond)
		}
		if d := r.Header.Get("X-Mock-Latency-Ms"); d != "" {
			if ms, err := strconv.Atoi(d); err == nil && ms > 0 && ms < 60_000 {
				time.Sleep(time.Duration(ms) * time.Millisecond)
			}
		}

		// 故障注入
		fault := r.Header.Get("X-Mock-Fault")
		if fault == "" && *faultRate > 0 && rand.Float64() < *faultRate {
			fault = "500"
		}
		switch fault {
		case "timeout":
			time.Sleep(60 * time.Second)
			http.Error(w, "mock: simulated timeout", http.StatusGatewayTimeout)
			return
		case "500":
			http.Error(w, "mock: simulated 500", http.StatusInternalServerError)
			log.Printf("[mock] FAULT 500 %s %s", r.Method, r.URL.Path)
			return
		case "429":
			w.Header().Set("Retry-After", "1")
			http.Error(w, "mock: simulated rate limit", http.StatusTooManyRequests)
			log.Printf("[mock] FAULT 429 %s %s", r.Method, r.URL.Path)
			return
		case "tls":
			// 强制断开
			hj, ok := w.(http.Hijacker)
			if ok {
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
					return
				}
			}
		}

		h.ServeHTTP(w, r)
		log.Printf("[mock] %s %s %v", r.Method, r.URL.Path, time.Since(start))
	})
}

// scheduleWebhookCallback 在 Authorize 后异步给商户推一条状态变更通知。
//
// 真实卡组织（除 UnionPay）通常不会主动推授权状态——授权同步给响应。但
// **settle/refund/dispute** 这类事件是异步的（清算 T+1，争议数小时后）。
// mock 简化：approved 后 webhookDelay 秒推一条 "settled"，方便测试 caller
// 的 webhook 处理路径。商户 URL 用 X-Mock-Webhook-URL header 或 flag 默认值。
func scheduleWebhookCallback(network string, t *txn, callerURL string) {
	url := callerURL
	if url == "" {
		url = *defaultWebhookURL
	}
	if url == "" {
		return
	}
	go func() {
		time.Sleep(time.Duration(*webhookDelay) * time.Millisecond)
		body, _ := json.Marshal(map[string]any{
			"event":          "transaction.settled",
			"network":        network,
			"network_ref_no": t.NetworkRefNo,
			"order_id":       t.OrderID,
			"status":         "settled",
			"amount":         t.Amount,
			"currency":       t.Currency,
			"settled_at":     time.Now().UTC().Format(time.RFC3339),
		})
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			log.Printf("[mock] webhook build failed: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Mock-Network", network)
		req.Header.Set("X-Mock-Event", "settled")
		resp, err := asyncWebhookClient.Do(req)
		if err != nil {
			log.Printf("[mock] webhook POST %s failed: %v", url, err)
			return
		}
		_ = resp.Body.Close()
		log.Printf("[mock] webhook POST %s → %d (network=%s ref=%s)", url, resp.StatusCode, network, t.NetworkRefNo)
	}()
}

// ─── Visa: CyberSource v2 shape ─────────────────────────────────────────
//
// POST /pts/v2/payments
// POST /pts/v2/payments/{id}/captures
// POST /pts/v2/payments/{id}/refunds
// POST /pts/v2/payments/{id}/voids
// GET  /tss/v2/transactions/{id}

func visaMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/pts/v2/payments", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(405); return
		}
		var body struct {
			ClientReferenceInformation struct{ Code string } `json:"clientReferenceInformation"`
			OrderInformation           struct {
				AmountDetails struct{ TotalAmount, Currency string } `json:"amountDetails"`
			} `json:"orderInformation"`
			PaymentInformation struct {
				Card struct{ Number string } `json:"card"`
			} `json:"paymentInformation"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		bin := safePrefix(body.PaymentInformation.Card.Number, 4)
		t := newMockTxn("visa", body.ClientReferenceInformation.Code, bin,
			body.PaymentInformation.Card.Number,
			body.OrderInformation.AmountDetails.TotalAmount,
			body.OrderInformation.AmountDetails.Currency)
		store.put(t)
		if t.Status == "approved" {
			scheduleWebhookCallback("visa", t, r.Header.Get("X-Mock-Webhook-URL"))
		}

		out := map[string]any{
			"id":               t.NetworkRefNo,
			"reconciliationId": t.NetworkRefNo + "-arn",
			"processorInformation": map[string]any{
				"approvalCode": "OK000",
				"avs":              map[string]string{"code": t.AVS},
				"cardVerification": map[string]string{"resultCode": t.CVV},
				"networkResponse":  map[string]string{"processorResponseCode": "00"},
			},
			"riskInformation": map[string]any{"score": map[string]int{"result": t.FraudScore}},
		}
		switch t.Status {
		case "approved":
			out["status"] = "AUTHORIZED"
		case "declined":
			out["status"] = "DECLINED"
			out["errorInformation"] = map[string]string{"reason": t.DeclineCode, "message": "mock decline"}
		case "pending":
			out["status"] = "PENDING"
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("/pts/v2/payments/", func(w http.ResponseWriter, r *http.Request) {
		// /pts/v2/payments/{id}/captures|refunds|voids
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/pts/v2/payments/"), "/")
		if len(parts) < 2 {
			w.WriteHeader(404); return
		}
		id, action := parts[0], parts[1]
		t := store.get(id)
		if t == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"reason": "NOT_FOUND"}); return
		}
		switch action {
		case "captures":
			store.update(id, func(t *txn) { t.Status = "captured" })
			writeJSON(w, 200, map[string]string{"id": id + "-cap", "status": "PENDING"})
		case "refunds":
			store.update(id, func(t *txn) { t.Status = "refunded" })
			writeJSON(w, 200, map[string]string{"id": id + "-rf", "status": "PENDING"})
		case "voids":
			store.update(id, func(t *txn) { t.Status = "voided" })
			writeJSON(w, 200, map[string]string{"id": id + "-vd", "status": "VOIDED"})
		default:
			w.WriteHeader(404)
		}
	})
	mux.HandleFunc("/tss/v2/transactions/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/tss/v2/transactions/")
		t := store.get(id)
		if t == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{}); return
		}
		writeJSON(w, 200, map[string]any{
			"id":                     t.NetworkRefNo,
			"applicationInformation": map[string]string{"status": strings.ToUpper(t.Status), "reasonCode": t.DeclineCode},
			"orderInformation": map[string]any{
				"amountDetails": map[string]string{
					"totalAmount": fmt.Sprintf("%d.%02d", t.Amount/100, t.Amount%100),
					"currency":    t.Currency,
				},
			},
		})
	})
	return mux
}

// ─── Mastercard MPGS: PUT /api/rest/version/76/merchant/.../order/.../transaction/... ─

func mcMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/rest/version/76/merchant/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			// Inquiry: /api/rest/version/76/merchant/{m}/order/{id}
			id := lastSegment(r.URL.Path, "order")
			t := store.get(id)
			if t == nil {
				writeJSON(w, 404, map[string]string{}); return
			}
			writeJSON(w, 200, map[string]any{
				"id": id, "status": strings.ToUpper(t.Status), "amount": fmt.Sprintf("%d.%02d", t.Amount/100, t.Amount%100), "currency": t.Currency,
			})
			return
		}
		// PUT or POST → transaction
		var body struct {
			APIOperation string `json:"apiOperation"`
			Order        struct{ Amount, Currency, Reference string } `json:"order"`
			SourceOfFunds struct {
				Provided struct{ Card struct{ Number string } `json:"card"` } `json:"provided"`
			} `json:"sourceOfFunds"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		orderID := lastSegment(r.URL.Path, "order")
		bin := safePrefix(body.SourceOfFunds.Provided.Card.Number, 4)
		switch body.APIOperation {
		case "PAY", "AUTHORIZE":
			t := newMockTxn("mastercard", orderID, bin, body.SourceOfFunds.Provided.Card.Number, body.Order.Amount, body.Order.Currency)
			store.put(t)
			if t.Status == "approved" {
				scheduleWebhookCallback("mastercard", t, r.Header.Get("X-Mock-Webhook-URL"))
			}
			result := "SUCCESS"
			if t.Status == "declined" {
				result = "FAILURE"
			} else if t.Status == "pending" {
				result = "PENDING"
			}
			writeJSON(w, 200, map[string]any{
				"result": result,
				"order":  map[string]string{"id": orderID, "status": strings.ToUpper(t.Status)},
				"transaction": map[string]string{
					"id":            t.NetworkRefNo,
					"receiptNumber": t.NetworkRefNo + "-rcpt",
				},
				"response": map[string]any{
					"gatewayCode": map[bool]string{true: "APPROVED", false: t.DeclineCode}[t.Status == "approved"],
				},
				"risk": map[string]any{"response": map[string]any{"score": map[string]int{"value": t.FraudScore}}},
			})
		case "CAPTURE", "REFUND", "VOID":
			store.update(orderID, func(t *txn) {
				switch body.APIOperation {
				case "CAPTURE":
					t.Status = "captured"
				case "REFUND":
					t.Status = "refunded"
				case "VOID":
					t.Status = "voided"
				}
			})
			writeJSON(w, 200, map[string]any{"result": "SUCCESS", "transaction": map[string]string{"id": orderID + "-" + strings.ToLower(body.APIOperation)}})
		default:
			writeJSON(w, 400, map[string]string{"error": "unknown apiOperation"})
		}
	})
	return mux
}

// ─── JCB / AmEx / UnionPay: 简化 mock ──────────────────────────────────

func jcbMux() http.Handler   { return jsonNetworkMux("jcb") }
func amexMux() http.Handler  { return jsonNetworkMux("amex") }

// 通用 JSON-based 网络（JCB / AmEx）mock
func jsonNetworkMux(network string) http.Handler {
	mux := http.NewServeMux()
	root := "/api/v1/payments"
	if network == "amex" {
		root = "/payments/digital/v2/payments"
	}
	mux.HandleFunc(root, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(405); return
		}
		var body struct {
			MerchantTransactionID string `json:"merchant_transaction_id"`
			Amount                any    `json:"amount"`
			Currency              string `json:"currency"`
			Card struct {
				Number string `json:"number"`
			} `json:"card"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		// AmEx 把 amount 套了一层 {value, currency}
		amount, currency := normalizeAmount(body.Amount, body.Currency)
		bin := safePrefix(body.Card.Number, 4)
		t := newMockTxn(network, body.MerchantTransactionID, bin, body.Card.Number, amount, currency)
		store.put(t)
		if t.Status == "approved" {
			scheduleWebhookCallback(network, t, r.Header.Get("X-Mock-Webhook-URL"))
		}
		out := map[string]any{
			"transaction_id": t.NetworkRefNo,
			"approval_code":  "OK",
			"avs_result":     t.AVS,
			"cvv_result":     t.CVV,
			"fraud_score":    t.FraudScore,
		}
		switch t.Status {
		case "approved":
			out["status"] = "APPROVED"
		case "pending":
			out["status"] = "PENDING"
		case "declined":
			out["status"] = "DECLINED"
			out["decline_code"] = t.DeclineCode
			out["decline_message"] = "mock decline"
		}
		writeJSON(w, 200, out)
	})
	mux.HandleFunc(root+"/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, root+"/")
		parts := strings.Split(path, "/")
		id := parts[0]
		var action string
		if len(parts) > 1 {
			action = parts[1]
		}
		switch action {
		case "":
			// GET inquiry
			t := store.get(id)
			if t == nil {
				writeJSON(w, 404, map[string]string{}); return
			}
			writeJSON(w, 200, map[string]any{
				"transaction_id": id, "status": strings.ToUpper(t.Status),
				"amount": map[string]any{"value": t.Amount, "currency": t.Currency},
			})
		case "capture":
			store.update(id, func(t *txn) { t.Status = "captured" })
			writeJSON(w, 200, map[string]string{"transaction_id": id + "-cap", "status": "CAPTURED"})
		case "refund":
			store.update(id, func(t *txn) { t.Status = "refunded" })
			writeJSON(w, 200, map[string]string{"refund_id": id + "-rf", "status": "REFUNDED"})
		case "void":
			store.update(id, func(t *txn) { t.Status = "voided" })
			writeJSON(w, 200, map[string]string{"status": "VOIDED"})
		default:
			w.WriteHeader(404)
		}
	})
	return mux
}

// ─── UnionPay form-encoded ──────────────────────────────────────────────

func upMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/gateway/api/backTransReq.do", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		txnType := r.Form.Get("txnType")
		orderId := r.Form.Get("orderId")
		amt, _ := parseInt(r.Form.Get("txnAmount"))
		currency := isoNumToCurrency(r.Form.Get("currencyCode"))
		bin := safePrefix(r.Form.Get("accNo"), 4)
		switch txnType {
		case "01":
			t := newMockTxn("unionpay", orderId, bin, r.Form.Get("accNo"), fmt.Sprintf("%d", amt), currency)
			t.Amount = amt
			t.Currency = currency
			store.put(t)
			if t.Status == "approved" {
				scheduleWebhookCallback("unionpay", t, r.Header.Get("X-Mock-Webhook-URL"))
			}
			respCode := "00"
			respMsg := "成功"
			if t.Status == "declined" {
				respCode = t.DeclineCode
				respMsg = "失败：" + t.DeclineCode
			}
			out := url.Values{}
			out.Set("respCode", respCode)
			out.Set("respMsg", respMsg)
			out.Set("queryId", t.NetworkRefNo)
			out.Set("traceNo", t.NetworkRefNo+"-trc")
			writeForm(w, out)
		case "03", "04", "31":
			origID := r.Form.Get("origQryId")
			store.update(origID, func(t *txn) {
				switch txnType {
				case "03": t.Status = "captured"
				case "04": t.Status = "refunded"
				case "31": t.Status = "voided"
				}
			})
			out := url.Values{}
			out.Set("respCode", "00")
			out.Set("queryId", origID+"-"+txnType)
			writeForm(w, out)
		}
	})
	mux.HandleFunc("/gateway/api/queryTrans.do", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		t := store.get(r.Form.Get("origQryId"))
		out := url.Values{}
		if t == nil {
			out.Set("respCode", "34")
			writeForm(w, out)
			return
		}
		respCode := "00"
		if t.Status == "declined" {
			respCode = t.DeclineCode
		}
		out.Set("respCode", respCode)
		out.Set("queryId", t.NetworkRefNo)
		out.Set("txnAmount", fmt.Sprintf("%d", t.Amount))
		out.Set("currencyCode", currencyToISONum(t.Currency))
		writeForm(w, out)
	})
	return mux
}

// ─── 共用 helpers ─────────────────────────────────────────────────────

// newMockTxn 按 BIN 决定 status / risk 字段，跟各 adapter 的 mockAuthorize 对齐。
func newMockTxn(network, orderID, bin, pan, amountStr, currency string) *txn {
	t := &txn{
		NetworkRefNo: network + "_" + orderID + "_" + fmt.Sprintf("%d", store.seq.Add(1)),
		OrderID:      orderID,
		Network:      network,
		Currency:     currency,
		BIN:          bin,
		Created:      time.Now(),
	}
	t.Amount = parseAmount(amountStr, currency)
	switch network {
	case "visa":
		switch bin {
		case "4242":
			t.Status, t.AVS, t.CVV, t.FraudScore = "approved", "Y", "M", 5
		case "4000":
			t.Status, t.DeclineCode, t.DeclineCategory = "declined", "INSUFFICIENT_FUNDS", "SOFT"
		case "4001":
			t.Status, t.DeclineCode, t.DeclineCategory, t.FraudScore = "declined", "STOLEN_CARD", "HARD", 99
		case "4002":
			t.Status = "pending"
		default:
			t.Status, t.AVS, t.CVV, t.FraudScore = "approved", "U", "U", 20
		}
	case "mastercard":
		switch bin {
		case "5454", "5555":
			t.Status, t.AVS, t.CVV, t.FraudScore = "approved", "MATCH", "MATCH", 8
		case "5100":
			t.Status, t.DeclineCode, t.DeclineCategory = "declined", "INSUFFICIENT_FUNDS", "SOFT"
		case "5101":
			t.Status, t.DeclineCode, t.DeclineCategory, t.FraudScore = "declined", "FRAUDULENT_TRANSACTION", "HARD", 95
		default:
			t.Status, t.AVS, t.CVV, t.FraudScore = "approved", "NOT_VERIFIED", "NOT_PRESENT", 25
		}
	case "jcb":
		switch bin {
		case "3528", "3589":
			t.Status, t.AVS, t.CVV, t.FraudScore = "approved", "Y", "M", 10
		case "3500":
			t.Status, t.DeclineCode, t.DeclineCategory = "declined", "INSUFFICIENT_FUNDS", "SOFT"
		case "3501":
			t.Status, t.DeclineCode, t.DeclineCategory, t.FraudScore = "declined", "FRAUDULENT_TRANSACTION", "HARD", 90
		default:
			t.Status, t.AVS, t.CVV = "approved", "U", "U"
		}
	case "amex":
		switch bin {
		case "3782", "3714":
			t.Status, t.AVS, t.CVV, t.FraudScore = "approved", "Y", "M", 12
		case "3700":
			t.Status, t.DeclineCode, t.DeclineCategory = "declined", "INSUFFICIENT_FUNDS", "SOFT"
		case "3701":
			t.Status, t.DeclineCode, t.DeclineCategory, t.FraudScore = "declined", "SUSPECTED_FRAUD", "HARD", 92
		default:
			t.Status = "approved"
		}
	case "unionpay":
		switch bin {
		case "6225":
			t.Status, t.DeclineCode, t.DeclineCategory = "declined", "51", "SOFT"
		case "6226":
			t.Status, t.DeclineCode, t.DeclineCategory, t.FraudScore = "declined", "34", "HARD", 95
		default:
			t.Status = "approved"
			if strings.HasPrefix(bin, "62") {
				t.AVS, t.CVV, t.FraudScore = "Y", "M", 8
			}
		}
	}
	return t
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
func writeForm(w http.ResponseWriter, body url.Values) {
	w.Header().Set("Content-Type", "application/x-www-form-urlencoded")
	w.WriteHeader(200)
	_, _ = fmt.Fprint(w, body.Encode())
}
func safePrefix(s string, n int) string {
	if len(s) < n {
		return s
	}
	return s[:n]
}
func lastSegment(path, marker string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if p == marker && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}
func parseAmount(s, currency string) int64 {
	if s == "" {
		return 0
	}
	switch strings.ToUpper(currency) {
	case "JPY", "KRW", "VND", "ISK":
		v, _ := parseInt(s)
		return v
	}
	parts := strings.Split(s, ".")
	major, _ := parseInt(parts[0])
	var minor int64
	if len(parts) > 1 {
		f := parts[1]
		if len(f) > 2 {
			f = f[:2]
		}
		for len(f) < 2 {
			f += "0"
		}
		minor, _ = parseInt(f)
	}
	return major*100 + minor
}
func parseInt(s string) (int64, error) {
	var n int64
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}
func normalizeAmount(amount any, currency string) (string, string) {
	switch v := amount.(type) {
	case map[string]any:
		// AmEx style
		val, _ := v["value"].(string)
		cur, _ := v["currency"].(string)
		return val, cur
	case string:
		return v, currency
	case float64:
		return fmt.Sprintf("%d", int64(v)), currency
	}
	return "0", currency
}
func currencyToISONum(c string) string {
	switch strings.ToUpper(c) {
	case "CNY": return "156"
	case "USD": return "840"
	case "EUR": return "978"
	case "JPY": return "392"
	case "HKD": return "344"
	case "GBP": return "826"
	}
	return ""
}
func isoNumToCurrency(n string) string {
	switch n {
	case "156": return "CNY"
	case "840": return "USD"
	case "978": return "EUR"
	case "392": return "JPY"
	case "344": return "HKD"
	case "826": return "GBP"
	}
	return ""
}
