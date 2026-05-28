//go:build feast

package mlscore

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/xiongwp/risk-manage/internal/featurestore"
)

// TestNoopProvider verifies the empty-feature path used when Feast disabled.
func TestNoopProvider(t *testing.T) {
	p := NoopProvider{}
	f, err := p.Provide(context.Background(), "c1", "d1", "1.1.1.1", "m1")
	if err != nil {
		t.Fatalf("noop returned err: %v", err)
	}
	if f.MerchantID != "" || f.MouseSpeedVariance != 0 {
		t.Fatalf("noop should return zero Features, got %+v", f)
	}
}

// TestBuildProvider_Disabled cfg.Enabled=false → NoopProvider。
func TestBuildProvider_Disabled(t *testing.T) {
	p, err := BuildProvider(FeastConfig{Enabled: false, Addr: "feast:6566"})
	if err != nil {
		t.Fatalf("BuildProvider err: %v", err)
	}
	if _, ok := p.(NoopProvider); !ok {
		t.Fatalf("expected NoopProvider, got %T", p)
	}
}

// TestBuildProvider_EmptyAddr cfg.Enabled=true 但 addr 空 → 也 Noop（防误配）。
func TestBuildProvider_EmptyAddr(t *testing.T) {
	p, err := BuildProvider(FeastConfig{Enabled: true, Addr: ""})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if _, ok := p.(NoopProvider); !ok {
		t.Fatalf("expected NoopProvider on empty addr, got %T", p)
	}
}

// TestBuildProvider_Enabled cfg 正常 → FeastFeatureProvider 实例 + 默认 timeout。
// 用本地 loopback addr 避免真连接（grpc.NewClient 不立即拨号）。
func TestBuildProvider_Enabled(t *testing.T) {
	p, err := BuildProvider(FeastConfig{
		Enabled:        true,
		Addr:           "127.0.0.1:6566",
		Project:        "risk",
		TimeoutMs:      30,
		FeatureService: "risk_realtime_v1",
	})
	if err != nil {
		t.Fatalf("BuildProvider err: %v", err)
	}
	ffp, ok := p.(*FeastFeatureProvider)
	if !ok {
		t.Fatalf("expected *FeastFeatureProvider, got %T", p)
	}
	if ffp.timeout != 30*time.Millisecond {
		t.Fatalf("expected timeout 30ms, got %v", ffp.timeout)
	}
	if ffp.cli == nil || ffp.cli.FeatureService != "risk_realtime_v1" {
		t.Fatalf("FeatureService not propagated; got %+v", ffp.cli)
	}
}

// TestFeastFeatureProvider_NilClient 防御：nil client → 明确错误。
func TestFeastFeatureProvider_NilClient(t *testing.T) {
	p := &FeastFeatureProvider{cli: nil, timeout: 10 * time.Millisecond}
	_, err := p.Provide(context.Background(), "c1", "d1", "1.1.1.1", "m1")
	if err == nil {
		t.Fatal("expected nil-client error")
	}
}

// TestFeastFeatureProvider_NotEnabled stub client（client 字段 nil）→ 所有 4 个
// fetch 都返 ErrFeastNotEnabled，整体 fallback。
func TestFeastFeatureProvider_NotEnabled(t *testing.T) {
	// FeastClient 直接构造（client 字段为 nil → 触发 stub 路径）
	cli := &featurestore.FeastClient{FeatureService: "risk_realtime_v1"}
	p := NewFeastFeatureProvider(cli, 20*time.Millisecond)
	_, err := p.Provide(context.Background(), "cust1", "dev1", "1.1.1.1", "m1")
	if err == nil {
		t.Fatal("expected error when feast client not enabled")
	}
	if !strings.Contains(err.Error(), "feast") {
		t.Fatalf("expected wrapped feast err, got: %v", err)
	}
}

