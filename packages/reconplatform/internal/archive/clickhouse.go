// Package archive — diff 冷热分层归档。
//
// 问题：现有 diffstate 把所有 diff 落 Redis，TTL 90 天。可:
//   - 90d 之后数据丢 → 财务对账要查"去年 11 月 11 日的差异" 没源
//   - Redis 内存爆：单 diff JSON ~500B × 30 万/月 = 150MB/月，6 个月 = 1GB+
//   - 跨年趋势分析（"双 11 vs 春节差异分布"）Redis 不擅长
//
// 解决：冷热分层
//   - HOT (Redis):  最近 7 天  — 个位 ms 查询，admin "我的待办" 用
//   - COLD (ClickHouse): >7d 永久归档 — 列存压缩比 10:1 + SQL 聚合
//
// 数据流:
//
//	publisher → Redis (热) ─────┐
//	                            ▼
//	   Worker(cron 1h) ── 每小时 SCAN 过期 diff (>7d) ──→ batch INSERT ClickHouse
//	                            │
//	                            └─→ 写完后 Redis 不删除（保留 90d TTL 自然老化），
//	                                 ClickHouse 是 source of truth (PRIMARY KEY = id)
//	                                 同一 diff 进 ClickHouse 多次靠 ReplacingMergeTree 去重
//
// 查询路由 query.go: Unified.Query(time_range) → 自动路由 Redis/CH，>7d 跨度走 CH。
//
// 表结构（schema.sql 同目录）：
//
//	CREATE TABLE recon_diffs_archive (
//	    id String,
//	    script_id LowCardinality(String),
//	    run_id String,
//	    type LowCardinality(String),
//	    key String,
//	    detail_json String,
//	    state LowCardinality(String),
//	    created_at DateTime64(3),
//	    updated_at DateTime64(3),
//	    updated_by LowCardinality(String),
//	    note String,
//	    archived_at DateTime DEFAULT now()
//	) ENGINE = ReplacingMergeTree(updated_at)
//	  PARTITION BY toYYYYMM(created_at)
//	  ORDER BY (created_at, id)
//	  TTL created_at + INTERVAL 5 YEAR;
//
// 不引 clickhouse-go：用 HTTP API（curl-friendly），避免大依赖。

package archive

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"reconcile-system/internal/diffstate"
	"reconcile-system/internal/metrics"
)

// Config ClickHouse HTTP 接入参数。
type Config struct {
	URL      string // http://clickhouse:8123  (无 trailing /)
	Database string // recon
	User     string
	Password string
	Table    string // recon_diffs_archive

	// Worker 控制
	HotWindow     time.Duration // 默认 7 * 24h
	BatchSize     int           // 默认 500
	WorkerEvery   time.Duration // 默认 1h
	MaxAgeRetain  time.Duration // 默认 90d — 写入 CH 后 Redis 仍保留至 TTL
}

// DefaultConfig 给 wire 用，env 缺省。
func DefaultConfig() Config {
	return Config{
		URL:          "http://clickhouse:8123",
		Database:     "recon",
		Table:        "recon_diffs_archive",
		HotWindow:    7 * 24 * time.Hour,
		BatchSize:    500,
		WorkerEvery:  time.Hour,
		MaxAgeRetain: 90 * 24 * time.Hour,
	}
}

// Archiver 归档器：把 Redis 里冷数据搬 ClickHouse + 提供 CH 查询。
type Archiver struct {
	cfg   Config
	rdb   redis.UniversalClient
	store *diffstate.Store
	log   *zap.Logger
	hc    *http.Client
}

