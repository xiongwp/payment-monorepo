// postgres_policy.go: engine.PolicyStore 的 Postgres 参考实现 + 内存
// cache。**未编译进默认 build**（//go:build pg）。
//
// 用法（删 build tag 后 + 加 pgx 依赖）：
//
//	pool, _ := pgxpool.New(ctx, dsn)
//	ps := store.NewPGPolicyStore(pool, 30*time.Second)
//	engine.SetPolicyStore(ps)
//	ps.StartRefresh(ctx)   // 后台 30s 拉一次全量
//
// Schema：
//
//	CREATE TABLE risk_merchant_policy (
//	    merchant_id    TEXT PRIMARY KEY,
//	    review_min     INT,
//	    deny_min       INT,
//	    disabled_rules JSONB DEFAULT '[]'::jsonb,
//	    weight_overrides JSONB DEFAULT '{}'::jsonb,
//	    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
//	    updated_by     TEXT
//	);
//
// 设计：
//   - Get() 永远查内存 cache（O(1)）；Evaluate 路径 zero DB hit
//   - StartRefresh 后台 ticker 拉全量 → 原子替换 cache（atomic.Pointer）
//   - admin 改完 → POST /admin/policy/invalidate 主动触发一次 refresh，
//     不必等 ticker（Reload 公开方法）
//   - 写一次 PG 错误不触发 cache 替换，沿用上次快照（fail-static）

//go:build pg

package store

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"github.com/xiongwp/risk-manage/internal/engine"
)

type PGPolicyStore struct {
	pool   *pgxpool.Pool
	ttl    time.Duration
	logger *zap.Logger
	cache  atomic.Pointer[map[string]*engine.MerchantPolicy]
	stop   chan struct{}
}

func NewPGPolicyStore(pool *pgxpool.Pool, refresh time.Duration, logger *zap.Logger) *PGPolicyStore {
	if refresh <= 0 {
		refresh = 30 * time.Second
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	s := &PGPolicyStore{pool: pool, ttl: refresh, logger: logger, stop: make(chan struct{})}
	empty := map[string]*engine.MerchantPolicy{}
	s.cache.Store(&empty)
	return s
}

// Get 从 atomic cache 读，永远不阻塞 / 不查 DB。
func (s *PGPolicyStore) Get(merchantID string) *engine.MerchantPolicy {
	if merchantID == "" {
		return nil
	}
	m := s.cache.Load()
	if m == nil {
		return nil
	}
	return (*m)[merchantID]
}

// StartRefresh 启动后台定时全量拉。第一次同步拉一次，确保启动后 cache 不空。
func (s *PGPolicyStore) StartRefresh(ctx context.Context) {
	if err := s.Reload(ctx); err != nil {
		s.logger.Warn("policy initial load failed", zap.Error(err))
	}
	go func() {
		t := time.NewTicker(s.ttl)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if err := s.Reload(ctx); err != nil {
					s.logger.Warn("policy refresh failed", zap.Error(err))
				}
			case <-s.stop:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
}

// Reload 同步全量拉 + 原子替换 cache。admin invalidate 也调它。
func (s *PGPolicyStore) Reload(ctx context.Context) error {
	rows, err := s.pool.Query(ctx,
		`SELECT merchant_id, review_min, deny_min, disabled_rules, weight_overrides
           FROM risk_merchant_policy`)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := make(map[string]*engine.MerchantPolicy, 256)
	for rows.Next() {
		var (
			p              engine.MerchantPolicy
			reviewMin      *int
			denyMin        *int
			disabledJSON   []byte
			weightsJSON    []byte
		)
		if err := rows.Scan(&p.MerchantID, &reviewMin, &denyMin, &disabledJSON, &weightsJSON); err != nil {
			s.logger.Warn("policy row scan failed", zap.Error(err))
			continue
		}
		if reviewMin != nil {
			p.ReviewMin = *reviewMin
		}
		if denyMin != nil {
			p.DenyMin = *denyMin
		}
		if len(disabledJSON) > 0 {
			_ = json.Unmarshal(disabledJSON, &p.DisabledRules)
		}
		if len(weightsJSON) > 0 {
			_ = json.Unmarshal(weightsJSON, &p.WeightOverrides)
		}
		out[p.MerchantID] = &p
	}
	s.cache.Store(&out)
	s.logger.Info("policy cache reloaded", zap.Int("count", len(out)))
	return nil
}

// Stop 停后台 refresh goroutine。fx OnStop hook 调一下即可。
func (s *PGPolicyStore) Stop() {
	select {
	case <-s.stop:
		// already closed
	default:
		close(s.stop)
	}
}
