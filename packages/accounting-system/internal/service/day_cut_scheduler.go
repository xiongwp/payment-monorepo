// Package service: DayCutScheduler — 自动日切触发器。
//
// 设计：根据 system_config.day_cut.scheduled_time 在每天对应时刻触发一次日切。
//   - 30s 一 tick；当且仅当 (a) NOW >= today's scheduled_time AND (b) 今天还没触发过 → 触发
//   - 一次触发 = 对每个配置的币种调一次 TriggerDayCut(cut_date, currency)
//     cut_date = computeCutDate(now, scheduled_time, tz) — 通常是"today"，
//     但如果手动调度时间在 boundary 之前，cut_date 是"yesterday"，与 booking 端一致
//   - 手动 admin 触发 (gRPC TriggerDayCut / admin HTTP) 不受影响，可任意时间触发
//
// system_config keys：
//   day_cut.enabled         "true" / "false"        默认 false（cron 关；admin 仍可手动）
//   day_cut.scheduled_time  "HH:MM:SS"              默认 "00:00:00"
//   day_cut.timezone        IANA tz name            默认 server local
//   day_cut.currencies      "PHP,USD,..." 逗号分隔  默认 "PHP"
package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// dayCutSchedulerTick 调度器 poll 间隔。
// 30s 让"漏窗时长"上限 = 30s（首次跨过 scheduled_time 后最迟 30s 内触发）。
const dayCutSchedulerTick = 30 * time.Second

// DayCutScheduler 自动日切调度器。
type DayCutScheduler struct {
	cfgSvc  SystemConfigService
	dayCut  DayCutService
	cutDate CutDateProvider
	logger  *zap.Logger

	// triggered 记录每对 (cut_date, currency) 是否已触发过本日切。
	// 避免同一窗口反复触发；进程重启会清空 → 重启后会再触发一次（DayCutService 的
	// run_id 自增机制让重复触发安全：当天该 cutDate+currency 直接生成 run_id+1 重跑）。
	triggered sync.Map // key = "cutDate|currency", value = struct{}
}

// NewDayCutScheduler 构造。fx 自动注入；nil-safe。
func NewDayCutScheduler(
	cfgSvc SystemConfigService,
	dayCut DayCutService,
	cutDate CutDateProvider,
	logger *zap.Logger,
) *DayCutScheduler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &DayCutScheduler{
		cfgSvc:  cfgSvc,
		dayCut:  dayCut,
		cutDate: cutDate,
		logger:  logger,
	}
}

// Run 阻塞循环；ctx.Done() 时退出。
func (s *DayCutScheduler) Run(ctx context.Context) {
	if s.dayCut == nil || s.cfgSvc == nil {
		s.logger.Warn("day cut scheduler disabled (missing dayCut or config service)")
		return
	}
	s.logger.Info("day cut scheduler started", zap.Duration("tick", dayCutSchedulerTick))
	ticker := time.NewTicker(dayCutSchedulerTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.logger.Info("day cut scheduler stopped")
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

func (s *DayCutScheduler) tick(ctx context.Context) {
	if !s.cfgSvc.GetBool(ConfigKeyDayCutEnabled, false) {
		return
	}
	timeStr := s.cfgSvc.GetString(ConfigKeyDayCutScheduledTime, defaultDayCutScheduledTime)
	tzStr := s.cfgSvc.GetString(ConfigKeyDayCutTimezone, "")
	currenciesStr := s.cfgSvc.GetString(ConfigKeyDayCutCurrencies, defaultDayCutCurrencies)

	h, m, sec, ok := parseHHMMSS(timeStr)
	if !ok {
		// 默认 00:00:00 — 与 booking 端 CutDateProvider 的 fallback 行为一致
		h, m, sec = 0, 0, 0
	}
	tz := time.Local
	if tzStr != "" {
		if loc, err := time.LoadLocation(tzStr); err == nil {
			tz = loc
		}
	}

	now := time.Now().In(tz)
	cutToday := time.Date(now.Year(), now.Month(), now.Day(), h, m, sec, 0, tz)
	if now.Before(cutToday) {
		// 今天的 scheduled_time 还没到 → 等下一个 tick
		return
	}

	// 触发的 cut_date：与 booking 端语义一致 — 当前时刻 >= cutToday，所以 cut_date == today
	// （ComputeCutDate 在 boundary 之时/之后归 today）
	cutDate := now.Format("2006-01-02")

	for _, cur := range strings.Split(currenciesStr, ",") {
		cur = strings.TrimSpace(cur)
		if cur == "" {
			continue
		}
		key := fmt.Sprintf("%s|%s", cutDate, cur)
		if _, already := s.triggered.LoadOrStore(key, struct{}{}); already {
			continue
		}
		s.logger.Info("day cut scheduler firing",
			zap.String("cut_date", cutDate),
			zap.String("currency", cur),
			zap.String("scheduled_time", timeStr),
			zap.String("tz", tz.String()))
		// TriggerDayCut 内部会自增 run_id；多次触发安全（增量 upsert + 续跑游标）
		if err := s.dayCut.TriggerDayCut(ctx, cutDate, cur); err != nil {
			s.logger.Error("day cut scheduler trigger failed",
				zap.String("cut_date", cutDate),
				zap.String("currency", cur),
				zap.Error(err))
		}
	}
}

// SeedDayCutDefaults 在启动时（一次性）确保 day_cut.* 的 4 个 key 在 system_config
// 表中存在。Upsert 之前先 GetString 探测：已存在的不覆盖（管理员可能已经在 admin
// 页面改过值）。这样新部署 / 老部署都能在 /system-config 页面立即看到这些 key。
func SeedDayCutDefaults(ctx context.Context, svc SystemConfigService, logger *zap.Logger) {
	if svc == nil {
		return
	}
	type seed struct {
		key, valueJSON, valueType, description string
	}
	seeds := []seed{
		{ConfigKeyDayCutEnabled, `"false"`, "string",
			"日切自动调度开关。true=cron 触发；false=仅 admin/gRPC 手动触发。任何时候手动触发都生效"},
		{ConfigKeyDayCutScheduledTime, `"` + defaultDayCutScheduledTime + `"`, "string",
			"日切时间（HH:MM:SS，本地或指定时区）。当日早于此时间的请求归属昨天业务日，等于/晚于的归今天"},
		{ConfigKeyDayCutTimezone, `"` + defaultDayCutTimezone + `"`, "string",
			"日切时区（IANA 名，如 Asia/Manila；空=服务器 Local）"},
		{ConfigKeyDayCutCurrencies, `"` + defaultDayCutCurrencies + `"`, "string",
			"日切币种列表（逗号分隔，如 PHP,USD,CNY）。每个币种独立 run"},
	}
	for _, s := range seeds {
		// 探测：已存在则跳过，避免覆盖管理员已改过的值
		if existing := svc.GetString(s.key, ""); existing != "" {
			continue
		}
		if err := svc.Upsert(ctx, s.key, s.valueJSON, s.valueType, s.description, "system-bootstrap"); err != nil {
			logger.Warn("seed day_cut config failed (non-fatal)",
				zap.String("key", s.key), zap.Error(err))
			continue
		}
		logger.Info("seeded day_cut config", zap.String("key", s.key))
	}
}
