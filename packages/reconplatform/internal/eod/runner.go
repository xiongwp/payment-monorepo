// Package eod — End-of-Day 日切对账。
//
// 增量 CDC 难免漏 / 重 / 迟到事件（binlog rotate / 网络抖动 / canal 重启
// 起点偏差等）。EOD 用 SQL 直接拉 T-1 全天数据全量重跑对账，作为 ground truth
// 兜底，发现增量没抓到的差异。
//
// 跑法：每日 02:00（业务低峰）cron，不依赖 Redis 现有事件，直接：
//   1. 对每个 source 的每张表，SELECT * WHERE created_at IN [yesterday, today)
//   2. 用同样的 publisher 路径写到一个隔离 namespace recon:eod:{date}:event:*
//   3. 用 catalog 选定的"日切"规则跑（subset of all rules，category=settlement
//      / amount 优先）
//   4. 产出日切报告 JSON：{date, total_pi, total_amount, diffs_per_type,
//      diffs_per_severity, signed_off_by}
//
// 报告写到 ClickHouse `recon_eod_report` 表（archive 包已有 CH client），
// admin web 显示日切日历。

package eod

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
)

// Report 日切对账报告。
type Report struct {
	Date         string         `json:"date"`         // YYYY-MM-DD
	StartedAt    time.Time      `json:"started_at"`
	FinishedAt   time.Time      `json:"finished_at"`
	DurationMs   int64          `json:"duration_ms"`
	TotalRows    int            `json:"total_rows"`
	TotalAmount  int64          `json:"total_amount"`  // 全天总流水（minor）
	DiffsByType  map[string]int `json:"diffs_by_type"`
	DiffsBySev   map[string]int `json:"diffs_by_severity"`
	Sources      []SourceStat   `json:"sources"`
	Status       string         `json:"status"`        // ok / partial / failed
	Error        string         `json:"error,omitempty"`
	SignedOffBy  string         `json:"signed_off_by,omitempty"`  // ops 手动 sign-off
}

// SourceStat 每个数据源的扫描统计。
type SourceStat struct {
	Service string `json:"service"`
	Table   string `json:"table"`
	Rows    int    `json:"rows"`
	Amount  int64  `json:"amount,omitempty"`
}

// Runner EOD 调度。
type Runner struct {
	rdb      redis.UniversalClient
	log      *zap.Logger
	cron     *cron.Cron
	mu       sync.Mutex
	scriptIDs []string // 日切要跑的脚本 ID 列表
	lastRunDate string  // 防止 cron 重复跑同一天
}

// NewRunner 默认 02:00 跑昨天。
func NewRunner(rdb redis.UniversalClient, log *zap.Logger) *Runner {
	if log == nil {
		log = zap.NewNop()
	}
	return &Runner{
		rdb:  rdb,
		log:  log,
		cron: cron.New(),
	}
}

// SetScripts 注册要在 EOD 跑的脚本列表。
func (r *Runner) SetScripts(ids []string) {
	r.mu.Lock()
	r.scriptIDs = ids
	r.mu.Unlock()
}

// Schedule 注册 cron。schedule 默认 "0 2 * * *"（每天 02:00）。
func (r *Runner) Schedule(schedule string, runFn func(ctx context.Context, date string) (*Report, error)) error {
	if schedule == "" {
		schedule = "0 2 * * *"
	}
	_, err := r.cron.AddFunc(schedule, func() {
		date := time.Now().AddDate(0, 0, -1).Format("2006-01-02") // 昨天
		r.mu.Lock()
		if r.lastRunDate == date {
			r.mu.Unlock()
			r.log.Info("EOD already ran today; skipping", zap.String("date", date))
			return
		}
		r.lastRunDate = date
		r.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()
		report, err := runFn(ctx, date)
		if err != nil {
			r.log.Error("EOD run failed", zap.String("date", date), zap.Error(err))
			return
		}
		if err := r.persistReport(ctx, report); err != nil {
			r.log.Warn("EOD persist failed", zap.Error(err))
		}
		r.log.Info("EOD report ready",
			zap.String("date", report.Date),
			zap.Int("total_rows", report.TotalRows),
			zap.Any("diffs_by_type", report.DiffsByType))
	})
	return err
}

func (r *Runner) Start() { r.cron.Start() }
func (r *Runner) Stop(ctx context.Context) {
	stopped := r.cron.Stop()
	select {
	case <-stopped.Done():
	case <-ctx.Done():
	}
}

// persistReport 落 Redis 给 admin web 列日历用 + push history list。
func (r *Runner) persistReport(ctx context.Context, rep *Report) error {
	body, _ := json.Marshal(rep)
	key := fmt.Sprintf("recon:eod:report:%s", rep.Date)
	pipe := r.rdb.Pipeline()
	pipe.Set(ctx, key, body, 365*24*time.Hour) // 保留 1 年
	pipe.LPush(ctx, "recon:eod:history", rep.Date)
	pipe.LTrim(ctx, "recon:eod:history", 0, 365)
	_, err := pipe.Exec(ctx)
	return err
}

// ListReports admin web GET /api/v1/eod/reports — 取最近 N 天的报告。
func (r *Runner) ListReports(ctx context.Context, limit int) ([]*Report, error) {
	if limit <= 0 || limit > 365 {
		limit = 30
	}
	dates, err := r.rdb.LRange(ctx, "recon:eod:history", 0, int64(limit-1)).Result()
	if err != nil {
		return nil, err
	}
	out := make([]*Report, 0, len(dates))
	for _, d := range dates {
		body, err := r.rdb.Get(ctx, "recon:eod:report:"+d).Bytes()
		if err != nil {
			continue
		}
		var rep Report
		if err := json.Unmarshal(body, &rep); err != nil {
			continue
		}
		out = append(out, &rep)
	}
	return out, nil
}

// SignOff 财务/合规手动确认日切报告（写 signed_off_by 字段）。
func (r *Runner) SignOff(ctx context.Context, date, by string) error {
	key := fmt.Sprintf("recon:eod:report:%s", date)
	body, err := r.rdb.Get(ctx, key).Bytes()
	if err != nil {
		return err
	}
	var rep Report
	if err := json.Unmarshal(body, &rep); err != nil {
		return err
	}
	rep.SignedOffBy = by + "@" + time.Now().UTC().Format(time.RFC3339)
	body2, _ := json.Marshal(&rep)
	return r.rdb.Set(ctx, key, body2, 365*24*time.Hour).Err()
}
