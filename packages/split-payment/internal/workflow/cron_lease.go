// cron_lease.go — SP-AC-7 X3: 多副本 cron lease.
//
// 问题: split-payment 多副本 (e.g. compose scale=2) 时, PayoutCron / HoldUnstickWorker /
// PayoutDispatchWorker 同时跑会重复扫数据 (虽然有 UNIQUE idempotency_key 兜底, 但浪费
// 计算 + 偶尔锁竞争).
//
// 设计:
//   - 用 MySQL 一张轻量表 `cron_lease` 做 leader election.
//   - 列: name PK / holder VARCHAR / leased_until TIMESTAMP.
//   - acquire 用 UPDATE ... WHERE name=? AND (holder IS NULL OR leased_until < NOW())
//     的 CAS 语义抢占; affected_rows == 1 → 抢到.
//   - renew 周期性续约 (interval / 3), 防其他副本误以为 lease 过期.
//
// 限制 / trade-off:
//   - MySQL 单点 lease, 故障时 leader 切换有 30s 窗口期 (lease TTL).
//   - 比 etcd lease 简单很多; 不需要 etcd 依赖.
//   - 单进程 panic 后 lease 自然过期, 不需要主动 release.
package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"go.uber.org/zap"
)

// CronLease 用 MySQL row CAS 实现的 lease.
type CronLease struct {
	DB       *sql.DB
	Name     string        // lease 标识 (e.g. "payout_cron")
	Holder   string        // 通常用 hostname; renew 时透传
	TTL      time.Duration // lease 有效期, 默认 30s
	Log      *zap.Logger

	mu      sync.Mutex
	stopped bool
}

// EnsureCronLeaseSchema 启动期建表 (幂等).
func EnsureCronLeaseSchema(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return nil
	}
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS cron_lease (
			name         VARCHAR(64)  NOT NULL PRIMARY KEY,
			holder       VARCHAR(128) NOT NULL DEFAULT '',
			leased_until DATETIME     NOT NULL DEFAULT '1970-01-01 00:00:00',
			updated_at   DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`)
	return err
}

// DefaultHolder 返进程标识 (hostname + pid).
func DefaultHolder() string {
	h, err := os.Hostname()
	if err != nil {
		h = "unknown"
	}
	return fmt.Sprintf("%s/%d", h, os.Getpid())
}

// TryAcquire 尝试抢 lease. 抢到 → true; 已被别人持有 → false (调用方 sleep + 重试).
//
// SQL: 用 INSERT ... ON DUPLICATE KEY UPDATE + 条件判断 (leased_until < NOW() 或 已是自己).
// MySQL 不支持 INSERT 里带 WHERE; 拆两步:
//   1. INSERT IGNORE 兜底建行 (第一次 lease 用)
//   2. UPDATE ... WHERE name=? AND (holder=? OR leased_until < NOW())
//      affected_rows=1 → 抢到; =0 → 别人持有有效 lease.
func (l *CronLease) TryAcquire(ctx context.Context) (bool, error) {
	if l.DB == nil {
		return false, errors.New("nil db")
	}
	if l.TTL <= 0 {
		l.TTL = 30 * time.Second
	}
	if l.Holder == "" {
		l.Holder = DefaultHolder()
	}
	// step 1: 兜底建行
	_, _ = l.DB.ExecContext(ctx,
		`INSERT IGNORE INTO cron_lease(name, holder, leased_until) VALUES (?, '', ?)`,
		l.Name, time.Unix(0, 0))
	// step 2: CAS update
	res, err := l.DB.ExecContext(ctx,
		`UPDATE cron_lease SET holder=?, leased_until=? WHERE name=? AND (holder=? OR leased_until < NOW())`,
		l.Holder, time.Now().Add(l.TTL), l.Name, l.Holder)
	if err != nil {
		return false, fmt.Errorf("cron_lease CAS: %w", err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// Renew 续约 (Holder 不变, leased_until 推后到 now+TTL). 返 false → lease 已被别人抢走.
func (l *CronLease) Renew(ctx context.Context) (bool, error) {
	res, err := l.DB.ExecContext(ctx,
		`UPDATE cron_lease SET leased_until=? WHERE name=? AND holder=?`,
		time.Now().Add(l.TTL), l.Name, l.Holder)
	if err != nil {
		return false, fmt.Errorf("cron_lease renew: %w", err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// Release 主动释放 (优雅停机). 若已被别人抢, 啥也不做.
func (l *CronLease) Release(ctx context.Context) error {
	l.mu.Lock()
	l.stopped = true
	l.mu.Unlock()
	_, err := l.DB.ExecContext(ctx,
		`UPDATE cron_lease SET holder='', leased_until='1970-01-01' WHERE name=? AND holder=?`,
		l.Name, l.Holder)
	return err
}

// RunWithLease 用 lease 包一个 cron worker.
//
// 行为:
//   - 启动后周期性 (TTL/3) TryAcquire → 抢到 → 起 work goroutine + renew loop.
//   - 已被别人持有 → 静默 sleep 等下次 tick.
//   - work goroutine 退出 (work() return) → 自动 release lease 让别副本接管.
//
// 用法:
//   lease := &CronLease{DB: db, Name: "payout_cron", TTL: 30*time.Second, Log: log}
//   go lease.RunWithLease(ctx, func(c context.Context) { payoutCron.Run(c) })
func (l *CronLease) RunWithLease(ctx context.Context, work func(context.Context)) {
	probeInterval := l.TTL / 3
	if probeInterval <= 0 {
		probeInterval = 10 * time.Second
	}
	if l.Log != nil {
		l.Log.Info("cron lease loop starting",
			zap.String("name", l.Name), zap.String("holder", l.Holder),
			zap.Duration("ttl", l.TTL), zap.Duration("probe", probeInterval))
	}
	t := time.NewTicker(probeInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = l.Release(context.Background())
			return
		default:
		}
		got, err := l.TryAcquire(ctx)
		if err != nil {
			if l.Log != nil {
				l.Log.Warn("cron lease acquire error", zap.String("name", l.Name), zap.Error(err))
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			continue
		}
		if !got {
			// 别副本持有, wait
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			continue
		}
		// 抢到 → 起 work 跟 renew goroutine
		if l.Log != nil {
			l.Log.Info("cron lease acquired, starting work", zap.String("name", l.Name))
		}
		workCtx, workCancel := context.WithCancel(ctx)
		go l.renewLoop(workCtx, probeInterval, workCancel)
		work(workCtx)
		workCancel()
		_ = l.Release(context.Background())
		if l.Log != nil {
			l.Log.Info("cron lease released", zap.String("name", l.Name))
		}
		// work() 返了后, 回到外层 loop 等下次有机会再抢. (一般 work 是 long-running 不会主动返).
	}
}

// renewLoop 续约直到 cancel.
func (l *CronLease) renewLoop(ctx context.Context, interval time.Duration, onLost context.CancelFunc) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ok, err := l.Renew(ctx)
			if err != nil || !ok {
				if l.Log != nil {
					l.Log.Warn("cron lease renew lost; canceling work",
						zap.String("name", l.Name), zap.Bool("renewed", ok), zap.Error(err))
				}
				onLost()
				return
			}
		}
	}
}
