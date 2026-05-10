// Package backfill 把业务库历史数据灌进 reconplatform Redis。
//
// 使用场景：
//
//   1. CDC 刚开始订 binlog → 只能追实时，历史数据搜不到
//   2. 索引列改了（加了新 idx_col）→ 老事件没该 idx，需要回灌补上
//   3. shard reshard 后 → 新表里事件需要重新走索引
//
// 工作方式（与 CDC binlog stream 互补）：
//
//   SELECT * FROM <table> WHERE <pk_col> > <last_pk> ORDER BY <pk_col> LIMIT N
//      ↓ 转成 cdc.Event (op=INSERT, ts=row.created_at)
//   publisher.PublishBatch (复用所有 enricher / RowFilter / TTL / 索引逻辑)
//
// 与 reshard migrator (payment-util/reshard) 的区别：
//   reshard      = 业务库 V1 → V2 数据迁移（写另一个业务库）
//   backfill     = 业务库 → reconplatform Redis 索引（不动业务库）

package backfill

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"go.uber.org/zap"

	"reconcile-system/internal/cdc"
)

// Spec 一次 backfill 任务的规格。
type Spec struct {
	Service     string            // 业务服务名（svc 字段）
	DSN         string            // mysql DSN（带库名 / 用户 / 密码）
	Table       string            // 目标表（不带 shard 后缀；caller 决定要不要 _NN）
	PKCol       string            // 主键列名（递增分页）
	IndexCols   []string          // 索引列名列表（写 recon:idx:<col>:<value>）
	IgnoreCols  []string          // 不入 Redis 的列（PII / 长 blob）
	BatchSize   int               // 每批拉多少行（默认 500）
	StartPK     int64             // 从这个 PK 开始（恢复中断的任务）
	StopPK      int64             // 拉到这个 PK 停（0 = 一直拉到最后）
	Where       string            // 额外 WHERE 子句（不加 WHERE 关键字）
	WhereArgs   []any
	TTL         time.Duration     // 写 Redis 的 TTL；0 = 用 publisher 内部 TTLProvider
}

