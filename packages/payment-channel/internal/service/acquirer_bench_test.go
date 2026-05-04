package service

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"github.com/xiongwp/payment-channel/internal/channel"
)

// 基线：fakeAdapter 直接返回 success 的 Charge 全链路耗时。
// 测 save-first-then-call 编排（两次 repo.Insert/UpdateResult + JSON marshal）
// 对单次 Charge 的成本。
func BenchmarkAcquirerService_Charge(b *testing.B) {
	ad := newFakeAdapter("gcash")
	reg := newFakeRegistry(ad)
	txRepo := newMemTxRepo()
	svc := NewAcquirerService(reg, txRepo, &memIDIssuer{}, zap.NewNop())

	ctx := context.Background()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		req := &channel.ChargeRequest{
			PiID:           "pi_bench",
			IdempotencyKey: keyFor(i), // 每次不同，确保走完整路径（非幂等回放）
			Amount:         10000,
			Currency:       "PHP",
		}
		if _, err := svc.Charge(ctx, "gcash", req); err != nil {
			b.Fatal(err)
		}
	}
}

// 幂等回放热路径：同 idempotency_key 重复调用。
// 测 FindByIdem 命中 + replayCharge 反序列化。生产里收银台误点、网络重试时
// 非常常见。
func BenchmarkAcquirerService_Charge_IdempotentReplay(b *testing.B) {
	ad := newFakeAdapter("gcash")
	reg := newFakeRegistry(ad)
	txRepo := newMemTxRepo()
	svc := NewAcquirerService(reg, txRepo, &memIDIssuer{}, zap.NewNop())

	ctx := context.Background()
	req := &channel.ChargeRequest{
		PiID: "pi_bench", IdempotencyKey: "idem-fixed", Amount: 10000, Currency: "PHP",
	}
	// 先跑一次让 tx 落盘
	if _, err := svc.Charge(ctx, "gcash", req); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := svc.Charge(ctx, "gcash", req); err != nil {
			b.Fatal(err)
		}
	}
}

// Refund 热路径。
func BenchmarkAcquirerService_Refund(b *testing.B) {
	ad := newFakeAdapter("gcash")
	reg := newFakeRegistry(ad)
	txRepo := newMemTxRepo()
	svc := NewAcquirerService(reg, txRepo, &memIDIssuer{}, zap.NewNop())

	ctx := context.Background()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		req := &channel.RefundRequest{
			PiID: "pi_bench", ExternalRefNo: "ref_bench", Amount: 5000, IdempotencyKey: keyFor(i),
		}
		if _, err := svc.Refund(ctx, "gcash", req); err != nil {
			b.Fatal(err)
		}
	}
}

// Query 读路径：不写 acquirer_tx，只透传到 adapter。
func BenchmarkAcquirerService_Query(b *testing.B) {
	ad := newFakeAdapter("gcash")
	reg := newFakeRegistry(ad)
	txRepo := newMemTxRepo()
	svc := NewAcquirerService(reg, txRepo, &memIDIssuer{}, zap.NewNop())

	ctx := context.Background()
	req := &channel.QueryRequest{PiID: "pi_bench", ExternalRefNo: "ref_bench"}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := svc.Query(ctx, "gcash", req); err != nil {
			b.Fatal(err)
		}
	}
}

func keyFor(i int) string {
	b := make([]byte, 0, 16)
	b = append(b, "idem-"...)
	b = appendInt(b, int64(i))
	return string(b)
}

func appendInt(b []byte, n int64) []byte {
	if n == 0 {
		return append(b, '0')
	}
	var tmp [20]byte
	i := len(tmp)
	for n > 0 {
		i--
		tmp[i] = byte('0' + n%10)
		n /= 10
	}
	return append(b, tmp[i:]...)
}
