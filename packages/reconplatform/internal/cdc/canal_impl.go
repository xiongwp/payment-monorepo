// canal_impl.go - go-mysql-org/go-mysql/canal 真实集成。
//
// 设计扩展点：
//
//   1. EventEnricher (插件链)
//      每条 Event 在 publish 前过 enricher 链，可以加业务字段、剔除 PII、
//      做 row 级过滤等。后续要加新 enricher 不用改 canal_impl，只需 register。
//
//      type EventEnricher interface {
//          Enrich(*Event) error  // 返 nil 继续；返 错误中止该事件 publish
//      }
//
//   2. PKResolver
//      默认从 source.tables[].pk 读 PK 配置；如果留空，自动从 schema 元信息
//      探测。生产里有些表（Hash 分片表、复合 PK 业务表）需要自定义解析逻辑——
//      实现 PKResolver 注册即可。
//
//   3. RowFilter
//      Source 配置的 ignore_columns 是基础；要做更复杂的"按值过滤"
//      （某些 status 状态的行不进 Redis）走 RowFilter 接口。
//
// canal 库要点：
//   - 一个 Canal 对应一个 MySQL endpoint（多分片要起多个 Canal）
//   - SetEventHandler 自定义 OnRow / OnTableChanged / OnPosSynced 回调
//   - Run() 阻塞直到 Close 或 binlog 错误
//   - canal 内部维护 schema 缓存，DDL 时自动刷新
//   - server-id 由调用方提供，必须全局唯一（不能跟业务库冲突）
package cdc

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-mysql-org/go-mysql/canal"
	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"
	"github.com/go-mysql-org/go-mysql/schema"
	"go.uber.org/zap"
)

// ─── 扩展点 1: EventEnricher 链 ─────────────────────────────────

// EventEnricher 在 publish 前最后加工 Event。链式调用，按 register 顺序。
//
// 使用场景：
//   - 添加业务派生字段（如计算 amount_in_usd）
//   - 剔除敏感信息（PCI、PII）
//   - 注入 trace_id（从 row 字段读出来塞进 indexes）
//   - 业务 key 标准化（pi_xxx → 大小写统一）
//
// 返 nil 继续；返 ErrSkipEvent 跳过本事件不 publish；返其他错误 → 记 metric 后跳过
// （不阻塞 binlog 读取）。
type EventEnricher interface {
	Name() string             // 给日志 / metric 用
	Enrich(e *Event) error    // 修改 Event 原地
}

// ErrSkipEvent enricher 主动决定跳过该事件（如某些 status=test 的不进 Redis）。
var ErrSkipEvent = fmt.Errorf("cdc: skip event")

// ─── 扩展点 2: RowFilter ─────────────────────────────────────────

// RowFilter 决定 row 是否进入 publish 链路。在 enricher 之前跑（enricher 可
// 加工，但不能阻止 publish；要阻止用 RowFilter 或 enricher 返 ErrSkipEvent）。
type RowFilter interface {
	Name() string
	// Allow 返 true 表示放行，false 跳过。
	Allow(svc, schema, table string, op Op, row map[string]any) bool
}

// ─── 真实 canal handler ──────────────────────────────────────────

// canalHandler 实现 canal.EventHandler 接口，把 row event 转成 cdc.Event 并 publish。
type canalHandler struct {
	source    Source
	publisher *Publisher
	schema    SchemaProvider
	logger    *zap.Logger

	enrichers []EventEnricher
	filters   []RowFilter

	// 当前 binlog 位点（OnPosSynced 更新；flushPositionThrottled 周期持久化）
	mu          sync.Mutex
	currentPos  mysql.Position
	currentGTID string
	lastFlushed time.Time
}

func newCanalHandler(src Source, pub *Publisher, sp SchemaProvider, logger *zap.Logger) *canalHandler {
	return &canalHandler{
		source:    src,
		publisher: pub,
		schema:    sp,
		logger:    logger,
	}
}

// AddEnricher / AddFilter 暴露扩展挂钩，main.go 可以挂自己的。
func (h *canalHandler) AddEnricher(e EventEnricher) { h.enrichers = append(h.enrichers, e) }
func (h *canalHandler) AddFilter(f RowFilter)       { h.filters = append(h.filters, f) }

// OnRotate / OnDDL / OnPosSynced 等位点 / 元信息事件（canal 接口要求）。

