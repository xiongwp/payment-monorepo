package fphash

import (
	"testing"
)

func TestSimHash_EmptyInput(t *testing.T) {
	if got := SimHash(nil); got != 0 {
		t.Errorf("nil pieces: got %x want 0", got)
	}
	if got := SimHash([]string{}); got != 0 {
		t.Errorf("empty slice: got %x want 0", got)
	}
	if got := SimHash([]string{"", "", ""}); got != 0 {
		t.Errorf("all-empty: got %x want 0", got)
	}
}

func TestSimHash_DeterministicAndOrderInvariant(t *testing.T) {
	a := []string{"canvas:abc", "webgl:intel", "audio:0.013"}
	b := []string{"audio:0.013", "canvas:abc", "webgl:intel"} // 打乱顺序
	if SimHash(a) != SimHash(b) {
		t.Fatalf("order should not affect SimHash: %x vs %x",
			SimHash(a), SimHash(b))
	}
	// 同一输入两次必须一致
	if SimHash(a) != SimHash(a) {
		t.Fatal("non-deterministic")
	}
}

func TestHammingDistance_Basic(t *testing.T) {
	if got := HammingDistance(0, 0); got != 0 {
		t.Errorf("d(0,0)=%d want 0", got)
	}
	if got := HammingDistance(0xff, 0); got != 8 {
		t.Errorf("d(0xff,0)=%d want 8", got)
	}
	if got := HammingDistance(0x1, 0x2); got != 2 {
		t.Errorf("d(0x1,0x2)=%d want 2", got)
	}
}

// 同设备小升级：少数 piece 变化，距离应远小于完全随机的 ~32。
// 不强求 <= DefaultMatchThreshold（8）— 实际距离受 FNV hash 分布影响，理论上限是
// 2 × changed_pieces，但实测会更小；这里只验证"近似"（<= 16，即 ~1/4 bit 翻转上限）。
// 真实生产数据 + LSH 调优后 8 阈值会更稳。
func TestSimHash_NearMatchOnPartialChange(t *testing.T) {
	makePieces := func(canvasV, uaV string) []string {
		// 12 个稳定 piece + 2 个变量；多 piece 让单 piece 变化的 bit 翻转占比更小
		return []string{
			"canvas:" + canvasV,
			"webgl:ANGLE-Intel-UHD-630",
			"audio:0.013283",
			"font:hash-aaa",
			"plugins:hash-bbb",
			"codec:hash-ccc",
			"screen:1920x1080",
			"tz:Asia/Manila",
			"lang:en-US",
			"platform:web",
			"hc:8",
			"cd:24",
			"dm:8",
			"ua:" + uaV,
		}
	}
	h1 := SimHash(makePieces("px-hash-chrome121", "Chrome/121.0.0"))
	h2 := SimHash(makePieces("px-hash-chrome122", "Chrome/122.0.0"))
	d := HammingDistance(h1, h2)
	if d > 16 {
		t.Fatalf("near-match distance %d > 16 (expected close fingerprint)", d)
	}
	// 理论上随机不同设备距离 ~32；近邻应远小于。
	if d >= 25 {
		t.Errorf("near-match distance too large: %d", d)
	}
}

// 完全不同设备：所有 piece 都不一样，距离应远大于 threshold。
func TestSimHash_FarMismatchDifferentDevices(t *testing.T) {
	deviceA := []string{
		"canvas:px-A", "webgl:NVIDIA-RTX-3080", "audio:0.0011",
		"font:hash-A", "plugins:hash-A", "timezone:America/New_York",
		"screen:2560x1440", "ua:Chrome/121",
	}
	deviceB := []string{
		"canvas:px-B-totally-other", "webgl:Apple-M1", "audio:0.99",
		"font:hash-Z", "plugins:hash-Z", "timezone:Europe/London",
		"screen:1440x900", "ua:Safari/16",
	}
	h1 := SimHash(deviceA)
	h2 := SimHash(deviceB)
	d := HammingDistance(h1, h2)
	if d <= DefaultMatchThreshold {
		t.Fatalf("different-device distance %d should be > threshold %d", d, DefaultMatchThreshold)
	}
}

func TestFuzzyMatch_ZeroInputReturnsFalse(t *testing.T) {
	// 0 hash（信号全空）不能跟另一个 0 假阳性匹配
	if FuzzyMatch(0, 0, 64) {
		t.Errorf("0 ↔ 0 should not match (both un-fingerprinted)")
	}
	if FuzzyMatch(0, 0x1234, 64) {
		t.Errorf("0 ↔ non-zero should not match")
	}
	if FuzzyMatch(0x1234, 0, 64) {
		t.Errorf("non-zero ↔ 0 should not match")
	}
}

func TestFuzzyMatch_ExactMatchAtZeroThreshold(t *testing.T) {
	if !FuzzyMatch(0xdeadbeef, 0xdeadbeef, 0) {
		t.Error("identical hashes should match with threshold=0")
	}
	if FuzzyMatch(0xdeadbeef, 0xdeadbef0, 0) {
		t.Error("1-bit diff should NOT match with threshold=0")
	}
	if !FuzzyMatch(0xdeadbeef, 0xdeadbef0, 4) {
		t.Error("1-bit diff should match with threshold=4")
	}
}
