package mlscore

import (
	"context"
	"errors"
	"testing"
)

// fakeService 给 ensemble 测试用的固定分数 service。
type fakeService struct {
	score float64
	err   error
}

func (s fakeService) Score(_ context.Context, _ Features) (Result, error) {
	return Result{Score: s.score, ModelVer: "fake"}, s.err
}

func TestEnsemble_WeightedMean(t *testing.T) {
	e := NewEnsemble("v1", AggregateWeightedMean,
		EnsembleMember{Name: "a", Service: fakeService{score: 0.2}, Weight: 1.0},
		EnsembleMember{Name: "b", Service: fakeService{score: 0.8}, Weight: 3.0},
	)
	r, err := e.Score(context.Background(), Features{})
	if err != nil {
		t.Fatal(err)
	}
	// (0.2*1 + 0.8*3) / 4 = 0.65
	if r.Score < 0.64 || r.Score > 0.66 {
		t.Fatalf("expected ~0.65; got %v", r.Score)
	}
	if r.ModelVer != "v1" {
		t.Fatalf("modelVer wrong: %q", r.ModelVer)
	}
}

func TestEnsemble_Max(t *testing.T) {
	e := NewEnsemble("vmax", AggregateMax,
		EnsembleMember{Name: "a", Service: fakeService{score: 0.3}},
		EnsembleMember{Name: "b", Service: fakeService{score: 0.9}},
		EnsembleMember{Name: "c", Service: fakeService{score: 0.5}},
	)
	r, _ := e.Score(context.Background(), Features{})
	if r.Score != 0.9 {
		t.Fatalf("expected max 0.9; got %v", r.Score)
	}
}

func TestEnsemble_PartialFailureSkipsMember(t *testing.T) {
	e := NewEnsemble("v1", AggregateWeightedMean,
		EnsembleMember{Name: "good", Service: fakeService{score: 0.5}},
		EnsembleMember{Name: "bad", Service: fakeService{err: errors.New("boom")}, Weight: 100},
	)
	r, err := e.Score(context.Background(), Features{})
	if err != nil {
		t.Fatal(err)
	}
	// bad service skipped → 只剩 good，权重 1，分 0.5
	if r.Score != 0.5 {
		t.Fatalf("expected 0.5 (good only); got %v", r.Score)
	}
}

func TestEnsemble_AllFailReturnsErr(t *testing.T) {
	e := NewEnsemble("v1", AggregateWeightedMean,
		EnsembleMember{Service: fakeService{err: errors.New("a")}},
		EnsembleMember{Service: fakeService{err: errors.New("b")}},
	)
	if _, err := e.Score(context.Background(), Features{}); err == nil {
		t.Fatal("expected error when all members fail")
	}
}

func TestEnsemble_ZeroWeightTreatedAsOne(t *testing.T) {
	e := NewEnsemble("v1", AggregateWeightedMean,
		EnsembleMember{Service: fakeService{score: 0.4}, Weight: 0},
		EnsembleMember{Service: fakeService{score: 0.6}, Weight: 0},
	)
	r, _ := e.Score(context.Background(), Features{})
	// 等权 → 平均 0.5
	if r.Score < 0.49 || r.Score > 0.51 {
		t.Fatalf("expected 0.5; got %v", r.Score)
	}
}

func TestEnsemble_ScoreClamp(t *testing.T) {
	// 成员 score 超 [0,1] 时 ensemble 兜底 clamp（防御 remote 模型乱报）
	e := NewEnsemble("v1", AggregateWeightedMean,
		EnsembleMember{Service: fakeService{score: 1.5}, Weight: 1},
	)
	r, _ := e.Score(context.Background(), Features{})
	if r.Score != 1.0 {
		t.Fatalf("expected clamped to 1.0; got %v", r.Score)
	}
	e2 := NewEnsemble("v1", AggregateMax,
		EnsembleMember{Service: fakeService{score: -0.3}, Weight: 1},
	)
	r2, _ := e2.Score(context.Background(), Features{})
	if r2.Score != 0 {
		t.Fatalf("expected clamped to 0.0; got %v", r2.Score)
	}
}

func TestEnsemble_NilService(t *testing.T) {
	var e *EnsembleService
	r, err := e.Score(context.Background(), Features{})
	if err != nil || r.Score != 0 {
		t.Fatalf("nil service should be safe; got %v / %v", r, err)
	}
}

// ── Behavior scorer tests ──

func TestBehaviorScorer_BaselineLowScore(t *testing.T) {
	b := NewBehaviorScorer()
	// 普通用户：mouse_entropy=3, typing_cv=0.6, time=10s, has fingerprint
	r, _ := b.Score(context.Background(), Features{
		MouseMovementEntropy: 3.0,
		KeystrokeCount:       50,
		TypingRhythmCV:       0.6,
		TimeToCheckoutMs:     10000,
		FingerprintHash:      "abc",
		HardwareConcurrency:  8,
	})
	if r.Score > 0.3 {
		t.Fatalf("baseline user should be low risk; got %v", r.Score)
	}
}

func TestBehaviorScorer_BotPatternHigh(t *testing.T) {
	b := NewBehaviorScorer()
	// 模拟 bot：无鼠标 + 恒定打字 + 极速结账 + 无 fingerprint
	r, _ := b.Score(context.Background(), Features{
		MouseMovementEntropy: 0,
		KeystrokeCount:       20,
		TypingRhythmCV:       0.02,    // bot
		TimeToCheckoutMs:     400,     // ultra rapid
		FingerprintHash:      "",
		HardwareConcurrency:  1,
		WebGLRenderer:        "SwiftShader",
	})
	if r.Score < 0.85 {
		t.Fatalf("bot pattern should be very high; got %v", r.Score)
	}
}

func TestBehaviorScorer_HeadlessRendererTriggers(t *testing.T) {
	b := NewBehaviorScorer()
	r, _ := b.Score(context.Background(), Features{
		WebGLRenderer:    "Mozilla phantomjs/Headless",
		FingerprintHash:  "abc",
		MouseMovementEntropy: 3,
		KeystrokeCount:   50,
		TypingRhythmCV:   0.6,
		TimeToCheckoutMs: 10000,
	})
	// HeadlessAgent (1.5) - intercept(2.0) - 无加号信号 → sigmoid(-0.5) ~ 0.378
	// 强信号但没到 high；阈值降到 0.35 验证 bump
	if r.Score < 0.35 {
		t.Fatalf("headless agent should significantly raise risk; got %v", r.Score)
	}
}

func TestBehaviorScorer_NilSafe(t *testing.T) {
	var b *BehaviorScorer
	r, _ := b.Score(context.Background(), Features{})
	if r.Score != 0 {
		t.Fatal("nil scorer should be 0")
	}
	b.SetWeights(BehaviorWeights{})
}

func TestBehaviorScorer_SetWeights(t *testing.T) {
	b := NewBehaviorScorer()
	b.SetWeights(BehaviorWeights{NoMouse: 100}) // 极端权重测 hot reload 生效
	r, _ := b.Score(context.Background(), Features{
		MouseMovementEntropy: 0,
		KeystrokeCount:       50,
	})
	// sigmoid(-2 + 100) ≈ 1.0
	if r.Score < 0.99 {
		t.Fatalf("expected ~1.0 with extreme weight; got %v", r.Score)
	}
}
