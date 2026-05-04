package rules

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/xiongwp/risk-manage/internal/engine"
)

func newBotRule(t *testing.T, cfg string) engine.Rule {
	t.Helper()
	r, err := BotDetectionFactory()("bot1", "bot", true, json.RawMessage(cfg))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestBotDetection_HeadlessChromeWithPuppeteer(t *testing.T) {
	r := newBotRule(t, `{"min_signals":2,"min_time_ms":1000}`)
	hit := r.Evaluate(context.Background(), &engine.TxnContext{
		WebGLRenderer:       "Google SwiftShader",
		HardwareConcurrency: 1,
		ScreenWxH:           "1024x768",
		TimeToCheckoutMs:    500,
	})
	if hit == nil {
		t.Fatal("expected bot hit")
	}
	if hit.Decision != engine.Review {
		t.Fatalf("default decision review, got %s", hit.Decision)
	}
}

func TestBotDetection_DenyMode(t *testing.T) {
	r := newBotRule(t, `{"min_signals":2,"decision":"deny"}`)
	hit := r.Evaluate(context.Background(), &engine.TxnContext{
		UserAgent: "Mozilla/5.0 (HeadlessChrome/120.0.0.0)",
		ScreenWxH: "",
	})
	if hit == nil {
		t.Fatal("expected hit")
	}
	if hit.Decision != engine.Deny {
		t.Fatalf("expected DENY, got %s", hit.Decision)
	}
}

func TestBotDetection_RealUserShouldNotHit(t *testing.T) {
	r := newBotRule(t, `{"min_signals":2,"min_time_ms":1000}`)
	hit := r.Evaluate(context.Background(), &engine.TxnContext{
		WebGLRenderer:        "ANGLE (Apple, Apple M2 Pro)",
		HardwareConcurrency:  10,
		ScreenWxH:            "2560x1440",
		MouseMovementEntropy: 3.2,
		ClickIntervalMs:      450,
		TimeToCheckoutMs:     12_000,
		KeystrokeCount:       80,
		UserAgent:            "Mozilla/5.0 ... Chrome/120.0",
	})
	if hit != nil {
		t.Fatalf("real user should not hit, got %+v", hit)
	}
}

func TestBotDetection_BelowThreshold(t *testing.T) {
	r := newBotRule(t, `{"min_signals":3}`)
	// 只命中一个信号 → score=1 < 3 → 不命中
	hit := r.Evaluate(context.Background(), &engine.TxnContext{
		HardwareConcurrency: 1,
	})
	if hit != nil {
		t.Fatalf("single signal should not hit at min_signals=3, got %+v", hit)
	}
}