// TestFeastFeatureProvider_EmptyIDs 空 entity ID → 跳过对应 fetch，整体不报错。
// 4 个 ID 都空时所有 fetch fn 返 (nil, nil)，merged 为空，返 zero Features。
func TestFeastFeatureProvider_EmptyIDs(t *testing.T) {
	cli := &featurestore.FeastClient{FeatureService: "risk_realtime_v1"}
	p := NewFeastFeatureProvider(cli, 20*time.Millisecond)
	feats, err := p.Provide(context.Background(), "", "", "", "")
	if err != nil {
		t.Fatalf("empty IDs should not error: %v", err)
	}
	if feats.MouseSpeedVariance != 0 || feats.IPProxy {
		t.Fatalf("expected zero feats for empty IDs, got %+v", feats)
	}
}

// TestMergeFeatures 验证 overlay 优先 + Extra map merge 语义。
func TestMergeFeatures(t *testing.T) {
	base := Features{
		MerchantID:         "m1",
		MouseSpeedVariance: 100.0,
		Extra:              map[string]string{"card_bin": "411111"},
	}
	overlay := Features{
		MouseSpeedVariance: 250.0,    // 覆盖
		KeystrokeDwellCV:   0.35,     // 新增
		IPProxy:            true,     // 覆盖（bool 优先 true）
		Extra: map[string]string{
			"paid_count_90d": "12",
			"card_bin":       "555555", // 覆盖 base 的
		},
	}
	out := MergeFeatures(base, overlay)
	if out.MerchantID != "m1" {
		t.Errorf("MerchantID lost: %q", out.MerchantID)
	}
	if out.MouseSpeedVariance != 250.0 {
		t.Errorf("MouseSpeedVariance not overridden: %v", out.MouseSpeedVariance)
	}
	if out.KeystrokeDwellCV != 0.35 {
		t.Errorf("KeystrokeDwellCV not added: %v", out.KeystrokeDwellCV)
	}
	if !out.IPProxy {
		t.Errorf("IPProxy not set")
	}
	if out.Extra["card_bin"] != "555555" {
		t.Errorf("Extra not overridden: %v", out.Extra)
	}
	if out.Extra["paid_count_90d"] != "12" {
		t.Errorf("Extra not merged: %v", out.Extra)
	}
}

// TestMergeFeatures_ZeroOverlay overlay 全零字段不应抹掉 base 已有值。
func TestMergeFeatures_ZeroOverlay(t *testing.T) {
	base := Features{
		MouseSpeedVariance: 100.0,
		MousePauseCount:    5,
	}
	out := MergeFeatures(base, Features{})
	if out.MouseSpeedVariance != 100.0 || out.MousePauseCount != 5 {
		t.Fatalf("zero overlay should not erase base: %+v", out)
	}
}

// TestProvide_TimeoutCtx 上层 ctx 已 cancel → 应立刻返错。
func TestProvide_TimeoutCtx(t *testing.T) {
	cli := &featurestore.FeastClient{FeatureService: "risk_realtime_v1"}
	p := NewFeastFeatureProvider(cli, 20*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := p.Provide(ctx, "cust1", "", "", "")
	if err == nil {
		t.Fatal("expected error with cancelled ctx")
	}
}

// TestProvide_Integration 真集成测试；ENV 没设跳过。
// CI 默认不跑——需要 docker compose -f deploy/feast/docker-compose.feast.yml --profile feast up。
func TestProvide_Integration(t *testing.T) {
	addr := os.Getenv("FEAST_TEST_ADDR")
	if addr == "" {
		t.Skip("FEAST_TEST_ADDR not set; skipping live Feast integration test")
	}
	p, err := BuildProvider(FeastConfig{
		Enabled: true, Addr: addr, Project: "risk", TimeoutMs: 200,
		FeatureService: "risk_realtime_v1",
	})
	if err != nil {
		t.Fatalf("BuildProvider: %v", err)
	}
	feats, err := p.Provide(context.Background(), "cust_42", "dev_42", "203.0.0.42", "m_5")
	if err != nil {
		// 真 ML proto 未 vendor 时预期 ErrFeastNotEnabled
		if errors.Is(err, featurestore.ErrFeastNotEnabled) {
			t.Skip("feast proto not vendored yet; integration deferred to ML team wiring")
		}
		t.Fatalf("Provide: %v", err)
	}
	t.Logf("got feats: %+v", feats)
}
