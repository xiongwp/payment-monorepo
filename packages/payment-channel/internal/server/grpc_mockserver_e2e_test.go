// End-to-end test wiring the real gRPC AcquirerService against the real GCash
// adapter against the in-process mockserver. Proves the full path
// (client → gRPC → AcquirerService → adapter → HTTP → mock) stays green after
// the mockserver landed. Reuses the bufconn harness from grpc_e2e_test.go.
package server_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	channelv1 "reconcile-system/packages/payment-channel/kitex_gen/channel/v1"
	"github.com/xiongwp/payment-channel/internal/adapter/gcash"
	"github.com/xiongwp/payment-channel/internal/mockserver"
)

func TestPaychanE2E_RealGCash_AgainstMock(t *testing.T) {
	mock, ts := spinUpMock(t, 0)

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privPEM, _ := mockserver.MarshalPKCS8PrivateKeyPEM(priv)

	adapter, err := gcash.New(gcash.Config{
		Env:          "sandbox",
		PartnerID:    "E2E_PARTNER",
		MerchantPriv: privPEM,
		GCashPubKey:  mock.GCashPublicKeyPEM(),
		BaseURL:      ts.URL,
		NotifyURL:    "http://127.0.0.1:0/wh/gcash",
	})
	if err != nil {
		t.Fatal(err)
	}

	cli := spinUpChannelServer(t, adapter)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	t.Run("charge_success", func(t *testing.T) {
		resp, err := cli.Charge(ctx, &channelv1.ChargeRequest{
			Adapter:        "gcash",
			PiId:           "pi_e2e_ok",
			IdempotencyKey: "idem_e2e_ok",
			Amount:         1,
			Currency:       "PHP",
		})
		if err != nil {
			t.Fatalf("charge: %v", err)
		}
		if resp.GetResult() != "succeeded" {
			t.Fatalf("want succeeded, got %s", resp.GetResult())
		}
		if resp.GetExternalRefNo() == "" {
			t.Fatal("want external ref")
		}
	})

	t.Run("charge_requires_action", func(t *testing.T) {
		resp, err := cli.Charge(ctx, &channelv1.ChargeRequest{
			Adapter: "gcash", PiId: "pi_e2e_ra", IdempotencyKey: "idem_e2e_ra",
			Amount: 2, Currency: "PHP", ReturnUrl: "https://merchant/ret",
		})
		if err != nil {
			t.Fatalf("charge: %v", err)
		}
		if resp.GetResult() != "requires_action" {
			t.Fatalf("want requires_action, got %s", resp.GetResult())
		}
		ra := resp.GetRequiredAction()
		if ra == nil {
			t.Fatal("required_action missing")
		}
		// Mock returns a gcash:// scheme url for the SchemeURL; the adapter
		// forwards whichever of scheme/applink/normal came back first.
		if ra.GetRedirectUrl() == "" {
			t.Fatalf("no redirect url, got %+v", ra)
		}
	})

	t.Run("charge_fail_card_declined", func(t *testing.T) {
		resp, err := cli.Charge(ctx, &channelv1.ChargeRequest{
			Adapter: "gcash", PiId: "pi_e2e_decl", IdempotencyKey: "idem_e2e_decl",
			Amount: 3, Currency: "PHP",
		})
		if err != nil {
			t.Fatalf("charge: %v", err)
		}
		if resp.GetResult() != "failed" {
			t.Fatalf("want failed, got %s", resp.GetResult())
		}
		if !strings.Contains(resp.GetFailureCode(), "declined") && resp.GetFailureCode() == "" {
			t.Fatalf("want a declined-ish failure code, got %q", resp.GetFailureCode())
		}
	})

	t.Run("idempotent_replay", func(t *testing.T) {
		resp1, err := cli.Charge(ctx, &channelv1.ChargeRequest{
			Adapter: "gcash", PiId: "pi_idem", IdempotencyKey: "idem_stable",
			Amount: 1, Currency: "PHP",
		})
		if err != nil {
			t.Fatal(err)
		}
		resp2, err := cli.Charge(ctx, &channelv1.ChargeRequest{
			Adapter: "gcash", PiId: "pi_idem", IdempotencyKey: "idem_stable",
			Amount: 1, Currency: "PHP",
		})
		if err != nil {
			t.Fatal(err)
		}
		if resp1.GetExternalRefNo() != resp2.GetExternalRefNo() {
			t.Fatalf("idempotency violated: %q vs %q",
				resp1.GetExternalRefNo(), resp2.GetExternalRefNo())
		}
	})

	t.Run("refund_partial_then_over", func(t *testing.T) {
		charge, err := cli.Charge(ctx, &channelv1.ChargeRequest{
			Adapter: "gcash", PiId: "pi_rfd", IdempotencyKey: "idem_rfd",
			Amount: 10000, Currency: "PHP",
		})
		if err != nil {
			t.Fatal(err)
		}
		if charge.GetResult() != "succeeded" {
			t.Fatalf("want succeeded, got %s", charge.GetResult())
		}
		ref := charge.GetExternalRefNo()

		r1, err := cli.Refund(ctx, &channelv1.RefundRequest{
			Adapter: "gcash", PiId: "pi_rfd", ExternalRefNo: ref,
			Amount: 4000, IdempotencyKey: "rfd_1", Reason: "customer",
		})
		if err != nil {
			t.Fatal(err)
		}
		if r1.GetResult() != "succeeded" {
			t.Fatalf("first partial refund failed: %s", r1.GetResult())
		}

		r2, err := cli.Refund(ctx, &channelv1.RefundRequest{
			Adapter: "gcash", PiId: "pi_rfd", ExternalRefNo: ref,
			Amount: 8000, IdempotencyKey: "rfd_2", Reason: "over",
		})
		if err != nil {
			t.Fatal(err)
		}
		if r2.GetResult() != "failed" {
			t.Fatalf("over-refund must fail, got %s", r2.GetResult())
		}
	})
}

// spinUpMock starts a mockserver behind an httptest.Server and returns both.
// t.Cleanup handles teardown.
func spinUpMock(t *testing.T, delay time.Duration) (*mockserver.Server, *httptest.Server) {
	t.Helper()
	srv, err := mockserver.New(mockserver.Options{
		Logger:       mockserver.DiscardLogger{},
		WebhookDelay: delay,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts
}
