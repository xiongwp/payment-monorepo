package mlscore

import (
	"context"
	"testing"
)

// 行为指标全 0（SDK 没上报）不应 panic + 不应显著抬升 score。
func TestLogistic_NoBehaviorSignalsBaseline(t *testing.T) {
	s := NewLogisticService()
	r, err := s.Score(context.Background(), Features{
		FingerprintHash:      "abc",
		HardwareConcurrency:  8,
		MouseMovementEntropy: 3.0,
		KeystrokeCount:       30,
		// 所有行为指标 = 0 (默认)
	})
	if err != nil {
		t.Fatalf("score err: %v", err)
	}
	if r.Score > 0.25 {
		t.Fatalf("no behavior signals should not push score, got %.3f", r.Score)
	}
}

// 行为异常 case（鼠标直线 / 无停顿 / 恒速 / 节律机械）应显著抬 score。
func TestLogistic_BotBehaviorRaisesScore(t *testing.T) {
	s := NewLogisticService()
	clean := Features{
		FingerprintHash:           "abc",
		HardwareConcurrency:       8,
		MouseMovementEntropy:      3.0,
		KeystrokeCount:            30,
		MouseAvgSpeedPxPerMs:      0.5,
		MouseSpeedVariance:        0.3,
		MouseAccelerationKurtosis: 4.0,
		MouseStraightnessRatio:    0.4, // 真实用户曲线
		MousePauseCount:           5,
		KeystrokeDwellMean:        80,
		KeystrokeDwellCV:          0.3,
		KeystrokeFlightMean:       120,
		KeystrokeFlightCV:         0.4,
	}
	bot := clean
	bot.MousePauseCount = 0          // 触发 BotBehaviorCombo
	bot.MouseStraightnessRatio = 0.99 // 触发 BotStraightLine
	bot.MouseSpeedVariance = 0.0001   // 触发 LowMouseVariance
	bot.KeystrokeDwellCV = 0.02       // 触发 BotKeystrokeRhythm（配合 flight）
	bot.KeystrokeFlightCV = 0.02

	rClean, _ := s.Score(context.Background(), clean)
	rBot, _ := s.Score(context.Background(), bot)

	if rBot.Score-rClean.Score < 0.2 {
		t.Fatalf("bot behavior delta too small: clean=%.3f bot=%.3f Δ=%.3f",
			rClean.Score, rBot.Score, rBot.Score-rClean.Score)
	}
}

// onnx_features.go 路径：8 个行为特征 + 2 indicator 全在 extractFeature 已知列表，
// 不走 unknown-feature 路径（vec 应为对应值）。
func TestFeaturesToFloat32_BehaviorFeaturesKnown(t *testing.T) {
	f := Features{
		MouseAvgSpeedPxPerMs:      1.5,
		MouseSpeedVariance:        0.2,
		MouseAccelerationKurtosis: 5.0,
		MouseStraightnessRatio:    0.97,
		MousePauseCount:           3,
		KeystrokeDwellMean:        85,
		KeystrokeDwellCV:          0.3,
		KeystrokeFlightMean:       130,
		KeystrokeFlightCV:         0.4,
		KeystrokeCount:            10,
	}
	order := []string{
		"mouse_avg_speed_px_per_ms", "mouse_speed_variance",
		"mouse_acceleration_kurtosis", "mouse_straightness_ratio",
		"mouse_pause_count",
		"keystroke_dwell_mean", "keystroke_dwell_cv",
		"keystroke_flight_mean", "keystroke_flight_cv",
		"bot_behavior_combo", "bot_straight_line",
	}
	vec := featuresToFloat32(f, order, nil)
	if vec[0] != 1.5 {
		t.Errorf("mouse_avg_speed=%v want 1.5", vec[0])
	}
	if vec[4] != 3 {
		t.Errorf("mouse_pause_count=%v want 3", vec[4])
	}
	// bot_behavior_combo: pause_count=3 != 0 → 0
	if vec[9] != 0 {
		t.Errorf("bot_behavior_combo=%v want 0 (pause>0)", vec[9])
	}
	// bot_straight_line: straightness=0.97 > 0.95 + speed > 0 → 1
	if vec[10] != 1 {
		t.Errorf("bot_straight_line=%v want 1", vec[10])
	}
}

// 全 0 输入也不能 panic（行为指标缺失）；score 应等于无 SDK baseline。
func TestLogistic_AllZeroFeaturesNoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Score panicked on empty features: %v", r)
		}
	}()
	s := NewLogisticService()
	_, err := s.Score(context.Background(), Features{})
	if err != nil {
		t.Fatalf("score err: %v", err)
	}
}

// bot_behavior_combo indicator：pause=0 + keystroke>0 → 1
func TestExtractFeature_BotBehaviorComboTrigger(t *testing.T) {
	f := Features{MousePauseCount: 0, KeystrokeCount: 5}
	v, known := extractFeature("bot_behavior_combo", f)
	if !known || v != 1 {
		t.Errorf("expected (1,true) got (%v,%v)", v, known)
	}
	// 反例：pause>0 → 0
	f2 := Features{MousePauseCount: 2, KeystrokeCount: 5}
	v2, _ := extractFeature("bot_behavior_combo", f2)
	if v2 != 0 {
		t.Errorf("pause>0 → expected 0 got %v", v2)
	}
}