func (h *canalHandler) OnRotate(_ *replication.EventHeader, e *replication.RotateEvent) error {
	h.mu.Lock()
	h.currentPos = mysql.Position{Name: string(e.NextLogName), Pos: uint32(e.Position)}
	h.mu.Unlock()
	return nil
}

func (h *canalHandler) OnTableChanged(_ *replication.EventHeader, schemaName, tableName string) error {
	h.logger.Info("DDL detected; canal will refresh schema cache",
		zap.String("schema", schemaName), zap.String("table", tableName))
	return nil
}

func (h *canalHandler) OnDDL(_ *replication.EventHeader, nextPos mysql.Position, _ *replication.QueryEvent) error {
	h.mu.Lock()
	h.currentPos = nextPos
	h.mu.Unlock()
	return nil
}

func (h *canalHandler) OnXID(_ *replication.EventHeader, nextPos mysql.Position) error {
	h.mu.Lock()
	h.currentPos = nextPos
	h.mu.Unlock()
	return nil
}

func (h *canalHandler) OnGTID(_ *replication.EventHeader, gtid mysql.BinlogGTIDEvent) error {
	h.mu.Lock()
	h.currentGTID = gtid.String()
	h.mu.Unlock()
	return nil
}

func (h *canalHandler) OnPosSynced(_ *replication.EventHeader, pos mysql.Position, _ mysql.GTIDSet, _ bool) error {
	h.mu.Lock()
	h.currentPos = pos
	flush := time.Since(h.lastFlushed) >= time.Second
	if flush {
		h.lastFlushed = time.Now()
	}
	pos2 := h.currentPos
	gtid := h.currentGTID
	h.mu.Unlock()
	if flush {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := h.publisher.SavePosition(ctx, h.source.Service, pos2.Name, pos2.Pos, gtid); err != nil {
			h.logger.Warn("save binlog position failed", zap.Error(err))
		}
	}
	return nil
}

// String for canal logger
func (h *canalHandler) String() string {
	return "reconplatform-cdc:" + h.source.Service
}

// OnRow 核心逻辑：把 canal RowsEvent 转成 cdc.Event 并 publish。
//
// canal 给的 e.Rows 在 UPDATE 时是 [before0, after0, before1, after1, ...]
// 偶数索引是 before，奇数是 after。INSERT / DELETE 时只有一个 row。
func (h *canalHandler) OnRow(e *canal.RowsEvent) error {
	// 1) Schema / table 是否在订阅范围
	schemaName := e.Table.Schema
	tableName := e.Table.Name
	if !h.source.IsSchemaWatched(schemaName) {
		return nil
	}
	tcfg, ok := h.matchTable(tableName)
	if !ok {
		return nil
	}

	op := canalActionToOp(e.Action)
	cols := canalColumnsToInfos(e.Table.Columns)

	// PK 列：source 配置优先；缺省用 information_schema 推断。
	pkCols := tcfg.PK
	if len(pkCols) == 0 {
		pkCols = inferPKFromCanalTable(e.Table)
	}
	if len(pkCols) == 0 {
		h.logger.Warn("table has no PK; skip", zap.String("table", schemaName+"."+tableName))
		return nil
	}

	switch op {
	case OpInsert, OpDelete:
		for _, row := range e.Rows {
			if err := h.publishOne(schemaName, tableName, cols, nil, row, op, tcfg, pkCols); err != nil {
				h.logger.Warn("publish failed", zap.Error(err))
			}
		}
	case OpUpdate:
		// pairs of [before, after]
		for i := 0; i+1 < len(e.Rows); i += 2 {
			if err := h.publishOne(schemaName, tableName, cols, e.Rows[i], e.Rows[i+1], op, tcfg, pkCols); err != nil {
				h.logger.Warn("publish failed", zap.Error(err))
			}
		}
	}
	return nil
}

