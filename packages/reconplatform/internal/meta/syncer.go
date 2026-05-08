// Package meta 把 13 个业务服务 MySQL 的 information_schema.columns 定时
// 同步到 reconplatform Redis，给 cdc parser + 脚本编辑器 autocomplete 用。
//
// Redis 布局：
//
//	recon:meta:tables                  → SET of "<svc>:<table>"
//	recon:meta:schema:<svc>:<table>    → JSON [{name, type, nullable, key, comment, ord}]
//	recon:meta:idx_keys                → SET of available index column names
//	                                       （所有 sources 配的 index_columns 并集，
//	                                        给编辑器 autocomplete 提示用）
//	recon:meta:last_sync:<svc>         → unix ms （上次同步时间，监控 lag）
package meta

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// Column 一列的元信息（持久化到 Redis）。
type Column struct {
	Name      string `json:"name"`
	Type      string `json:"type"`            // "varchar(64)" / "bigint" / ...
	Nullable  bool   `json:"nullable"`
	Key       string `json:"key,omitempty"`   // "PRI" / "MUL" / "UNI" / ""
	Comment   string `json:"comment,omitempty"`
	Ord       int    `json:"ord"`             // 在表里的序号（1-based）
}

// Source 一个待同步的 MySQL 实例。
//
// 跟 cdc.Source 不一样：那是 binlog 订阅；这里只是 SELECT
// information_schema 拉 schema，连普通 ROW DML 权限都不需要。
//
// 大部分 service 的 cdc.Source 和 meta.Source 共用同一组 endpoint，
// 但权限可以分开（meta 用一个 SELECT 只读账号即可）。
type Source struct {
	Service  string
	DSN      string   // root:pass@tcp(host:3306)/?parseTime=true
	Schemas  []string // 要扫的 schema 名字（shard 库列表）；空则扫所有非系统库
}

// Syncer 拉 information_schema 的本体。一份 Syncer 串行扫所有 sources；
// 周期由 Run 接口注入（默认 5min；schema 变更不频繁）。
type Syncer struct {
	r       redis.UniversalClient
	logger  *zap.Logger

	mu      sync.Mutex
	sources []Source
	idxKeys []string // 所有 sources 的 index_columns 并集，给编辑器用
}

func NewSyncer(r redis.UniversalClient, logger *zap.Logger) *Syncer {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Syncer{r: r, logger: logger}
}

// Reload 替换待同步的 sources。OnChange 时调用。
// idxKeys 是所有 cdc.Source.IndexColumns 的并集，给编辑器 autocomplete 用。
func (s *Syncer) Reload(sources []Source, idxKeys []string) {
	s.mu.Lock()
	s.sources = sources
	s.idxKeys = idxKeys
	s.mu.Unlock()
}

// Run 阻塞循环：每个 interval 扫一遍所有 sources。
func (s *Syncer) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	// 启动期立即跑一次
	s.SyncOnce(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			s.SyncOnce(ctx)
		}
	}
}

// SyncOnce 跑一次全量同步。每个 service 失败不中断其他 service。
func (s *Syncer) SyncOnce(ctx context.Context) {
	s.mu.Lock()
	srcs := append([]Source(nil), s.sources...)
	idxKeys := append([]string(nil), s.idxKeys...)
	s.mu.Unlock()

	allTables := make([]string, 0, 256)
	for _, src := range srcs {
		tables, err := s.syncOne(ctx, src)
		if err != nil {
			s.logger.Warn("meta sync failed",
				zap.String("svc", src.Service), zap.Error(err))
			continue
		}
		allTables = append(allTables, tables...)
		// 记录上次成功同步时间
		_ = s.r.Set(ctx,
			"recon:meta:last_sync:"+src.Service,
			time.Now().UnixMilli(),
			0).Err()
	}

	// 全量替换 tables / idx_keys 集合（用 DEL + SADD 而不是 SADD 累加，
	// 避免删了的表残留在集合里）
	if len(allTables) > 0 {
		pipe := s.r.Pipeline()
		pipe.Del(ctx, "recon:meta:tables")
		pipe.SAdd(ctx, "recon:meta:tables", interfaceSlice(allTables)...)
		_, _ = pipe.Exec(ctx)
	}
	if len(idxKeys) > 0 {
		pipe := s.r.Pipeline()
		pipe.Del(ctx, "recon:meta:idx_keys")
		pipe.SAdd(ctx, "recon:meta:idx_keys", interfaceSlice(idxKeys)...)
		_, _ = pipe.Exec(ctx)
	}
}

