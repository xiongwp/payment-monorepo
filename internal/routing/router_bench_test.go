package routing

import "testing"

// 10 条典型 PH 规则，接近 production config 的体量。
func benchmarkRules() []Rule {
	return []Rule{
		{Priority: 100, Country: "PH", PaymentMethod: "GCASH", Adapter: "gcash"},
		{Priority: 100, Country: "PH", PaymentMethod: "MAYA", Adapter: "maya"},
		{Priority: 100, Country: "PH", PaymentMethod: "GRABPAY", Adapter: "grabpay"},
		{Priority: 100, Country: "PH", PaymentMethod: "COINS_PH", Adapter: "coinsph"},
		{Priority: 100, Country: "PH", PaymentMethod: "SHOPEEPAY", Adapter: "shopeepay"},
		{Priority: 100, Country: "PH", PaymentMethod: "INSTAPAY", AmountMax: 5000000, Adapter: "instapay"},
		{Priority: 110, Country: "PH", PaymentMethod: "INSTAPAY", AmountMin: 5000001, Adapter: "pesonet"},
		{Priority: 100, Country: "PH", PaymentMethod: "PESONET", Adapter: "pesonet"},
		{Priority: 50, Merchant: "vip", Country: "PH", PaymentMethod: "GCASH", Adapter: "gcash_vip"},
		{Priority: 999, Country: "PH", Adapter: "gcash"},
	}
}

// Route 的热路径基准：命中前面的规则（第 1 条）。
func BenchmarkRouter_Route_FirstHit(b *testing.B) {
	r := NewRouter(benchmarkRules())
	in := MatchInput{Country: "PH", PaymentMethod: "GCASH", Amount: 10000}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = r.Route(in)
	}
}

// 命中靠后的规则（需要走完大半个规则集才命中）。
func BenchmarkRouter_Route_MidHit(b *testing.B) {
	r := NewRouter(benchmarkRules())
	in := MatchInput{Country: "PH", PaymentMethod: "INSTAPAY", Amount: 6000000}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = r.Route(in)
	}
}

// 命中兜底规则（最坏情况：跑完整个规则集）。
func BenchmarkRouter_Route_Fallback(b *testing.B) {
	r := NewRouter(benchmarkRules())
	in := MatchInput{Country: "PH", PaymentMethod: "UNKNOWN", Amount: 10000}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = r.Route(in)
	}
}

// 商户专属规则命中（测 Merchant 比较分支）。
func BenchmarkRouter_Route_MerchantSpecific(b *testing.B) {
	r := NewRouter(benchmarkRules())
	in := MatchInput{Merchant: "vip", Country: "PH", PaymentMethod: "GCASH", Amount: 10000}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = r.Route(in)
	}
}

// 输入大小写混杂：验证 ToUpper 预归一化的代价。
func BenchmarkRouter_Route_MixedCase(b *testing.B) {
	r := NewRouter(benchmarkRules())
	in := MatchInput{Country: "ph", PaymentMethod: "gcash", Amount: 10000}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = r.Route(in)
	}
}

// 并发压测：多 goroutine 同时 Route，验证 atomic.Pointer 读取无锁竞争。
func BenchmarkRouter_Route_Parallel(b *testing.B) {
	r := NewRouter(benchmarkRules())
	in := MatchInput{Country: "PH", PaymentMethod: "GCASH", Amount: 10000}
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = r.Route(in)
		}
	})
}
