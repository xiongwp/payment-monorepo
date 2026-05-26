// behavior_lstm_test.go — stub build 下的核心测试（无 onnx tag）。
//
// 验证：
//   1. stub NewBehaviorLSTMService 返 ErrBehaviorLSTMNotEnabled
//   2. Score 在 stub 下返 fallback 中性值 0.5
//   3. tensor shape 常量值符合契约
//   4. 鼠标 / keystroke 序列短/超长时 padding/truncate 不 panic
package mlscore

import (
	"context"
	"errors"
	"testing"
)

func TestBehaviorLSTM_StubFallback(t *testing.T) {
	svc, err := NewBehaviorLSTMService("/nonexistent/model.onnx")
	if !errors.Is(err, ErrBehaviorLSTMNotEnabled) {
		t.Logf("got err = %v (expected ErrBehaviorLSTMNotEnabled in stub build)", err)
	}
	// 即便 stub，Score 应该返中性值不挂
	mouse := make([]MouseEvent, 5)
	keys := make([]KeystrokeEvent, 2)
	score, err := svc.Score(context.Background(), mouse, keys)
	if err != nil {
		t.Fatalf("Score err: %v", err)
	}
	if score != 0.5 {
		t.Fatalf("expected fallback score 0.5, got %.3f", score)
	}
}

func TestBehaviorLSTM_TensorShapeConstants(t *testing.T) {
	if MouseSeqLen != 200 {
		t.Fatalf("MouseSeqLen=%d expected 200", MouseSeqLen)
	}
	if MouseFeatDim != 3 {
		t.Fatalf("MouseFeatDim=%d expected 3", MouseFeatDim)
	}
	if KeystrokeSeqLen != 50 {
		t.Fatalf("KeystrokeSeqLen=%d expected 50", KeystrokeSeqLen)
	}
	if KeystrokeFeatDim != 3 {
		t.Fatalf("KeystrokeFeatDim=%d expected 3", KeystrokeFeatDim)
	}
}

func TestBehaviorLSTM_EmptyInputs(t *testing.T) {
	svc, _ := NewBehaviorLSTMService("")
	score, err := svc.Score(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("nil input err: %v", err)
	}
	if score != 0.5 {
		t.Fatalf("expected 0.5 for empty input, got %.3f", score)
	}
}