// syncOne 扫一个 service 的所有 schema → 返回 "<svc>:<table>" 列表。
func (s *Syncer) syncOne(ctx context.Context, src Source) ([]string, error) {
	db, err := sql.Open("mysql", src.DSN)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	defer db.Close()
	db.SetConnMaxLifetime(30 * time.Second)
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)

	// 查 columns
	q := `
SELECT TABLE_SCHEMA, TABLE_NAME, COLUMN_NAME, COLUMN_TYPE,
       IS_NULLABLE, COLUMN_KEY, COLUMN_COMMENT, ORDINAL_POSITION
FROM information_schema.columns
WHERE TABLE_SCHEMA NOT IN ('information_schema','mysql','performance_schema','sys')`
	args := []any{}
	if len(src.Schemas) > 0 {
		// 限定 schema 范围
		q += ` AND TABLE_SCHEMA IN (` + placeholders(len(src.Schemas)) + `)`
		for _, sch := range src.Schemas {
			args = append(args, sch)
		}
	}
	q += ` ORDER BY TABLE_SCHEMA, TABLE_NAME, ORDINAL_POSITION`

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	// 同一 (schema, table) 的列攒到 cols；遇到 break 就 flush 一条 schema JSON。
	type bucket struct {
		schema, table string
		cols          []Column
	}
	var cur *bucket
	flushed := make([]string, 0, 64)
	pipe := s.r.Pipeline()
	flush := func() {
		if cur == nil || len(cur.cols) == 0 {
			return
		}
		jsonBytes, _ := json.Marshal(cur.cols)
		key := fmt.Sprintf("recon:meta:schema:%s:%s", src.Service, cur.table)
		pipe.Set(ctx, key, jsonBytes, 0) // 永久（被新一轮 SyncOnce 覆盖）
		flushed = append(flushed, src.Service+":"+cur.table)
	}

	for rows.Next() {
		var schema, table, name, ctype, nullable, key, comment string
		var ord int
		if err := rows.Scan(&schema, &table, &name, &ctype, &nullable, &key, &comment, &ord); err != nil {
			return flushed, fmt.Errorf("scan: %w", err)
		}
		if cur == nil || cur.schema != schema || cur.table != table {
			flush()
			cur = &bucket{schema: schema, table: table}
		}
		cur.cols = append(cur.cols, Column{
			Name:     name,
			Type:     ctype,
			Nullable: nullable == "YES",
			Key:      key,
			Comment:  comment,
			Ord:      ord,
		})
	}
	flush()
	if err := rows.Err(); err != nil {
		return flushed, fmt.Errorf("rows: %w", err)
	}

	if _, err := pipe.Exec(ctx); err != nil {
		return flushed, fmt.Errorf("redis pipeline: %w", err)
	}
	return flushed, nil
}

// Provider 给 cdc parser 用：拿 (svc, schema, table) 的列定义。
//
// 直接走 Redis 缓存（每个 schema sync 周期更新一次），避免 binlog 高频
// 走 information_schema 拖慢业务库。
type Provider struct {
	r redis.UniversalClient
}

func NewProvider(r redis.UniversalClient) *Provider { return &Provider{r: r} }

// Columns 实现 cdc.SchemaProvider 接口（注意：cdc 包内部定义的 columnInfo
// 是私有类型，这里返回的是它的等价 view）。
func (p *Provider) GetColumns(ctx context.Context, service, table string) ([]Column, error) {
	key := fmt.Sprintf("recon:meta:schema:%s:%s", service, table)
	raw, err := p.r.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return nil, fmt.Errorf("schema not synced: %s/%s", service, table)
	}
	if err != nil {
		return nil, err
	}
	var cols []Column
	if err := json.Unmarshal(raw, &cols); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return cols, nil
}

// PKColumns 探测主键列（自动推断，给 cdc.TableConfig.PK 留空时用）。
func (p *Provider) PKColumns(ctx context.Context, service, table string) ([]string, error) {
	cols, err := p.GetColumns(ctx, service, table)
	if err != nil {
		return nil, err
	}
	var pks []string
	for _, c := range cols {
		if c.Key == "PRI" {
			pks = append(pks, c.Name)
		}
	}
	if len(pks) == 0 {
		return nil, fmt.Errorf("no PRI key in %s/%s", service, table)
	}
	return pks, nil
}

// ─── helpers ──────────────────────────────────────────────────────

func interfaceSlice(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	out := make([]byte, 0, 2*n-1)
	for i := 0; i < n; i++ {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, '?')
	}
	return string(out)
}
