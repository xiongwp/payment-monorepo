package gcash

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/xiongwp/payment-channel/internal/channel"
)

// 用 RoundTripper 拦截 HTTP 请求 + 注入伪响应：避免真连 GCash 沙箱。
type fakeRT struct {
	gotBody  []byte
	respJSON string
}

func (f *fakeRT) RoundTrip(r *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(r.Body)
	f.gotBody = b
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(f.respJSON)),
		Header:     make(http.Header),
	}, nil
}

func newTestAdapter(t *testing.T, rt http.RoundTripper) *Adapter {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(priv),
	})
	a, err := New(Config{
		Env:          "sandbox",
		PartnerID:    "P-TEST",
		MerchantPriv: string(pemBytes),
		BaseURL:      "https://example.invalid",
	})
	if err != nil {
		t.Fatal(err)
	}
	a.h = &http.Client{Transport: rt}
	return a
}

// 默认 H5 流：metadata 不带 gcash_flow → CONNECT_WALLET + app_redirect
func TestGcashCharge_DefaultFlow_AppRedirect(t *testing.T) {
	rt := &fakeRT{respJSON: `{"result":{"resultStatus":"U","resultCode":"PAYMENT_IN_PROCESS"},"paymentId":"PAY123","normalUrl":"https://gcash.test/redir"}`}
	a := newTestAdapter(t, rt)

	resp, err := a.Charge(context.Background(), &channel.ChargeRequest{
		PiID:           "pi_001",
		IdempotencyKey: "idem_h5_001",
		Amount:         10000,
		Currency:       "PHP",
		ReturnURL:      "https://shop.test/return",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Result != channel.ResultRequiresAction {
		t.Fatalf("result = %s, want requires_action", resp.Result)
	}
	if resp.RequiredAction == nil || resp.RequiredAction.Type != "app_redirect" {
		t.Fatalf("RequiredAction = %+v, want type=app_redirect", resp.RequiredAction)
	}
	if resp.RequiredAction.RedirectURL == "" {
		t.Fatal("expected non-empty RedirectURL for H5 flow")
	}
	// H5 flow 不应注入 mini-program Extra
	if _, ok := resp.RequiredAction.Extra["payment_id"]; ok {
		t.Fatal("H5 flow should not put payment_id in Extra")
	}
	// 请求体应使用 CONNECT_WALLET
	if !strings.Contains(string(rt.gotBody), `"paymentMethodType":"CONNECT_WALLET"`) {
		t.Fatalf("expected CONNECT_WALLET, body=%s", rt.gotBody)
	}
}

// 小程序流：metadata gcash_flow=miniprogram → MINI_APP + mini_program_invoke
func TestGcashCharge_MiniProgramFlow(t *testing.T) {
	rt := &fakeRT{respJSON: `{"result":{"resultStatus":"U","resultCode":"PAYMENT_IN_PROCESS"},"paymentId":"PAY999"}`}
	a := newTestAdapter(t, rt)

	resp, err := a.Charge(context.Background(), &channel.ChargeRequest{
		PiID:           "pi_002",
		IdempotencyKey: "idem_mp_001",
		Amount:         50000,
		Currency:       "PHP",
		Metadata:       map[string]string{MetaFlow: FlowMiniProgram},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Result != channel.ResultRequiresAction {
		t.Fatalf("result = %s, want requires_action", resp.Result)
	}
	ra := resp.RequiredAction
	if ra == nil || ra.Type != ActionTypeMiniProgramInvoke {
		t.Fatalf("RequiredAction = %+v, want type=mini_program_invoke", ra)
	}
	if ra.RedirectURL != "" {
		t.Fatalf("mini_program_invoke must not carry RedirectURL, got %q", ra.RedirectURL)
	}
	if got := ra.Extra["payment_id"]; got != "PAY999" {
		t.Fatalf("payment_id = %q, want PAY999", got)
	}
	if got := ra.Extra["partner_id"]; got != "P-TEST" {
		t.Fatalf("partner_id = %q, want P-TEST", got)
	}
	if got := ra.Extra["sign_method"]; got != "RSA256" {
		t.Fatalf("sign_method = %q, want RSA256", got)
	}
	// 请求体应 MINI_APP，不是 CONNECT_WALLET
	if !strings.Contains(string(rt.gotBody), `"paymentMethodType":"MINI_APP"`) {
		t.Fatalf("expected MINI_APP, body=%s", rt.gotBody)
	}
	// 校验请求体里的 idempotency_key 透传成 paymentRequestId
	var req map[string]any
	_ = json.Unmarshal(rt.gotBody, &req)
	if req["paymentRequestId"] != "idem_mp_001" {
		t.Fatalf("paymentRequestId = %v, want idem_mp_001", req["paymentRequestId"])
	}
}

// 同步成功（极小额免密）路径：mini-program 模式下 ResultStatus="S" 直接 Succeeded
// 不返回 RequiredAction（前端无需唤起 JSAPI）
func TestGcashCharge_MiniProgramFlow_DirectSuccess(t *testing.T) {
	rt := &fakeRT{respJSON: `{"result":{"resultStatus":"S","resultCode":"SUCCESS"},"paymentId":"PAY_S"}`}
	a := newTestAdapter(t, rt)
	resp, err := a.Charge(context.Background(), &channel.ChargeRequest{
		PiID:           "pi_003",
		IdempotencyKey: "idem_mp_002",
		Amount:         100,
		Currency:       "PHP",
		Metadata:       map[string]string{MetaFlow: FlowMiniProgram},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Result != channel.ResultSucceeded {
		t.Fatalf("result = %s, want succeeded", resp.Result)
	}
	if resp.RequiredAction != nil {
		t.Fatalf("succeeded path must not return RequiredAction, got %+v", resp.RequiredAction)
	}
}
