// Package repo: shardscan 把跨分片串行扫改成并发扫的小工具。
//
// 跨分片定时任务（ListExpired / Reconcile / 聚合）原本 100 分片串行，每个
// shard 一次 DB 往返就 100ms × 100 = 10s 起步，远超 worker tick 间隔。并发
// 之后总耗时 ≈ max(单 shard 耗时)。
//
// 限流：默认 10 并发避免压垮连接池（10 个物理 DB × 默认 conn pool ≈ 50 = 500
// 个 conn 上限；100 并发会立刻把池用完）。
package repo

import (
	"context"
	"errors"
	"sync"
)

// firstNonCancelErr 在并发任务的 error 列表里找出**真正触发**的错误。
//
// 为什么需要：fail-fast 时 cancel(ctx) 会让其它仍在跑的 goroutine 收到
// context.Canceled。如果仅按 errs[] 顺序找第一个非 nil，可能拿到的是
// 派生的 ctx err 而不是原始触发原因，让上层报错信息错乱。
//
// 策略：优先返回非 context.Canceled / context.DeadlineExceeded 的错误；
// 全是这两类时 fall back 到第一个非 nil（保证 nil 不会被误返）。
func firstNonCancelErr(errs []error) error {
	var fallback error
	for _, e := range errs {
		if e == nil {
			continue
		}
		if errors.Is(e, context.Canceled) || errors.Is(e, context.DeadlineExceeded) {
			if fallback == nil {
				fallback = e
			}
			continue
		}
		return e
	}
	return fallback
}

// shardScanConcurrency 并发上限。和物理 DB 数对齐 = 每库一条扫描线程。
// 调小可以减压；调大要确保 DB 连接池能撑住。
const shardScanConcurrency = 10

// fanOutShards 对每个 shard 并发跑 fn。任一 fn 报错即中断（context 被 cancel），
// 已经返回的部分结果**不会**被合并；调用方拿到 (nil, err) 不要复用 partial。
//
// fn 必须是只读 / 幂等的：取消后 fn 内部走 ctx.Done() 早退，避免再写副作用。
//
// 结果按 shard 顺序拼接（与原来串行行为一致），便于调用方还原可观察到的
// stable order。limit 是输出截断（不影响每 shard 自己 LIMIT；那是查询层职责）。
func fanOutShards[T any](
	ctx context.Context,
	shards [][2]int,
	limit int,
	fn func(ctx context.Context, dbIdx, tblIdx int) ([]T, error),
) ([]T, error) {
	if len(shards) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make([][]T, len(shards))
	errs := make([]error, len(shards))
	sem := make(chan struct{}, shardScanConcurrency)
	var wg sync.WaitGroup
	for i, sh := range shards {
		i, sh := i, sh
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			part, err := fn(ctx, sh[0], sh[1])
			if err != nil {
				errs[i] = err
				cancel() // 早退所有其它 goroutine
				return
			}
			results[i] = part
		}()
	}
	wg.Wait()

	if e := firstNonCancelErr(errs); e != nil {
		return nil, e
	}

	// 顺序拼接 + 限流截断
	var out []T
	for _, r := range results {
		out = append(out, r...)
		if limit > 0 && len(out) >= limit {
			out = out[:limit]
			break
		}
	}
	return out, nil
}

// fanOutShardsSum 跨分片求和的并发版（int64 / 整数累加单测最常见）。
// 任一 fn 报错即返回。
func fanOutShardsSum(
	ctx context.Context,
	shards [][2]int,
	fn func(ctx context.Context, dbIdx, tblIdx int) (int64, error),
) (int64, error) {
	if len(shards) == 0 {
		return 0, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	parts := make([]int64, len(shards))
	errs := make([]error, len(shards))
	sem := make(chan struct{}, shardScanConcurrency)
	var wg sync.WaitGroup
	for i, sh := range shards {
		i, sh := i, sh
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			v, err := fn(ctx, sh[0], sh[1])
			if err != nil {
				errs[i] = err
				cancel()
				return
			}
			parts[i] = v
		}()
	}
	wg.Wait()

	if e := firstNonCancelErr(errs); e != nil {
		return 0, e
	}
	var total int64
	for _, v := range parts {
		total += v
	}
	return total, nil
}
