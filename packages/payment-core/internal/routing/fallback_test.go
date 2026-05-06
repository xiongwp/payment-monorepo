package routing

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestFallbackRouter_GetFallbackChain(t *testing.T) {
	logger := zap.NewNop()
	fr := NewFallbackRouter(logger)

	// 准备测试数据
	cfg := []byte(`{
  "rules": [
    {
      "country": "PH",
      "payment_method": "GCASH",
      "bin": "",
      "currency": "PHP",
      "priority": [
        {"adapter": "gcash", "weight": 100},
        {"adapter": "paymongo", "weight": 50},
        {"adapter": "xendit", "weight": 25}
      ]
    },
    {
      "country": "PH",
      "payment_method": "CARD",
      "bin": "",
      "currency": "PHP",
      "priority": [
        {"adapter": "card", "weight": 100},
        {"adapter": "stripe", "weight": 50}
      ]
    }
  ]
}`)

	err := fr.UpdateConfig(cfg)
	if err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}

	// 测试用例
	tests := []struct {
		name    string
		primary string
		input   FallbackInput
		want    []string
	}{
		{
			name:    "GCASH primary, should have fallback to paymongo/xendit",
			primary: "gcash",
			input: FallbackInput{
				Country:       "PH",
				PaymentMethod: "GCASH",
				Currency:      "PHP",
			},
			want: []string{"gcash", "paymongo", "xendit"},
		},
		{
			name:    "CARD primary, should have fallback to stripe",
			primary: "card",
			input: FallbackInput{
				Country:       "PH",
				PaymentMethod: "CARD",
				Currency:      "PHP",
			},
			want: []string{"card", "stripe"},
		},
		{
			name:    "Unknown payment method, no fallback",
			primary: "maya",
			input: FallbackInput{
				Country:       "PH",
				PaymentMethod: "UNKNOWN",
				Currency:      "PHP",
			},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fr.GetFallbackChain(tt.primary, tt.input)
			if !sliceEqual(got, tt.want) {
				t.Errorf("GetFallbackChain() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRetryScheduler_NextRetryTime(t *testing.T) {
	scheduler := &RetryScheduler{}

	// 验证退避时间
	tests := []struct {
		attempt      int
		expectedMin  time.Duration
		expectedMax  time.Duration
	}{
		{0, 25 * time.Second, 35 * time.Second},   // 30s ± 5s
		{1, 2*time.Minute - 10*time.Second, 2*time.Minute + 10*time.Second},  // 2m ± 10s
		{2, 15*time.Minute - 1*time.Minute, 15*time.Minute + 1*time.Minute},  // 15m ± 1m
		{3, 2*time.Hour - 10*time.Minute, 2*time.Hour + 10*time.Minute},     // 2h ± 10m
		{999, 2*time.Hour - 10*time.Minute, 2*time.Hour + 10*time.Minute},   // 最后一个时间
	}

	for _, tt := range tests {
		t.Run(string(rune(tt.attempt)), func(t *testing.T) {
			before := time.Now()
			nextTime := scheduler.NextRetryTime(tt.attempt)
			after := time.Now()

			// nextTime 应该在 [now + expectedMin, now + expectedMax] 范围内
			diff := nextTime.Sub(before)
			if diff < tt.expectedMin {
				t.Errorf("attempt %d: backoff too short: %v < %v", tt.attempt, diff, tt.expectedMin)
			}
			if diff > tt.expectedMax+100*time.Millisecond { // 允许 100ms 误差（测试执行时间）
				t.Errorf("attempt %d: backoff too long: %v > %v", tt.attempt, diff, tt.expectedMax)
			}
		})
	}
}

func TestMemoryRetryQueue(t *testing.T) {
	ctx := context.Background()
	q := NewMemoryRetryQueue()

	// 测试 Enqueue
	task1 := &RetryTask{
		ID:              "task_1",
		PaymentIntentID: "pi_001",
		IdempotencyKey:  "idem_001",
		Attempt:         0,
		NextRetryAt:     time.Now().Add(-10 * time.Second), // 已过期，应被 Dequeue
	}
	task2 := &RetryTask{
		ID:              "task_2",
		PaymentIntentID: "pi_002",
		IdempotencyKey:  "idem_002",
		Attempt:         0,
		NextRetryAt:     time.Now().Add(1 * time.Hour), // 未来时间
	}

	if err := q.Enqueue(ctx, task1); err != nil {
		t.Fatalf("Enqueue task1 failed: %v", err)
	}
	if err := q.Enqueue(ctx, task2); err != nil {
		t.Fatalf("Enqueue task2 failed: %v", err)
	}

	// 测试 Dequeue
	tasks, err := q.Dequeue(ctx, 10)
	if err != nil {
		t.Fatalf("Dequeue failed: %v", err)
	}
	if len(tasks) != 1 {
		t.Errorf("expected 1 task, got %d", len(tasks))
	}
	if tasks[0].ID != "task_1" {
		t.Errorf("expected task_1, got %s", tasks[0].ID)
	}

	// 测试 MarkRetry
	err = q.MarkRetry(ctx, "task_1", 1, time.Now().Add(2*time.Minute), "network timeout")
	if err != nil {
		t.Fatalf("MarkRetry failed: %v", err)
	}

	// 验证更新
	tasks, _ = q.Dequeue(ctx, 10)
	if len(tasks) != 0 {
		t.Errorf("after MarkRetry, should have 0 overdue tasks, got %d", len(tasks))
	}

	// 测试 MarkSuccess
	err = q.MarkSuccess(ctx, "task_1")
	if err != nil {
		t.Fatalf("MarkSuccess failed: %v", err)
	}

	// 验证删除
	err = q.MarkSuccess(ctx, "task_1")
	if err == nil {
		t.Error("expected error for non-existent task")
	}
}

func TestRetryQueueStats(t *testing.T) {
	ctx := context.Background()
	q := NewMemoryRetryQueue()

	for i := 0; i < 5; i++ {
		task := &RetryTask{
			ID:              string(rune(i)),
			PaymentIntentID: "pi_00" + string(rune(i)),
			NextRetryAt:     time.Now().Add(-1 * time.Second), // 已过期
		}
		q.Enqueue(ctx, task)
	}

	for i := 5; i < 10; i++ {
		task := &RetryTask{
			ID:              string(rune(i)),
			PaymentIntentID: "pi_00" + string(rune(i)),
			NextRetryAt:     time.Now().Add(1 * time.Hour), // 未来
		}
		q.Enqueue(ctx, task)
	}

	stats := q.Stats()
	if stats["total"] != 10 {
		t.Errorf("expected total=10, got %v", stats["total"])
	}
	if stats["overdue"] != 5 {
		t.Errorf("expected overdue=5, got %v", stats["overdue"])
	}
	if stats["pending"] != 5 {
		t.Errorf("expected pending=5, got %v", stats["pending"])
	}
}

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
