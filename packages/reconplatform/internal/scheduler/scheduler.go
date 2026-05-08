// Package scheduler 把脚本按三种方式触发：
//   1. Cron：脚本配 schedule="*/15 * * * *" 等
//   2. Stream：脚本配 triggers=["order-core:payment_intents", "*"]，事件
//      到达即触发（XREADGROUP from recon:stream:events）
//   3. Manual：admin web POST /scripts/<id>/run（不走本包，由 api server 直调 loader）
//
// 设计：
//   - 一个 Scheduler 内嵌 cron + stream 两个 worker
//   - 启动期 + 每分钟 reload 已加载脚本列表，diff 计算重建 cron entries
//   - stream 一直阻塞 XREADGROUP，避免轮询；ACK 在脚本运行完成后
package scheduler

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/robfig/cron/v3"
	"go.uber.org/zap"

	"reconcile-system/internal/script"
	"reconcile-system/internal/store"
)

// Scheduler 调度器。
type Scheduler struct {
	loader   *script.Loader
	scriptDB *script.Store
	searcher *store.Searcher
	r        redis.UniversalClient
	logger   *zap.Logger

	cronEng    *cron.Cron
	mu         sync.Mutex
	cronEntries map[string]cron.EntryID // scriptID → entryID
}

// New 构造（不立刻启动）。
func New(loader *script.Loader, scriptDB *script.Store, searcher *store.Searcher, r redis.UniversalClient, logger *zap.Logger) *Scheduler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Scheduler{
		loader:     loader,
		scriptDB:   scriptDB,
		searcher:   searcher,
		r:          r,
		logger:     logger,
		cronEng:    cron.New(cron.WithSeconds()),
		cronEntries: make(map[string]cron.EntryID),
	}
}

// Run 阻塞，直到 ctx 取消。每分钟 reload 一次脚本列表（增删 cron entries），
// 同时跑一个 goroutine 消费 Redis Stream。
func (s *Scheduler) Run(ctx context.Context) error {
	s.cronEng.Start()
	defer s.cronEng.Stop()

	// 启动期立即 reload
	s.reload(ctx)

	// stream consumer goroutine
	go s.runStreamConsumer(ctx)

	// 周期 reload
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			s.reload(ctx)
		}
	}
}

// reload 同步当前 loader 已加载脚本到 cron schedule。
func (s *Scheduler) reload(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()

	scripts := s.loader.List()
	wantIDs := make(map[string]bool, len(scripts))
	for _, sc := range scripts {
		wantIDs[sc.ID] = true
		if sc.Schedule == "" {
			// 不需要 cron
			if eid, exists := s.cronEntries[sc.ID]; exists {
				s.cronEng.Remove(eid)
				delete(s.cronEntries, sc.ID)
			}
			continue
		}
		// 已有 entry → 假设 schedule 没变（admin 改 schedule 时由 api layer 触发 Replace
		// → loader.Replace → 这里 reload diff 出 ID，一致就跳过）
		// 简化：每次 reload 都 remove + add（cron 调度成本可忽略）
		if eid, exists := s.cronEntries[sc.ID]; exists {
			s.cronEng.Remove(eid)
		}
		scID := sc.ID
		eid, err := s.cronEng.AddFunc(sc.Schedule, func() {
			s.runOne(context.Background(), scID, "cron")
		})
		if err != nil {
			s.logger.Warn("cron parse failed",
				zap.String("script", sc.ID),
				zap.String("schedule", sc.Schedule),
				zap.Error(err))
			continue
		}
		s.cronEntries[sc.ID] = eid
	}
	// 清掉已删除脚本的 cron entries
	for id, eid := range s.cronEntries {
		if !wantIDs[id] {
			s.cronEng.Remove(eid)
			delete(s.cronEntries, id)
		}
	}
}

// runStreamConsumer 消费 recon:stream:events，按 trigger 路由到对应脚本。
//
// XREADGROUP BLOCK 永久；进程退出由 ctx 取消触发。
func (s *Scheduler) runStreamConsumer(ctx context.Context) {
	const stream = "recon:stream:events"
	const group = "recon-engine"
	const consumer = "scheduler-1"

	// 创建 consumer group（已存在则忽略错误）
	_ = s.r.XGroupCreateMkStream(ctx, stream, group, "$").Err()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		res, err := s.r.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    group,
			Consumer: consumer,
			Streams:  []string{stream, ">"},
			Count:    100,
			Block:    5 * time.Second,
		}).Result()
		if err != nil {
			if err == redis.Nil {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			s.logger.Warn("XReadGroup failed", zap.Error(err))
			time.Sleep(time.Second)
			continue
		}
		for _, st := range res {
			for _, m := range st.Messages {
				svc, _ := m.Values["svc"].(string)
				table, _ := m.Values["table"].(string)
				eventTrigger := svc + ":" + table
				// 找匹配的脚本（triggers 列表里有 svc:table 或 "*"）
				for _, sc := range s.loader.List() {
					for _, tg := range sc.Triggers {
						if tg == eventTrigger || tg == "*" || tg == svc+":*" {
							s.runOne(ctx, sc.ID, "stream:"+eventTrigger)
							break
						}
					}
				}
				// ACK
				_ = s.r.XAck(ctx, stream, group, m.ID).Err()
			}
		}
	}
}

// runOne 执行单条脚本 + 持久化结果（cron / stream 共用）。
func (s *Scheduler) runOne(ctx context.Context, scriptID, trigger string) {
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	sctx := script.NewContext(runCtx, s.searcher, &zapAdapter{z: s.logger}, nil)
	res := s.loader.Run(sctx, scriptID, trigger)
	if err := s.scriptDB.SaveResult(ctx, res); err != nil {
		s.logger.Warn("save result failed",
			zap.String("script", scriptID),
			zap.String("trigger", trigger),
			zap.Error(err))
		return
	}
	// 关键事件落 INFO 日志（diff > 0 升 WARN）
	level := s.logger.Info
	if len(res.Diffs) > 0 {
		level = s.logger.Warn
	}
	level("script run done",
		zap.String("script", scriptID),
		zap.String("trigger", trigger),
		zap.String("status", res.Status),
		zap.Int("diffs", len(res.Diffs)),
		zap.Duration("dur", res.FinishedAt.Sub(res.StartedAt)),
	)
}

// String 返一份调试 dump（admin /api/v1/scheduler/status 用）。
func (s *Scheduler) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	parts := []string{"scheduler:"}
	for id, eid := range s.cronEntries {
		entry := s.cronEng.Entry(eid)
		parts = append(parts, fmt.Sprintf("  %s next=%s", id, entry.Next.Format(time.RFC3339)))
	}
	return strings.Join(parts, "\n")
}

// ─── helpers ─────────────────────────────────────────────────────

type zapAdapter struct{ z *zap.Logger }

func (a *zapAdapter) Info(msg string, kv ...any)  { a.z.Sugar().Infow(msg, kv...) }
func (a *zapAdapter) Warn(msg string, kv ...any)  { a.z.Sugar().Warnw(msg, kv...) }
func (a *zapAdapter) Error(msg string, kv ...any) { a.z.Sugar().Errorw(msg, kv...) }
