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

	// **分库分表合并**：order_core_db_0.payment_intents_00 / db_1.payment_intents_00
	// / ... 共 10×N 张物理表合并成一个逻辑 (svc, base_table)。
	//
	// 合并策略：
	//   schema 名取首个看到的（dbg 用）；列定义只存第一次（同一逻辑表所有 shard
	//   schema 相同，否则上线流程肯定挂了；不一致时后写覆盖更新，记 shard 增量）
	//   shard_count 累加：admin web 显示 'order-core:payment_intents (10 shards)'
	type logicalKey struct {
		service   string
		baseTable string
	}
	type logical struct {
		key        logicalKey
		cols       []Column
		shardSet   map[string]struct{} // physical_table → 见过的 shard 数
	}
	logicals := make(map[logicalKey]*logical)
	flushed := make([]string, 0, 64)
	pipe := s.r.Pipeline()

	for rows.Next() {
		var schema, table, name, ctype, nullable, key, comment string
		var ord int
		if err := rows.Scan(&schema, &table, &name, &ctype, &nullable, &key, &comment, &ord); err != nil {
			return flushed, fmt.Errorf("scan: %w", err)
		}
		base := baseTableName(table)
		k := logicalKey{service: src.Service, baseTable: base}
		lg, ok := logicals[k]
		if !ok {
			lg = &logical{key: k, shardSet: map[string]struct{}{}}
			logicals[k] = lg
		}
		lg.shardSet[schema+"."+table] = struct{}{}
		// 列定义只在首次出现的 (schema, table) 上累计；其他 shard 跳过
		// （ord 从 1 重置 = 新物理表）
		if ord == 1 && len(lg.cols) > 0 {
			continue // 已经从首张 shard 拿到列定义，后面 shard 不再追加
		}
		lg.cols = append(lg.cols, Column{
			Name:     name,
			Type:     ctype,
			Nullable: nullable == "YES",
			Key:      key,
			Comment:  comment,
			Ord:      ord,
		})
	}
	for k, lg := range logicals {
		if len(lg.cols) == 0 {
			continue
		}
		jsonBytes, _ := json.Marshal(lg.cols)
		// 主 schema 写在 base_table 名下（去 shard 后缀）
		redisKey := fmt.Sprintf("recon:meta:schema:%s:%s", k.service, k.baseTable)
		pipe.Set(ctx, redisKey, jsonBytes, 0)
		// shard_count 元信息：admin web 用来显示 '(N shards)'
		pipe.Set(ctx,
			fmt.Sprintf("recon:meta:shards:%s:%s", k.service, k.baseTable),
			fmt.Sprintf("%d", len(lg.shardSet)), 0)
		flushed = append(flushed, src.Service+":"+k.baseTable)
	}
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

// baseTableName 把分库分表的物理表名归一化为逻辑表名。
//
// 各服务约定的命名规则：
//
//	V1 (现役):  base_table_NN          后缀 2 位数字（00-99）→ 100 shard
//	V2 (新):    base_table_NN_NNN      后缀 5 位 (db_NN, tbl_NNN)  → 1000 shard
//	shadow:    base_table_NN_shadow   或 base_table_NN_NNN_shadow → 影子流量
//
// 实现：从右往左剥后缀
//   1. 末尾 _shadow → 剥
//   2. 末尾 _NNN_NN 或 _NN → 剥
//
// 例：
//	payment_intents_03                → payment_intents
//	payment_intents_03_shadow         → payment_intents
//	voucher_07_813                    → voucher
//	voucher_07_813_shadow             → voucher
//	leaf_alloc                        → leaf_alloc (无后缀，原样返)
func baseTableName(table string) string {
	t := table
	// 1) 剥 _shadow 后缀
	const shadowSuf = "_shadow"
	if len(t) > len(shadowSuf) && t[len(t)-len(shadowSuf):] == shadowSuf {
		t = t[:len(t)-len(shadowSuf)]
	}
	// 2) 循环剥末尾的 _<digits> 段；最多剥 2 段（V2 layout 是 _NN_NNN）
	for i := 0; i < 2; i++ {
		idx := -1
		for j := len(t) - 1; j >= 0; j-- {
			c := t[j]
			if c >= '0' && c <= '9' {
				continue
			}
			if c == '_' {
				idx = j
			}
			break
		}
		if idx <= 0 {
			break
		}
		// 必须 _ 后全是数字（至少 1 位），否则不是 shard 后缀
		allDigits := true
		for j := idx + 1; j < len(t); j++ {
			c := t[j]
			if c < '0' || c > '9' {
				allDigits = false
				break
			}
		}
		if !allDigits || idx+1 == len(t) {
			break
		}
		t = t[:idx]
	}
	return t
}
