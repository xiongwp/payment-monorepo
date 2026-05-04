package mockserver_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/xiongwp/payment-channel/internal/adapter/dragonpay"
	"github.com/xiongwp/payment-channel/internal/adapter/gcash"
	"github.com/xiongwp/payment-channel/internal/adapter/grabpay"
	"github.com/xiongwp/payment-channel/internal/adapter/maya"
	"github.com/xiongwp/payment-channel/internal/adapter/paymongo"
	"github.com/xiongwp/payment-channel/internal/adapter/xendit"
	"github.com/xiongwp/payment-channel/internal/channel"
	"github.com/xiongwp/payment-channel/internal/mockserver"
)

// TestGCashChargeSuccess drives the GCash adapter against the in-process mock
// end-to-end: scenario=success → adapter returns Succeeded synchronously.
func TestGCashChargeSuccess(t *testing.T) {
	srv, base, stop := newMock(t, 0)
	defer stop()
	_ = srv

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privPEM, _ := mockserver.MarshalPKCS8PrivateKeyPEM(priv)

	a, err := gcash.New(gcash.Config{
		Env:          "sandbox",
		PartnerID:    "PARTNER_TEST",
		MerchantPriv: privPEM,
		GCashPubKey:  srv.GCashPublicKeyPEM(),
		BaseURL:      base,
		NotifyURL:    "http://example/wh/gcash",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := a.Charge(ctx, &channel.ChargeRequest{
		PiID:           "pi_1",
		IdempotencyKey: "idem_success_1",
		Amount:         1, // scenario: success
		Currency:       "PHP",
	})
	if err != nil {
		t.Fatalf("charge: %v", err)
	}
	if resp.Result != channel.ResultSucceeded {
		t.Fatalf("want Succeeded, got %s (fail=%s msg=%s)", resp.Result, resp.FailureCode, resp.FailureMessage)
	}
	if resp.ExternalRefNo == "" {
		t.Fatal("want external ref")
	}

	// Idempotent retry returns same external ref.
	resp2, err := a.Charge(ctx, &channel.ChargeRequest{
		PiID: "pi_1", IdempotencyKey: "idem_success_1", Amount: 1, Currency: "PHP",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp2.ExternalRefNo != resp.ExternalRefNo {
		t.Fatalf("non-idempotent: %s vs %s", resp2.ExternalRefNo, resp.ExternalRefNo)
	}
}

func TestGCashChargeRequiresAction(t *testing.T) {
	srv, base, stop := newMock(t, 0)
	defer stop()

	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	privPEM, _ := mockserver.MarshalPKCS8PrivateKeyPEM(priv)

	a, _ := gcash.New(gcash.Config{
		Env: "sandbox", PartnerID: "P", MerchantPriv: privPEM,
		GCashPubKey: srv.GCashPublicKeyPEM(), BaseURL: base,
		NotifyURL: "http://example/wh/gcash",
	})
	resp, err := a.Charge(context.Background(), &channel.ChargeRequest{
		PiID: "pi_ra", IdempotencyKey: "idem_ra", Amount: 2, Currency: "PHP",
		ReturnURL: "https://merchant/ret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Result != channel.ResultRequiresAction {
		t.Fatalf("want RequiresAction, got %s", resp.Result)
	}
	if resp.RequiredAction == nil || resp.RequiredAction.RedirectURL == "" {
		t.Fatal("want redirect url")
	}
}

func TestGCashChargeFailures(t *testing.T) {
	srv, base, stop := newMock(t, 0)
	defer stop()
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	privPEM, _ := mockserver.MarshalPKCS8PrivateKeyPEM(priv)
	a, _ := gcash.New(gcash.Config{
		Env: "sandbox", PartnerID: "P", MerchantPriv: privPEM,
		GCashPubKey: srv.GCashPublicKeyPEM(), BaseURL: base,
	})
	// amount convention drives the scenario.
	for _, tc := range []struct {
		amt  int64
		name string
	}{
		{3, "card_declined"},
		{4, "insufficient_funds"},
		{5, "risk_blocked"},
		{6, "channel_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := a.Charge(context.Background(), &channel.ChargeRequest{
				PiID: "pi_" + tc.name, IdempotencyKey: "idem_" + tc.name, Amount: tc.amt, Currency: "PHP",
			})
			if err != nil {
				t.Fatal(err)
			}
			if resp.Result != channel.ResultFailed {
				t.Fatalf("want Failed, got %s", resp.Result)
			}
			if resp.FailureCode == "" {
				t.Fatal("want failure code")
			}
		})
	}
}

// TestGCashAsyncWebhook exercises the async scenario: adapter returns
// processing/requires_action, mock fires webhook, adapter verifies signature.
func TestGCashAsyncWebhook(t *testing.T) {
	srv, base, stop := newMock(t, 20*time.Millisecond)
	defer stop()

	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	privPEM, _ := mockserver.MarshalPKCS8PrivateKeyPEM(priv)

	// Capture the webhook on a local HTTP test server and parse it with the
	// adapter so we also verify ParseWebhook + signature end-to-end.
	a, _ := gcash.New(gcash.Config{
		Env: "sandbox", PartnerID: "P", MerchantPriv: privPEM,
		GCashPubKey: srv.GCashPublicKeyPEM(), BaseURL: base,
	})

	var gotEvt *channel.WebhookEvent
	var wg sync.WaitGroup
	wg.Add(1)
	whSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer wg.Done()
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		hdrs := map[string]string{}
		for k, v := range r.Header {
			if len(v) > 0 {
				hdrs[k] = v[0]
			}
		}
		evt, err := a.ParseWebhook(hdrs, body)
		if err != nil {
			t.Logf("parse webhook: %v", err)
		}
		gotEvt = evt
		w.WriteHeader(http.StatusOK)
	}))
	defer whSrv.Close()

	aWithWH, _ := gcash.New(gcash.Config{
		Env: "sandbox", PartnerID: "P", MerchantPriv: privPEM,
		GCashPubKey: srv.GCashPublicKeyPEM(), BaseURL: base,
		NotifyURL: whSrv.URL,
	})
	resp, err := aWithWH.Charge(context.Background(), &channel.ChargeRequest{
		PiID: "pi_async", IdempotencyKey: "idem_async_ok", Amount: 7, Currency: "PHP",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Result != channel.ResultRequiresAction && resp.Result != channel.ResultProcessing {
		// GCash adapter maps status=U (processing) to RequiresAction.
		t.Fatalf("want processing/requires_action, got %s", resp.Result)
	}

	doneCh := make(chan struct{})
	go func() { wg.Wait(); close(doneCh) }()
	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("webhook never delivered")
	}
	if gotEvt == nil {
		t.Fatal("webhook not parsed")
	}
	if !gotEvt.SignatureOK {
		t.Fatal("webhook signature not verified — mock signing mismatch")
	}
	if gotEvt.EventType != "charge.succeeded" {
		t.Fatalf("unexpected event type: %s", gotEvt.EventType)
	}
}

