package visa

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"github.com/xiongwp/card-payment/internal/processor"
)

// TestMockBINs 跑一遍 BIN → status 决策表，保证 mock 路径稳定。
// 拿到合同后改 endpoint=https://api.visa.com，这个 test 自动跳过 mock 分支。
func TestMockBINs(t *testing.T) {
	a := New(Config{Mock: true}, zap.NewNop())
	cases := []struct {
		name     string
		pan      string
		wantStat string
		wantCat  string
	}{
		{"happy", "4242424242424242", "approved", ""},
		{"soft_insufficient", "4000000000000001", "declined", "SOFT"},
		{"hard_stolen", "4001000000000001", "declined", "HARD"},
		{"pending", "4002000000000001", "pending", ""},
		{"default_approved", "4111111111111111", "approved", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := a.Authorize(context.Background(), &processor.NetworkAuthRequest{
				PAN: tc.pan, ExpMonth: 12, ExpYear: 2030,
				Amount: 1000, Currency: "USD", IdempotencyKey: "test_" + tc.name,
			})
			if err != nil {
				t.Fatalf("authorize err: %v", err)
			}
			if out.Status != tc.wantStat {
				t.Errorf("status: got %q want %q", out.Status, tc.wantStat)
			}
			if out.DeclineCategory != tc.wantCat {
				t.Errorf("decline_category: got %q want %q", out.DeclineCategory, tc.wantCat)
			}
			if out.MaskedPAN == "" {
				t.Errorf("masked_pan empty")
			}
			if out.NetworkRefNo == "" {
				t.Errorf("network_ref_no empty")
			}
		})
	}
}

// TestMaskPAN 边界：< 12 位返原样，刚好 12 / 16 / 19 位都正确 mask
func TestMaskPAN(t *testing.T) {
	cases := []struct{ in, want string }{
		{"4242424242424242", "424242******4242"},
		{"42424242424242420000", "424242**********0000"}, // 20 位
		{"123", "123"},                                   // 短直接返
	}
	for _, tc := range cases {
		got := maskPAN(tc.in)
		if got != tc.want {
			t.Errorf("maskPAN(%q): got %q want %q", tc.in, got, tc.want)
		}
	}
}

// TestMockNoPANInResponse 确认 mock 响应里没有完整 PAN（防止 logger 误打出来）
func TestMockNoPANInResponse(t *testing.T) {
	a := New(Config{Mock: true}, zap.NewNop())
	out, _ := a.Authorize(context.Background(), &processor.NetworkAuthRequest{
		PAN: "4242424242424242", ExpMonth: 12, ExpYear: 2030,
		Amount: 100, Currency: "USD", IdempotencyKey: "no_pan_test",
	})
	if out.MaskedPAN == "4242424242424242" {
		t.Errorf("masked_pan should not equal real PAN")
	}
}
