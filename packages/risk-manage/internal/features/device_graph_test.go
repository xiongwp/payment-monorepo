package features

import (
	"context"
	"testing"

	"github.com/xiongwp/risk-manage/internal/engine"
	"github.com/xiongwp/risk-manage/internal/fphash"
	"github.com/xiongwp/risk-manage/internal/store"
)

func TestDeviceGraphExtractor_DistinctCustomersCount(t *testing.T) {
	ls := store.NewMemLinkStore()
	ctx := context.Background()
	// 同一台 device 在过去窗口里关联到 4 个不同 customer
	ls.Link(ctx, "device:dev-abc", "customer:1")
	ls.Link(ctx, "device:dev-abc", "customer:2")
	ls.Link(ctx, "device:dev-abc", "customer:3")
	ls.Link(ctx, "device:dev-abc", "customer:4")

	ex := NewDeviceGraphExtractor(ls)
	txn := &engine.TxnContext{DeviceID: "dev-abc"}
	ex.Enrich(ctx, txn)

	if txn.DeviceDistinctCustomers90d != 4 {
		t.Fatalf("expected 4 distinct customers, got %d", txn.DeviceDistinctCustomers90d)
	}
}

func TestDeviceGraphExtractor_NoDeviceIDLeavesZero(t *testing.T) {
	ls := store.NewMemLinkStore()
	ex := NewDeviceGraphExtractor(ls)
	txn := &engine.TxnContext{} // 无 DeviceID
	ex.Enrich(context.Background(), txn)
	if txn.DeviceDistinctCustomers90d != 0 {
		t.Fatalf("no device id → expected 0, got %d", txn.DeviceDistinctCustomers90d)
	}
}

func TestDeviceGraphExtractor_PreservesPrefilledValue(t *testing.T) {
	// 商户预填了值（来自 metadata）→ extractor 不覆盖
	ls := store.NewMemLinkStore()
	ctx := context.Background()
	ls.Link(ctx, "device:dev-x", "customer:1")
	ex := NewDeviceGraphExtractor(ls)
	txn := &engine.TxnContext{DeviceID: "dev-x", DeviceDistinctCustomers90d: 99}
	ex.Enrich(ctx, txn)
	if txn.DeviceDistinctCustomers90d != 99 {
		t.Fatalf("prefilled value should be preserved, got %d", txn.DeviceDistinctCustomers90d)
	}
}

func TestDeviceGraphExtractor_SimHashComputed(t *testing.T) {
	ex := NewDeviceGraphExtractor(nil) // 不依赖 LinkStore
	txn := &engine.TxnContext{
		CanvasFingerprint:   "px-hash-aaa",
		WebGLRenderer:       "ANGLE (Intel UHD)",
		AudioContextHash:    "audio-0.013",
		FontHash:            "font-hash-x",
		ScreenWxH:           "1920x1080",
		Timezone:            "Asia/Manila",
		Language:            "en-US",
		Platform:            "web",
		UserAgent:           "Chrome/121",
		HardwareConcurrency: 8,
	}
	ex.Enrich(context.Background(), txn)
	if txn.DeviceFingerprintSimHash == 0 {
		t.Fatal("SimHash should be non-zero for non-empty signals")
	}
}

func TestDeviceGraphExtractor_SimHashAllEmptyZero(t *testing.T) {
	ex := NewDeviceGraphExtractor(nil)
	txn := &engine.TxnContext{} // 全空
	ex.Enrich(context.Background(), txn)
	if txn.DeviceFingerprintSimHash != 0 {
		t.Fatalf("empty signals → SimHash must stay 0, got %x",
			txn.DeviceFingerprintSimHash)
	}
}

func TestDeviceGraphExtractor_SimHashStableAcrossSmallChanges(t *testing.T) {
	ex := NewDeviceGraphExtractor(nil)
	base := &engine.TxnContext{
		CanvasFingerprint: "px-aaa", WebGLRenderer: "ANGLE Intel",
		AudioContextHash: "audio-0.013", FontHash: "font-x",
		PluginsHash: "plug-y", ScreenWxH: "1920x1080",
		Timezone: "Asia/Manila", Language: "en-US",
		Platform: "web", UserAgent: "Chrome/121.0",
	}
	upgraded := *base
	upgraded.UserAgent = "Chrome/122.0" // UA 升级
	upgraded.CanvasFingerprint = "px-aaa-v2" // 像素小变

	ex.Enrich(context.Background(), base)
	ex.Enrich(context.Background(), &upgraded)

	if base.DeviceFingerprintSimHash == upgraded.DeviceFingerprintSimHash {
		t.Fatal("after small change SimHash should differ slightly (but match by hamming)")
	}
	d := fphash.HammingDistance(base.DeviceFingerprintSimHash, upgraded.DeviceFingerprintSimHash)
	if d > fphash.DefaultMatchThreshold {
		t.Fatalf("small-change distance %d > threshold %d (lost fuzzy identity)",
			d, fphash.DefaultMatchThreshold)
	}
}
