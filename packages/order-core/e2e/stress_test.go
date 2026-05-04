//go:build e2e
// 三层压测 / 基准：order-core → payment-core → payment-channel。
//
// 目的：
//  1. 跑一个 go test -bench 的数据点：单 payment Charge 全链路延迟 + 吞吐
//  2. 跑一个并发 soak：在 N goroutine × M iter 下不出错、不 deadlock
//  3. 给路由器的幂等重放打一个 hot path 基准
//
// 运行：
//   go test -tags=e2e -run '^$' -bench=. -benchmem ./order-core/e2e/...
//   go test -tags=e2e -run TestThreeTier_ConcurrentLoad ./order-core/e2e/...
package e2e

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ordchannel "github.com/xiongwp/order-core/internal/channel"
)

// BenchmarkThreeTier_ChargeGCash 每次迭代发一次 Charge，走 GCash (requires_action) 路径。
// 每次 pi_id 独立，避免被 payment-channel 的 UNIQUE(idempotency_key) 截胡。
func BenchmarkThreeTier_ChargeGCash(b *testing.B) {
	st := spinUp(b)
	ctx := context.Background()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		piID := fmt.Sprintf("pi_bench_%08d", i)
		_, err := st.pc.Charge(ctx, ordchannel.PaymentRequest{
			PaymentIntentID: piID,
			Amount:          10000,
			Currency:        "PHP",
			PaymentMethod:   "GCASH",
			ReturnURL:       "https://cashier.example.com/ret",
			Metadata:        map[string]string{"country": "PH"},
		})
		if err != nil {
			b.Fatalf("iter %d: %v", i, err)
		}
	}
}

// BenchmarkThreeTier_ChargeSync 同步成功路径的基准（InstaPay 走 succeeded）。
// 比 requires_action 少一次 RequiredAction 构造，能看出 proto 打包开销占比。
func BenchmarkThreeTier_ChargeSync(b *testing.B) {
	st := spinUp(b)
	ctx := context.Background()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		piID := fmt.Sprintf("pi_bench_sync_%08d", i)
		_, err := st.pc.Charge(ctx, ordchannel.PaymentRequest{
			PaymentIntentID: piID,
			Amount:          500000,
			Currency:        "PHP",
			PaymentMethod:   "INSTAPAY",
			Metadata:        map[string]string{"country": "PH"},
		})
		if err != nil {
			b.Fatalf("iter %d: %v", i, err)
		}
	}
}

// BenchmarkThreeTier_IdempotentReplay 同一个 pi_id 重复打，走 payment-channel 的
// UNIQUE(adapter, idem) 回放路径。用来看 "第二次 Charge" 有多便宜。
func BenchmarkThreeTier_IdempotentReplay(b *testing.B) {
	st := spinUp(b)
	ctx := context.Background()
	req := ordchannel.PaymentRequest{
		PaymentIntentID: "pi_replay_0001",
		Amount:          10000, Currency: "PHP", PaymentMethod: "GCASH",
		Metadata: map[string]string{"country": "PH"},
	}
	// 预热一次，让后续全部走 replay
	if _, err := st.pc.Charge(ctx, req); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := st.pc.Charge(ctx, req); err != nil {
			b.Fatalf("iter %d: %v", i, err)
		}
	}
}

// TestThreeTier_ConcurrentLoad 非基准的并发 soak：N 个 goroutine 各打 M 次，全部不报错，
// 总时长不超过 softLimit。用来抓 race / deadlock 而不是度量绝对吞吐。
func TestThreeTier_ConcurrentLoad(t *testing.T) {
	const (
		concurrency = 32
		perWorker   = 100
		softLimit   = 20 * time.Second
	)
	st := spinUp(t)

	var (
		ok     atomic.Int64
		errCnt atomic.Int64
		wg     sync.WaitGroup
	)
	start := time.Now()
	wg.Add(concurrency)
	for w := 0; w < concurrency; w++ {
		w := w
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), softLimit)
			defer cancel()
			for i := 0; i < perWorker; i++ {
				piID := fmt.Sprintf("pi_load_%02d_%04d", w, i)
				_, err := st.pc.Charge(ctx, ordchannel.PaymentRequest{
					PaymentIntentID: piID,
					Amount:          10000,
					Currency:        "PHP",
					PaymentMethod:   "GCASH",
					Metadata:        map[string]string{"country": "PH"},
				})
				if err != nil {
					errCnt.Add(1)
					t.Logf("worker=%d iter=%d err=%v", w, i, err)
					continue
				}
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	total := int64(concurrency * perWorker)
	if ok.Load() != total {
		t.Fatalf("got ok=%d err=%d want ok=%d", ok.Load(), errCnt.Load(), total)
	}
	if elapsed > softLimit {
		t.Fatalf("took %s (> %s soft limit)", elapsed, softLimit)
	}
	t.Logf("concurrency=%d iters=%d elapsed=%s throughput=%.0f rps",
		concurrency, total, elapsed, float64(total)/elapsed.Seconds())
}

// TestThreeTier_MixedMethods 混合 payment_method 在同一个 stack 里并发跑，
// 验证 router 无共享状态 / 无串扰。
func TestThreeTier_MixedMethods(t *testing.T) {
	st := spinUp(t)
	methods := []struct {
		name    string
		method  string
		wantRes ordchannel.PaymentResultType
	}{
		{"gcash", "GCASH", ordchannel.PaymentResultRequiresAction},
		{"maya", "MAYA", ordchannel.PaymentResultRequiresAction},
		{"instapay", "INSTAPAY", ordchannel.PaymentResultSucceeded},
	}

	var wg sync.WaitGroup
	for _, m := range methods {
		m := m
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for i := 0; i < 30; i++ {
				piID := fmt.Sprintf("pi_mix_%s_%03d", m.name, i)
				resp, err := st.pc.Charge(ctx, ordchannel.PaymentRequest{
					PaymentIntentID: piID,
					Amount:          10000, Currency: "PHP", PaymentMethod: m.method,
					ReturnURL: "https://cashier.example.com/ret",
					Metadata:  map[string]string{"country": "PH"},
				})
				if err != nil {
					t.Errorf("%s iter %d: %v", m.name, i, err)
					return
				}
				if resp.ResultType != m.wantRes {
					t.Errorf("%s iter %d: got %s want %s", m.name, i, resp.ResultType, m.wantRes)
					return
				}
			}
		}()
	}
	wg.Wait()
}