// Result 任务统计。
type Result struct {
	Spec       *Spec     `json:"spec"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Rows       int64     `json:"rows"`
	Batches    int       `json:"batches"`
	LastPK     int64     `json:"last_pk"`
	Error      string    `json:"error,omitempty"`
}

// Run 执行 backfill 任务。
//
// 调用者通常给 1 张表跑 1 次 Run；多张表 / 多 shard 起多 goroutine 并发。
//
// 中断恢复：result.LastPK 持久化即可，下次传给 spec.StartPK 续跑。
func Run(ctx context.Context, sp *Spec, pub *cdc.Publisher, log *zap.Logger) *Result {
	if log == nil {
		log = zap.NewNop()
	}
	if sp.BatchSize <= 0 {
		sp.BatchSize = 500
	}
	res := &Result{Spec: sp, StartedAt: time.Now(), LastPK: sp.StartPK}

	db, err := sql.Open("mysql", sp.DSN)
	if err != nil {
		res.Error = fmt.Sprintf("open db: %v", err)
		res.FinishedAt = time.Now()
		return res
	}
	defer db.Close()
	db.SetConnMaxLifetime(60 * time.Second)
	db.SetMaxOpenConns(2)

	// 看 column 列表（拼通用 SELECT *）
	cols, err := tableColumns(ctx, db, sp.Table)
	if err != nil {
		res.Error = fmt.Sprintf("schema: %v", err)
		res.FinishedAt = time.Now()
		return res
	}
	colExpr := buildColExpr(cols)
	pkCol := sp.PKCol
	if pkCol == "" {
		pkCol = "id"
	}

	for {
		if err := ctx.Err(); err != nil {
			res.Error = err.Error()
			break
		}
		batch, maxPK, err := fetchBatch(ctx, db, sp, colExpr, pkCol, res.LastPK)
		if err != nil {
			res.Error = err.Error()
			log.Warn("backfill: fetch failed",
				zap.String("svc", sp.Service), zap.String("table", sp.Table),
				zap.Int64("last_pk", res.LastPK), zap.Error(err))
			break
		}
		if len(batch) == 0 {
			log.Info("backfill: done",
				zap.String("svc", sp.Service), zap.String("table", sp.Table),
				zap.Int64("rows", res.Rows), zap.Int("batches", res.Batches))
			break
		}
		// 转 cdc.Event 列表
		events := make([]*cdc.Event, 0, len(batch))
		for _, row := range batch {
			ev := rowToEvent(sp, cols, row, pkCol)
			if ev == nil {
				continue
			}
			events = append(events, ev)
		}
		if err := pub.PublishBatch(ctx, events); err != nil {
			res.Error = "publish: " + err.Error()
			log.Warn("backfill: publish failed",
				zap.String("svc", sp.Service), zap.String("table", sp.Table),
				zap.Int("batch_size", len(batch)), zap.Error(err))
			break
		}
		res.Rows += int64(len(batch))
		res.Batches++
		res.LastPK = maxPK

		// 进度日志（每 10 batch 一次）
		if res.Batches%10 == 0 {
			log.Info("backfill: progress",
				zap.String("svc", sp.Service), zap.String("table", sp.Table),
				zap.Int64("rows", res.Rows), zap.Int64("last_pk", res.LastPK))
		}

		if sp.StopPK > 0 && maxPK >= sp.StopPK {
			break
		}
		if len(batch) < sp.BatchSize {
			// 不足一 batch → 已扫到末尾
			break
		}
	}
	res.FinishedAt = time.Now()
	return res
}

// fetchBatch 拉一 batch 行，返 maxPK 用于下轮分页。
func fetchBatch(ctx context.Context, db *sql.DB, sp *Spec, colExpr, pkCol string, lastPK int64) ([]map[string]any, int64, error) {
	q := fmt.Sprintf("SELECT %s FROM %s WHERE %s > ?", colExpr, sp.Table, pkCol)
	args := []any{lastPK}
	if sp.Where != "" {
		q += " AND " + sp.Where
		args = append(args, sp.WhereArgs...)
	}
	q += fmt.Sprintf(" ORDER BY %s ASC LIMIT %d", pkCol, sp.BatchSize)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, lastPK, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	colNames, _ := rows.Columns()
	out := make([]map[string]any, 0, sp.BatchSize)
	maxPK := lastPK
	for rows.Next() {
		vals := make([]any, len(colNames))
		ptrs := make([]any, len(colNames))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return out, maxPK, fmt.Errorf("scan: %w", err)
		}
		row := make(map[string]any, len(colNames))
		for i, name := range colNames {
			row[name] = unwrapBytes(vals[i])
		}
		if pk, ok := row[pkCol].(int64); ok && pk > maxPK {
			maxPK = pk
		}
		out = append(out, row)
	}
	return out, maxPK, rows.Err()
}

// rowToEvent 把单行转 cdc.Event。op 强制 INSERT（backfill 不知道历史 op，
// 但脚本用 ctx.get_by_index 不关心 op，只看 row 内容）。
func rowToEvent(sp *Spec, cols []string, row map[string]any, pkCol string) *cdc.Event {
	pk, _ := row[pkCol].(int64)
	if pk == 0 {
		if s, ok := row[pkCol].(string); ok && s != "" {
			// 字符串 PK 也允许（pi_xxx）
			ev := &cdc.Event{
				Service:   sp.Service,
				Table:     sp.Table,
				PK:        s,
				Op:        cdc.OpInsert,
				After:     filterCols(row, sp.IgnoreCols),
				Timestamp: rowTimestamp(row),
				Indexes:   collectIndexes(row, sp.IndexCols),
			}
			return ev
		}
		return nil
	}
	return &cdc.Event{
		Service:   sp.Service,
		Table:     sp.Table,
		PK:        fmt.Sprintf("%d", pk),
		Op:        cdc.OpInsert,
		After:     filterCols(row, sp.IgnoreCols),
		Timestamp: rowTimestamp(row),
		Indexes:   collectIndexes(row, sp.IndexCols),
	}
}

// rowTimestamp 取行时间戳（created_at / updated_at），缺省 now。
func rowTimestamp(row map[string]any) time.Time {
	for _, col := range []string{"created_at", "updated_at", "ts", "create_time", "update_time"} {
		if v, ok := row[col]; ok {
			if t, ok := v.(time.Time); ok {
				return t
			}
		}
	}
	return time.Now()
}

func filterCols(row map[string]any, ignore []string) map[string]any {
	if len(ignore) == 0 {
		return row
	}
	skip := make(map[string]struct{}, len(ignore))
	for _, c := range ignore {
		skip[c] = struct{}{}
	}
	out := make(map[string]any, len(row))
	for k, v := range row {
		if _, drop := skip[k]; !drop {
			out[k] = v
		}
	}
	return out
}

func collectIndexes(row map[string]any, idxCols []string) map[string]string {
	out := make(map[string]string, len(idxCols))
	for _, col := range idxCols {
		v, ok := row[col]
		if !ok || v == nil {
			continue
		}
		switch x := v.(type) {
		case string:
			out[col] = x
		case int64:
			out[col] = fmt.Sprintf("%d", x)
		default:
			out[col] = fmt.Sprintf("%v", v)
		}
	}
	return out
}

// tableColumns 拉表的列名清单（用于通用 SELECT *）。
func tableColumns(ctx context.Context, db *sql.DB, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT COLUMN_NAME FROM information_schema.columns
		   WHERE TABLE_NAME = ? AND TABLE_SCHEMA = DATABASE()
		   ORDER BY ORDINAL_POSITION`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func buildColExpr(cols []string) string {
	if len(cols) == 0 {
		return "*"
	}
	expr := ""
	for i, c := range cols {
		if i > 0 {
			expr += ","
		}
		expr += "`" + c + "`"
	}
	return expr
}

// unwrapBytes 把 []byte（mysql driver 偶尔返）转 string，方便 JSON 序列化。
func unwrapBytes(v any) any {
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return v
}
