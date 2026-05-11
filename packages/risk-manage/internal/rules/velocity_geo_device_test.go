package rules

import (
	"context"
	"testing"
	"time"
)

func TestVelocity_CountLimit(t *testing.T) {
	v := NewVGDVelocityChecker([]VGDVelocityRule{
		{Key: "user_id", Window: time.Hour, MaxCount: 3, ScoreOnHit: 50},
	})
	now := time.Now()
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		hits := v.Evaluate(ctx, VGDTxnContext{UserID: "u1", AmountMinor: 100, At: now})
		if len(hits) != 0 {
			t.Fatalf("attempt %d: expected no hits, got %v", i, hits)
		}
	}
	// 第 4 次超阈
	hits := v.Evaluate(ctx, VGDTxnContext{UserID: "u1", AmountMinor: 100, At: now})
	if len(hits) == 0 {
		t.Fatal("expected velocity hit on 4th call")
	}
}

func TestVelocity_AmountLimit(t *testing.T) {
	v := NewVGDVelocityChecker([]VGDVelocityRule{
		{Key: "card_token", Window: 24 * time.Hour, MaxAmount: 1000_00, ScoreOnHit: 70},
	})
	now := time.Now()
	ctx := context.Background()
	v.Evaluate(ctx, VGDTxnContext{CardToken: "tok_1", AmountMinor: 500_00, At: now})
	v.Evaluate(ctx, VGDTxnContext{CardToken: "tok_1", AmountMinor: 400_00, At: now})
	// 之前 900, 现在 +200 = 1100 > 1000 → hit
	hits := v.Evaluate(ctx, VGDTxnContext{CardToken: "tok_1", AmountMinor: 200_00, At: now})
	if len(hits) == 0 {
		t.Fatal("expected amount over hit")
	}
}

func TestGeo_ImpossibleTravel(t *testing.T) {
	g := NewVGDGeoChecker(800) // 800 km/h
	ctx := context.Background()
	now := time.Now()
	// 上海 -> 5min 后到纽约 → 速度 ~135000 km/h, 必然 hit
	tc1 := VGDTxnContext{UserID: "u1", At: now}
	g.Evaluate(ctx, tc1, 31.23, 121.47) // Shanghai
	tc2 := VGDTxnContext{UserID: "u1", At: now.Add(5 * time.Minute)}
	hits := g.Evaluate(ctx, tc2, 40.71, -74.00) // NYC
	if len(hits) == 0 {
		t.Fatal("expected geo impossible_travel hit")
	}
}

func TestGeo_SameCity(t *testing.T) {
	g := NewVGDGeoChecker(800)
	ctx := context.Background()
	now := time.Now()
	g.Evaluate(ctx, VGDTxnContext{UserID: "u1", At: now}, 31.23, 121.47)
	hits := g.Evaluate(ctx, VGDTxnContext{UserID: "u1", At: now.Add(10 * time.Minute)},
		31.25, 121.50)
	if len(hits) != 0 {
		t.Fatalf("same-city should pass, got %v", hits)
	}
}

func TestDevice_Blacklist(t *testing.T) {
	d := NewVGDDeviceChecker([]string{"bad-device"}, 5)
	hits := d.Evaluate(context.Background(), VGDTxnContext{
		UserID: "u1", DeviceID: "bad-device",
	})
	if len(hits) == 0 || hits[0].Score != 100 {
		t.Fatalf("expected blacklist hit, got %v", hits)
	}
}

func TestDevice_NewForUser(t *testing.T) {
	d := NewVGDDeviceChecker(nil, 5)
	ctx := context.Background()
	// 第 1 次见, 标 new
	hits := d.Evaluate(ctx, VGDTxnContext{UserID: "u1", DeviceID: "dev1"})
	found := false
	for _, h := range hits {
		if h.Name == "device:new_for_user" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected new_for_user hit")
	}
	// 第 2 次同 user 同 device, 不再 new
	hits = d.Evaluate(ctx, VGDTxnContext{UserID: "u1", DeviceID: "dev1"})
	for _, h := range hits {
		if h.Name == "device:new_for_user" {
			t.Fatal("should not flag known device as new")
		}
	}
}

func TestDevice_SharedAcrossUsers(t *testing.T) {
	d := NewVGDDeviceChecker(nil, 3)
	ctx := context.Background()
	for i := 1; i <= 4; i++ {
		hits := d.Evaluate(ctx, VGDTxnContext{
			UserID: "u" + string(rune('0'+i)), DeviceID: "shared-dev",
		})
		if i >= 3 {
			// 第 3+ 个 user 用同设备, 应 hit shared
			ok := false
			for _, h := range hits {
				if h.Name == "device:shared" {
					ok = true
				}
			}
			if !ok {
				t.Errorf("attempt %d: expected device:shared hit", i)
			}
		}
	}
}

func TestEngine_Combine_Decline(t *testing.T) {
	e := &VGDEngine{
		Device: NewVGDDeviceChecker([]string{"bad-dev"}, 5),
	}
	d := e.Score(context.Background(), VGDTxnContext{
		UserID: "u1", DeviceID: "bad-dev",
	})
	if d.Outcome != "decline" {
		t.Errorf("blacklist device should decline, got %s", d.Outcome)
	}
}

func TestEngine_Combine_Allow(t *testing.T) {
	e := &VGDEngine{
		Device:   NewVGDDeviceChecker(nil, 5),
		Velocity: NewVGDVelocityChecker(nil),
	}
	d := e.Score(context.Background(), VGDTxnContext{
		UserID: "u1", DeviceID: "dev1", AmountMinor: 100, At: time.Now(),
	})
	// 新设备 score=25 → allow (< 50)
	if d.Outcome != "allow" {
		t.Errorf("low score should allow, got %s score=%d", d.Outcome, d.Score)
	}
}
