package shadow

import (
	"context"
	"testing"
)

// V1 ID 仍 round-trip OK（向后兼容是硬要求）。
func TestDecodeAccountID_V1RoundTrip(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		currency, accountType, gtbl, biz int
		seq                              int64
	}{
		{608, 1, 42, 0, 1},          // PHP / USER / shard 42
		{840, 2, 99, 9999, 9_999_999}, // USD / MERCHANT / max
	}
	for _, c := range cases {
		id, err := EncodeAccountID(ctx, c.currency, c.accountType, c.gtbl, c.biz, c.seq)
		if err != nil {
			t.Fatalf("V1 encode err: %v", err)
		}
		if AccountIDLayoutVersion(id) != 1 {
			t.Fatalf("V1 ID %d misdetected as V%d", id, AccountIDLayoutVersion(id))
		}
		isShadow, cur, at, gtbl, biz, seq := DecodeAccountID(id)
		if isShadow || cur != c.currency || at != c.accountType ||
			gtbl != c.gtbl || biz != c.biz || seq != c.seq {
			t.Errorf("V1 round-trip failed: enc(%v) → %d → dec(%v,%d,%d,%d,%d,%d)",
				c, id, isShadow, cur, at, gtbl, biz, seq)
		}
	}
}

// V2 ID round-trip：含 globalTbl 0-999 + accountType 1-9。
func TestEncodeAccountIDV2_RoundTrip(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		currency, accountType, gtbl, biz int
		seq                              int64
	}{
		{608, 1, 0, 0, 1},
		{840, 9, 999, 9999, 9_999_999}, // 各字段最大值
		{978, 5, 500, 100, 12345},      // 中间值
	}
	for _, c := range cases {
		id, err := EncodeAccountIDV2(ctx, c.currency, c.accountType, c.gtbl, c.biz, c.seq)
		if err != nil {
			t.Fatalf("V2 encode err: %v (case %v)", err, c)
		}
		if AccountIDLayoutVersion(id) != 2 {
			t.Fatalf("V2 ID %d misdetected as V%d", id, AccountIDLayoutVersion(id))
		}
		isShadow, cur, at, gtbl, biz, seq := DecodeAccountID(id)
		if isShadow || cur != c.currency || at != c.accountType ||
			gtbl != c.gtbl || biz != c.biz || seq != c.seq {
			t.Errorf("V2 round-trip failed: enc(%v) → %d → dec(%v,%d,%d,%d,%d,%d)",
				c, id, isShadow, cur, at, gtbl, biz, seq)
		}
	}
}

// V2 shadow flag round-trip。
func TestEncodeAccountIDV2_Shadow(t *testing.T) {
	ctx := WithShadow(context.Background(), true)
	id, err := EncodeAccountIDV2(ctx, 608, 1, 500, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !IsShadowAccountIDLayout(id) {
		t.Fatalf("V2 shadow flag not detected on id=%d", id)
	}
	if AccountIDLayoutVersion(id) != 2 {
		t.Fatalf("V2 shadow detection wrong: got version %d", AccountIDLayoutVersion(id))
	}
	isShadow, _, _, gtbl, _, _ := DecodeAccountID(id)
	if !isShadow || gtbl != 500 {
		t.Fatalf("V2 shadow decode wrong: shadow=%v gtbl=%d", isShadow, gtbl)
	}
}

// V2 越界 reject —— accountType > 9 / globalTbl > 999 必须 error。
func TestEncodeAccountIDV2_OutOfRange(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name                             string
		currency, accountType, gtbl, biz int
		seq                              int64
		wantErr                          bool
	}{
		{"accountType > 9", 608, 10, 0, 0, 1, true},
		{"globalTbl > 999", 608, 1, 1000, 0, 1, true},
		{"currency > 999", 1000, 1, 0, 0, 1, true},
		{"business > 9999", 608, 1, 0, 10000, 1, true},
		{"seq <= 0", 608, 1, 0, 0, 0, true},
		{"all valid max", 999, 9, 999, 9999, 9_999_999, false},
	}
	for _, c := range cases {
		_, err := EncodeAccountIDV2(ctx, c.currency, c.accountType, c.gtbl, c.biz, c.seq)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: wantErr=%v got err=%v", c.name, c.wantErr, err)
		}
	}
}

// V1 / V2 同字段编码出的 ID 不能撞 — 通过 version 高位区分。
func TestEncodeAccountID_V1V2_NoCollision(t *testing.T) {
	ctx := context.Background()
	v1ID, _ := EncodeAccountID(ctx, 608, 1, 42, 0, 1)
	v2ID, _ := EncodeAccountIDV2(ctx, 608, 1, 42, 0, 1)
	if v1ID == v2ID {
		t.Fatalf("V1 and V2 collide: %d", v1ID)
	}
	if v2ID-v1ID != accountIDV2VersionMul {
		t.Fatalf("V2-V1 delta should equal version flag (%d), got %d", accountIDV2VersionMul, v2ID-v1ID)
	}
}

// AccountIDDBIndex / AccountIDTableIndex 自动按 layout 版本分发。
func TestAccountIDDBIndex_Dispatches(t *testing.T) {
	ctx := context.Background()
	// V1: globalTbl 42 → dbIdx = 42/10 = 4
	v1ID, _ := EncodeAccountID(ctx, 608, 1, 42, 0, 1)
	if got := AccountIDDBIndex(v1ID); got != 4 {
		t.Errorf("V1 dbIdx: got %d want 4", got)
	}
	if got := AccountIDTableIndex(v1ID); got != 42 {
		t.Errorf("V1 tblIdx: got %d want 42", got)
	}
	// V2: globalTbl 543 → dbIdx = 543/100 = 5
	v2ID, _ := EncodeAccountIDV2(ctx, 608, 1, 543, 0, 1)
	if got := AccountIDDBIndex(v2ID); got != 5 {
		t.Errorf("V2 dbIdx: got %d want 5", got)
	}
	if got := AccountIDTableIndex(v2ID); got != 543 {
		t.Errorf("V2 tblIdx: got %d want 543", got)
	}
}

// IsShadowAccountIDLayout 在 V1 / V2 上都正确。
func TestIsShadowAccountIDLayout_BothVersions(t *testing.T) {
	bg := context.Background()
	sh := WithShadow(bg, true)

	v1Main, _ := EncodeAccountID(bg, 608, 1, 0, 0, 1)
	v1Shadow, _ := EncodeAccountID(sh, 608, 1, 0, 0, 1)
	v2Main, _ := EncodeAccountIDV2(bg, 608, 1, 0, 0, 1)
	v2Shadow, _ := EncodeAccountIDV2(sh, 608, 1, 0, 0, 1)

	cases := []struct {
		id   int64
		want bool
		desc string
	}{
		{v1Main, false, "V1 main"},
		{v1Shadow, true, "V1 shadow"},
		{v2Main, false, "V2 main"},
		{v2Shadow, true, "V2 shadow"},
	}
	for _, c := range cases {
		if got := IsShadowAccountIDLayout(c.id); got != c.want {
			t.Errorf("%s id=%d: shadow=%v want %v", c.desc, c.id, got, c.want)
		}
	}
}