// publishOne 单行：parse + filter + enrich + publish
func (h *canalHandler) publishOne(
	schemaName, tableName string,
	cols []columnInfo,
	beforeRow, afterRow []any,
	op Op,
	tcfg TableConfig,
	pkCols []string,
) error {
	h.mu.Lock()
	pos := h.currentPos
	gtid := h.currentGTID
	h.mu.Unlock()

	evt, err := parseRow(
		h.source.Service, schemaName, tableName,
		cols, beforeRow, afterRow,
		op, time.Now(),
		pos.Name, pos.Pos, gtid,
		pkCols, tcfg.IndexColumns, tcfg.IgnoreColumns,
	)
	if err != nil {
		return fmt.Errorf("parseRow: %w", err)
	}

	// RowFilter 链：任何一个返 false → 跳过
	row := evt.Row()
	for _, f := range h.filters {
		if !f.Allow(evt.Service, evt.Schema, evt.Table, evt.Op, row) {
			return nil
		}
	}

	// EventEnricher 链
	for _, enr := range h.enrichers {
		if err := enr.Enrich(evt); err != nil {
			if err == ErrSkipEvent {
				return nil
			}
			h.logger.Warn("enricher error", zap.String("name", enr.Name()), zap.Error(err))
			// 不中止 publish；enricher 自负责
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return h.publisher.Publish(ctx, evt)
}

// matchTable 在 source.Tables 里找 table 对应的 TableConfig；支持
// 精确匹配 + table_pattern（accounting 100 表分表场景）。
func (h *canalHandler) matchTable(tableName string) (TableConfig, bool) {
	if cfg, ok := h.source.Tables[tableName]; ok {
		return cfg, true
	}
	// table_pattern fallback：遍历 source.Tables 看哪个 key 是 pattern
	for k, cfg := range h.source.Tables {
		if strings.HasSuffix(k, "_*") || strings.HasSuffix(k, "_\\d+") {
			// 简化：用 prefix 匹配（"account_transaction_*" 匹配 "account_transaction_5"）
			prefix := strings.TrimSuffix(strings.TrimSuffix(k, "_*"), "_\\d+")
			if strings.HasPrefix(tableName, prefix+"_") {
				return cfg, true
			}
		}
	}
	return TableConfig{}, false
}

// canalActionToOp canal 的 Action string → 我们的 Op 枚举。
func canalActionToOp(action string) Op {
	switch action {
	case canal.InsertAction:
		return OpInsert
	case canal.UpdateAction:
		return OpUpdate
	case canal.DeleteAction:
		return OpDelete
	}
	return OpInsert // 兜底
}

// canalColumnsToInfos 把 canal 的 *schema.TableColumn 转成 columnInfo。
func canalColumnsToInfos(cols []schema.TableColumn) []columnInfo {
	out := make([]columnInfo, 0, len(cols))
	for _, c := range cols {
		out = append(out, columnInfo{
			Name: c.Name,
			Type: c.RawType,
		})
	}
	return out
}

// inferPKFromCanalTable canal *schema.Table 自带 PK 信息。
func inferPKFromCanalTable(t *schema.Table) []string {
	if t == nil || len(t.PKColumns) == 0 {
		return nil
	}
	out := make([]string, 0, len(t.PKColumns))
	for _, idx := range t.PKColumns {
		if idx >= 0 && idx < len(t.Columns) {
			out = append(out, t.Columns[idx].Name)
		}
	}
	return out
}

// ─── newRealCanal: 替换 canalLike stub 的真实实现 ───────────────

// newRealCanal 由 Runner.Run 调用替换 stub。本文件 build 进 binary 时
// canal 包真接进来；测试场景仍可用 stub。
func newRealCanal(src Source, h *canalHandler, instanceAddr string) (canalLike, error) {
	cfg := canal.NewDefaultConfig()
	cfg.Addr = instanceAddr
	cfg.User = src.User
	cfg.Password = src.Password
	cfg.ServerID = src.ServerID
	cfg.Flavor = "mysql"
	cfg.Charset = "utf8mb4"

	// 只订阅指定 schema + table。canal 接受正则
	if len(src.Schemas) > 0 {
		cfg.IncludeTableRegex = make([]string, 0, len(src.Schemas))
		for _, s := range src.Schemas {
			// 转成 canal 期望的 "schema\\.table" 格式
			cfg.IncludeTableRegex = append(cfg.IncludeTableRegex, s+"\\..*")
		}
	}

	c, err := canal.NewCanal(cfg)
	if err != nil {
		return nil, fmt.Errorf("canal.NewCanal: %w", err)
	}
	c.SetEventHandler(h)
	return &realCanal{c: c}, nil
}

// realCanal 把 *canal.Canal 包成我们的 canalLike 接口。
type realCanal struct {
	c *canal.Canal
}

func (r *realCanal) Run() error  {
	// canal 默认从 latest 起跑；要按 saved position 起跑用 RunFrom。
	// MVP：先跑 latest，等运行稳定再加位点 resume（OnPosSynced 已在 SavePosition）。
	return r.c.Run()
}
func (r *realCanal) Close() {
	r.c.Close()
}
