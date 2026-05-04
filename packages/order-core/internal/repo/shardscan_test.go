package repo

import (
	"context"
	"errors"
	"sort"
	"sync/atomic"
	"testing"
	"time"
)

func makeShards(n int) [][2]int {
	out := make([][2]int, n)
	for i := 0; i < n; i++ {
		out[i] = [2]int{i / 10, i % 10}
	}
	return out
}

// fanOutShards 顺序拼接 + limit 截断 + 并发执行。
func TestFanOutShards_OrderedConcat(t *testing.T) {
	shards := makeShards(20)
	got, err := fanOutShards(context.Background(), shards, 0,
		func(_ context.Context, db, tbl int) ([]int, error) {
			// 每个 shard 返回 [db*10+tbl] 一项，顺序应保持 0..19
			return []int{db*10 + tbl}, nil
		})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 20 {
		t.Fatalf("expected 20 items, got %d", len(got))
	}
	if !sort.IntsAreSorted(got) {
		t.Fatalf("expected sorted 0..19 (shard order preserved), got %v", got)
	}
}

func TestFanOutShards_LimitTruncates(t *testing.T) {
	shards := makeShards(20)
	got, err := fanOutShards(context.Background(), shards, 5,
		func(_ context.Context, _, _ int) ([]int, error) {
			return []int{1, 1}, nil // 每 shard 2 个，需要 3 shard 就够 5 个
		})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("expected truncated to 5, got %d", len(got))
	}
}

func TestFanOutShards_FailFast(t *testing.T) {
	shards := makeShards(50)
	var calls atomic.Int32
	wantErr := errors.New("boom")
	_, err := fanOutShards(context.Background(), shards, 0,
		func(ctx context.Context, db, tbl int) ([]int, error) {
			calls.Add(1)
			if db == 2 && tbl == 0 {
				return nil, wantErr
			}
			// 让 cancel 有时间传播
			select {
			case <-time.After(50 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return []int{1}, nil
		})
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected wantErr, got %v", err)
	}
	// 不强求 calls 数；只要 cancel 能传，多数 shard 早退
}

func TestFanOutShards_EmptyInput(t *testing.T) {
	got, err := fanOutShards(context.Background(), nil, 0,
		func(_ context.Context, _, _ int) ([]int, error) { return nil, nil })
	if err != nil || got != nil {
		t.Fatalf("expected nil/nil, got %v / %v", got, err)
	}
}

func TestFanOutShardsSum_Adds(t *testing.T) {
	shards := makeShards(10)
	total, err := fanOutShardsSum(context.Background(), shards,
		func(_ context.Context, db, tbl int) (int64, error) {
			return int64(db*10 + tbl + 1), nil // 1..10 sum=55
		})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if total != 55 {
		t.Fatalf("expected 55, got %d", total)
	}
}

func TestFanOutShardsSum_FailFast(t *testing.T) {
	shards := makeShards(20)
	wantErr := errors.New("nope")
	_, err := fanOutShardsSum(context.Background(), shards,
		func(_ context.Context, db, _ int) (int64, error) {
			if db == 0 {
				return 0, wantErr
			}
			return 1, nil
		})
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected wantErr, got %v", err)
	}
}