// New 构造。store 用来取 diff 详情，rdb 用来 SCAN。
func New(cfg Config, rdb redis.UniversalClient, store *diffstate.Store, log *zap.Logger) *Archiver {
	if log == nil {
		log = zap.NewNop()
	}
	if cfg.HotWindow == 0 {
		cfg.HotWindow = 7 * 24 * time.Hour
	}
	if cfg.BatchSize == 0 {
		cfg.BatchSize = 500
	}
	if cfg.WorkerEvery == 0 {
		cfg.WorkerEvery = time.Hour
	}
	if cfg.Table == "" {
		cfg.Table = "recon_diffs_archive"
	}
	if cfg.Database == "" {
		cfg.Database = "recon"
	}
	return &Archiver{
		cfg:   cfg,
		rdb:   rdb,
		store: store,
		log:   log,
		hc:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Run 启动后台 worker：周期把 (now - HotWindow) 之前的 diff 灌 CH。
//
// 跑法：
//
//	go a.Run(ctx)
//
// 退出：ctx 取消。
func (a *Archiver) Run(ctx context.Context) {
	a.log.Info("archive worker starting",
		zap.Duration("interval", a.cfg.WorkerEvery),
		zap.Duration("hot_window", a.cfg.HotWindow),
		zap.String("ch_url", a.cfg.URL),
	)
	// 启动时跑一次（追上停机期间未归档的）
	a.archiveOnce(ctx)
	t := time.NewTicker(a.cfg.WorkerEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			a.log.Info("archive worker stopped")
			return
		case <-t.C:
			a.archiveOnce(ctx)
		}
	}
}

// archiveOnce 单次归档。
//
// 策略：扫所有 by_state ZSET 里 score < cutoff 的 diff_id（注：score=updated_ms），
// 拿详情，batch INSERT ClickHouse。失败的 diff 下次还会被扫到（幂等）。
func (a *Archiver) archiveOnce(ctx context.Context) {
	cutoff := time.Now().Add(-a.cfg.HotWindow).UnixMilli()
	states := []diffstate.State{
		diffstate.StateOpen,
		diffstate.StateAcked,
		diffstate.StateResolved,
		diffstate.StateFalsePositive,
		diffstate.StateExpired,
	}
	totalArch := 0
	for _, st := range states {
		ids, err := a.rdb.ZRangeByScore(ctx, "recon:diff:by_state:"+string(st),
			&redis.ZRangeBy{
				Min:   "-inf",
				Max:   strconv.FormatInt(cutoff, 10),
				Count: int64(a.cfg.BatchSize),
			}).Result()
		if err != nil {
			a.log.Warn("archive scan failed", zap.String("state", string(st)), zap.Error(err))
			continue
		}
		if len(ids) == 0 {
			continue
		}
		batch := make([]*diffstate.Diff, 0, len(ids))
		for _, id := range ids {
			d, err := a.store.Get(ctx, id)
			if err != nil || d == nil {
				continue
			}
			batch = append(batch, d)
		}
		if len(batch) == 0 {
			continue
		}
		t0 := time.Now()
		if err := a.insertBatch(ctx, batch); err != nil {
			metrics.ArchiveBatchTotal.WithLabelValues(string(st), "err").Inc()
			a.log.Warn("archive insert failed",
				zap.String("state", string(st)),
				zap.Int("batch_size", len(batch)),
				zap.Error(err))
			continue
		}
		metrics.ArchiveBatchTotal.WithLabelValues(string(st), "ok").Inc()
		metrics.ArchiveBatchSize.Observe(float64(len(batch)))
		metrics.ArchiveInsertSeconds.Observe(time.Since(t0).Seconds())
		a.log.Info("archive batch ok",
			zap.String("state", string(st)),
			zap.Int("count", len(batch)),
		)
		totalArch += len(batch)
	}
	if totalArch > 0 {
		a.log.Info("archive cycle complete", zap.Int("total", totalArch))
	}
}

// insertBatch 批量 INSERT ClickHouse via HTTP /?query=INSERT...FORMAT JSONEachRow
//
// JSONEachRow 是 CH 推荐的批量入库格式：一行一个 JSON 对象，body 直接 raw bytes。
func (a *Archiver) insertBatch(ctx context.Context, diffs []*diffstate.Diff) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, d := range diffs {
		dj, _ := json.Marshal(d.Detail)
		row := map[string]any{
			"id":         d.ID,
			"script_id":  d.ScriptID,
			"run_id":     d.RunID,
			"type":       d.Type,
			"key":        d.Key,
			"detail_json": string(dj),
			"state":      string(d.State),
			"created_at": d.CreatedAt.UTC().Format("2006-01-02 15:04:05.000"),
			"updated_at": d.UpdatedAt.UTC().Format("2006-01-02 15:04:05.000"),
			"updated_by": d.UpdatedBy,
			"note":       d.Note,
		}
		if err := enc.Encode(row); err != nil {
			return err
		}
	}
	q := fmt.Sprintf("INSERT INTO %s.%s FORMAT JSONEachRow",
		a.cfg.Database, a.cfg.Table)
	return a.execInsert(ctx, q, buf.Bytes())
}

// execInsert POST body to CH HTTP endpoint.
func (a *Archiver) execInsert(ctx context.Context, query string, body []byte) error {
	u, _ := url.Parse(a.cfg.URL)
	q := u.Query()
	q.Set("query", query)
	if a.cfg.Database != "" {
		q.Set("database", a.cfg.Database)
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, "POST", u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	if a.cfg.User != "" {
		req.SetBasicAuth(a.cfg.User, a.cfg.Password)
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := a.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("clickhouse INSERT %d: %s",
			resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

// EnsureSchema 启动时调一次：创建 database + table（IF NOT EXISTS）。
//
// schema.sql 同目录有完整 DDL；这里嵌入避免文件读取。
func (a *Archiver) EnsureSchema(ctx context.Context) error {
	stmts := []string{
		fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s", a.cfg.Database),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.%s (
    id String,
    script_id LowCardinality(String),
    run_id String,
    type LowCardinality(String),
    key String,
    detail_json String,
    state LowCardinality(String),
    created_at DateTime64(3),
    updated_at DateTime64(3),
    updated_by LowCardinality(String),
    note String,
    archived_at DateTime DEFAULT now()
) ENGINE = ReplacingMergeTree(updated_at)
  PARTITION BY toYYYYMM(created_at)
  ORDER BY (created_at, id)
  TTL toDateTime(created_at) + INTERVAL 5 YEAR
  SETTINGS index_granularity = 8192`, a.cfg.Database, a.cfg.Table),
	}
	for _, s := range stmts {
		if err := a.execDDL(ctx, s); err != nil {
			return fmt.Errorf("DDL %q failed: %w", firstLine(s), err)
		}
	}
	a.log.Info("clickhouse schema ensured",
		zap.String("db", a.cfg.Database),
		zap.String("table", a.cfg.Table),
	)
	return nil
}

func (a *Archiver) execDDL(ctx context.Context, sql string) error {
	u, _ := url.Parse(a.cfg.URL)
	req, err := http.NewRequestWithContext(ctx, "POST", u.String(), strings.NewReader(sql))
	if err != nil {
		return err
	}
	if a.cfg.User != "" {
		req.SetBasicAuth(a.cfg.User, a.cfg.Password)
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i > 0 {
		return s[:i] + "..."
	}
	return s
}