func TestGCashRefund(t *testing.T) {
	srv, base, stop := newMock(t, 0)
	defer stop()
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	privPEM, _ := mockserver.MarshalPKCS8PrivateKeyPEM(priv)
	a, _ := gcash.New(gcash.Config{
		Env: "sandbox", PartnerID: "P", MerchantPriv: privPEM,
		GCashPubKey: srv.GCashPublicKeyPEM(), BaseURL: base,
	})
	ctx := context.Background()
	pay, err := a.Charge(ctx, &channel.ChargeRequest{
		PiID: "pi_r", IdempotencyKey: "idem_r", Amount: 1000, Currency: "PHP",
	})
	if err != nil {
		t.Fatal(err)
	}
	if pay.Result != channel.ResultSucceeded {
		t.Fatalf("want succeeded, got %s", pay.Result)
	}
	r, err := a.Refund(ctx, &channel.RefundRequest{
		PiID: "pi_r", ExternalRefNo: pay.ExternalRefNo,
		Amount: 400, IdempotencyKey: "idem_r_rfd1", Reason: "requested",
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Result != channel.ResultSucceeded {
		t.Fatalf("partial refund failed: %s", r.Result)
	}
	// Over-refund should fail.
	r2, err := a.Refund(ctx, &channel.RefundRequest{
		PiID: "pi_r", ExternalRefNo: pay.ExternalRefNo,
		Amount: 800, IdempotencyKey: "idem_r_rfd2", Reason: "too-much",
	})
	if err != nil {
		t.Fatal(err)
	}
	if r2.Result != channel.ResultFailed {
		t.Fatalf("over-refund should fail, got %s", r2.Result)
	}
}

// TestOtherChannels is a table smoke test: each adapter should Charge()
// successfully against the mock.
func TestOtherChannels(t *testing.T) {
	_, base, stop := newMock(t, 0)
	defer stop()

	cases := []struct {
		name string
		run  func(t *testing.T, base string)
	}{
		{"maya", func(t *testing.T, base string) {
			a := maya.New(maya.Config{Env: "sandbox", PublicKey: "pk", SecretKey: "sk", BaseURL: base})
			r, err := a.Charge(context.Background(), &channel.ChargeRequest{
				PiID: "pi", IdempotencyKey: "im", Amount: 1, Currency: "PHP",
				ReturnURL: "https://ret",
			})
			if err != nil || r.Result != channel.ResultRequiresAction {
				t.Fatalf("maya: result=%v err=%v", r.Result, err)
			}
		}},
		{"grabpay", func(t *testing.T, base string) {
			a := grabpay.New(grabpay.Config{Env: "sandbox", PartnerID: "p", PartnerSecret: "s", MerchantID: "m", BaseURL: base})
			r, err := a.Charge(context.Background(), &channel.ChargeRequest{
				PiID: "pi", IdempotencyKey: "ig", Amount: 1, Currency: "PHP",
			})
			if err != nil {
				t.Fatalf("grabpay: %v", err)
			}
			if r.Result == channel.ResultFailed {
				t.Fatalf("grabpay: unexpected failure %s", r.FailureMessage)
			}
		}},
		{"paymongo", func(t *testing.T, base string) {
			a := paymongo.New(paymongo.Config{SecretKey: "sk_test_x", Env: "sandbox", BaseURL: base})
			r, err := a.Charge(context.Background(), &channel.ChargeRequest{
				PiID: "pi", IdempotencyKey: "ipm", Amount: 1, Currency: "PHP",
				ReturnURL: "https://ret",
			})
			if err != nil {
				t.Fatalf("paymongo: %v", err)
			}
			if r.Result == channel.ResultFailed {
				t.Fatalf("paymongo: %s", r.FailureMessage)
			}
		}},
		{"xendit", func(t *testing.T, base string) {
			a := xendit.New(xendit.Config{SecretKey: "xnd_x", Env: "sandbox", BaseURL: base})
			r, err := a.Charge(context.Background(), &channel.ChargeRequest{
				PiID: "pi", IdempotencyKey: "ixn", Amount: 1, Currency: "PHP",
				ReturnURL: "https://ret",
			})
			if err != nil {
				t.Fatalf("xendit: %v", err)
			}
			if r.Result == channel.ResultFailed {
				t.Fatalf("xendit: %s", r.FailureMessage)
			}
		}},
		{"dragonpay", func(t *testing.T, base string) {
			a := dragonpay.New(dragonpay.Config{Env: "sandbox", MerchantID: "m", MerchantKey: "k", BaseURL: base})
			r, err := a.Charge(context.Background(), &channel.ChargeRequest{
				PiID: "pi", IdempotencyKey: "idp", Amount: 1, Currency: "PHP",
			})
			if err != nil {
				t.Fatalf("dragonpay: %v", err)
			}
			if r.Result == channel.ResultFailed {
				t.Fatalf("dragonpay: %s", r.FailureMessage)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, base) })
	}
}

// newMock spins up the mockserver on a random port and returns it + the base
// URL + a stop function.
func newMock(t *testing.T, delay time.Duration) (*mockserver.Server, string, func()) {
	t.Helper()
	srv, err := mockserver.New(mockserver.Options{
		Logger:       mockserver.DiscardLogger{},
		WebhookDelay: delay,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	return srv, ts.URL, ts.Close
}
