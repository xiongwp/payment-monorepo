// 内置 EventEnricher / RowFilter 实现。
//
// 这些是即用即可的常见加工器；自定义业务逻辑写自己的实现即可。
//
// 注册示例（main.go 启动期）：
//
//	mgr.AddGlobalEnricher(cdc.TraceIDEnricher{})
//	mgr.AddGlobalEnricher(cdc.MaskPIIEnricher{Cols: []string{"phone","email"}})
//	mgr.AddGlobalFilter(cdc.SkipShadowRowsFilter{})
package cdc

import (
	"strings"
)

// TraceIDEnricher 把 row 里 trace_id / x_trace_id / shadow 等业务列提到
// Indexes 顶层，方便脚本快速跨服务关联（已经在 source 配置里 index_columns
// 也声明的话其实重复，但有些场景 source 没显式配，这里兜底注入）。
type TraceIDEnricher struct{}

func (TraceIDEnricher) Name() string { return "trace-id" }

func (TraceIDEnricher) Enrich(e *Event) error {
	if e == nil {
		return nil
	}
	row := e.Row()
	if row == nil {
		return nil
	}
	if e.Indexes == nil {
		e.Indexes = make(map[string]string)
	}
	for _, col := range []string{"trace_id", "x_trace_id", "request_id"} {
		if _, ok := e.Indexes[col]; ok {
			continue // 已经在 source.IndexColumns 里声明了
		}
		if v, ok := row[col]; ok && v != nil {
			if s, ok := v.(string); ok && s != "" {
				e.Indexes[col] = s
			}
		}
	}
	return nil
}

// MaskPIIEnricher 把指定列内容脱敏（保留长度 + 末尾 4 位）。给合规 / PII 守护
// 用。Cols 列表精确匹配（不模糊；想模糊就写多个）。
//
//	cdc.MaskPIIEnricher{Cols: []string{"phone","email","id_card"}}
type MaskPIIEnricher struct {
	Cols []string // 例 ["phone", "email"]
}

func (m MaskPIIEnricher) Name() string { return "mask-pii" }

func (m MaskPIIEnricher) Enrich(e *Event) error {
	if e == nil || len(m.Cols) == 0 {
		return nil
	}
	for _, col := range m.Cols {
		maskCol(e.Before, col)
		maskCol(e.After, col)
	}
	return nil
}

func maskCol(row map[string]any, col string) {
	if row == nil {
		return
	}
	v, ok := row[col]
	if !ok || v == nil {
		return
	}
	s, ok := v.(string)
	if !ok {
		return
	}
	if len(s) <= 4 {
		row[col] = strings.Repeat("*", len(s))
		return
	}
	row[col] = strings.Repeat("*", len(s)-4) + s[len(s)-4:]
}

// SkipShadowRowsFilter 跳过影子流量数据（业务 row 里有 _shadow=1 / shadow=true 字段）。
// 业务用 X-Shadow header 落到影子表的写入；recon 通常不需要对账影子数据。
type SkipShadowRowsFilter struct{}

func (SkipShadowRowsFilter) Name() string { return "skip-shadow" }

func (SkipShadowRowsFilter) Allow(svc, schema, table string, op Op, row map[string]any) bool {
	if row == nil {
		return true
	}
	if v, ok := row["_shadow"]; ok && truthy(v) {
		return false
	}
	if v, ok := row["shadow"]; ok && truthy(v) {
		return false
	}
	// schema 名字带 _shadow 后缀（some 服务直接用 shadow database 而不是字段）
	if strings.HasSuffix(schema, "_shadow") {
		return false
	}
	return true
}

// SkipDeletedFilter 跳过 deleted=1 的软删行（可选；业务层一般还想看 DELETE）。
type SkipDeletedFilter struct{}

func (SkipDeletedFilter) Name() string { return "skip-soft-deleted" }

func (SkipDeletedFilter) Allow(_, _, _ string, _ Op, row map[string]any) bool {
	if row == nil {
		return true
	}
	if v, ok := row["deleted"]; ok && truthy(v) {
		return false
	}
	if v, ok := row["is_deleted"]; ok && truthy(v) {
		return false
	}
	return true
}

func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case int:
		return x != 0
	case int32:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	case string:
		return x == "1" || strings.EqualFold(x, "true") || strings.EqualFold(x, "yes")
	}
	return false
}
