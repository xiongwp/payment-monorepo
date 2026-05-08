package cdc

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// rowParser 把 canal 给的 row event 转成 cdc.Event。
//
// 输入 raw row 是 []interface{}（go-mysql 库的格式：每个元素对应一个列）。
// 我们用 schema（每个表的 column 名 + 类型）把它转成 map[string]any，
// 提取 PK + index columns，最终得到一条可发布的 Event。
//
// 这里只做转换；不调 Redis（那是 publisher 的事）。

// columnInfo 解析所需的列元信息。从 information_schema 同步得来。
type columnInfo struct {
	Name string
	Type string // "varchar", "int", "datetime", ...
}

// parseRow 把 (svc, schema, table, columns, beforeRow, afterRow, op, ts, ...)
// 拼成 Event。binlog 来源给的 BeforeRow / AfterRow 都是 []any，按 columns
// 顺序对应。一边按 PK 拼 pk 字符串，一边按 index_columns 抽 index 值。
//
// 注意：UPDATE 时 PK 用 AfterRow（PK 改了的话以新值为准；现实里 PK 一般不改）。
//      INSERT 用 AfterRow，DELETE 用 BeforeRow。
func parseRow(
	svc, schema, table string,
	columns []columnInfo,
	beforeRow, afterRow []any,
	op Op,
	ts time.Time,
	binlogFile string, binlogPos uint32, gtid string,
	pkCols, idxCols, ignoreCols []string,
) (*Event, error) {
	if len(columns) == 0 {
		return nil, fmt.Errorf("parseRow: empty columns for %s/%s.%s", svc, schema, table)
	}

	// 选哪份 row 提取 PK / index：
	// INSERT 与 UPDATE 用 after，DELETE 用 before。
	source := afterRow
	if op == OpDelete {
		source = beforeRow
	}
	if len(source) == 0 {
		return nil, fmt.Errorf("parseRow: empty source row for %s/%s.%s op=%s", svc, schema, table, op)
	}

	// 列名 → 索引位置
	colIndex := make(map[string]int, len(columns))
	for i, c := range columns {
		colIndex[c.Name] = i
	}

	// 1) PK 提取
	pk, err := buildPK(source, colIndex, pkCols)
	if err != nil {
		return nil, fmt.Errorf("parseRow: %s/%s.%s: %w", svc, schema, table, err)
	}

	// 2) Indexes 提取（每个 idx_col 在 source row 里的值）
	indexes := make(map[string]string, len(idxCols))
	for _, col := range idxCols {
		idx, ok := colIndex[col]
		if !ok || idx >= len(source) {
			continue // 列不存在；可能 schema 改了，跳过不报错
		}
		indexes[col] = stringify(source[idx])
	}

	// 3) Before / After map 构造（剔除 ignore cols）
	ignore := make(map[string]struct{}, len(ignoreCols))
	for _, c := range ignoreCols {
		ignore[c] = struct{}{}
	}

	var before, after map[string]any
	if len(beforeRow) > 0 {
		before = rowToMap(beforeRow, columns, ignore)
	}
	if len(afterRow) > 0 {
		after = rowToMap(afterRow, columns, ignore)
	}

	return &Event{
		Service:    svc,
		Schema:     schema,
		Table:      table,
		PK:         pk,
		Op:         op,
		Before:     before,
		After:      after,
		BinlogFile: binlogFile,
		BinlogPos:  binlogPos,
		GTID:       gtid,
		Timestamp:  ts,
		Indexes:    indexes,
	}, nil
}

// buildPK 多列联合主键拼成 "v1|v2|v3"。空 pkCols 视为配置错误。
func buildPK(row []any, colIndex map[string]int, pkCols []string) (string, error) {
	if len(pkCols) == 0 {
		return "", fmt.Errorf("buildPK: pk columns empty (auto-detect from information_schema not yet wired)")
	}
	parts := make([]string, 0, len(pkCols))
	for _, col := range pkCols {
		idx, ok := colIndex[col]
		if !ok {
			return "", fmt.Errorf("buildPK: pk column %q not in row schema", col)
		}
		if idx >= len(row) {
			return "", fmt.Errorf("buildPK: pk column %q index %d out of row range %d", col, idx, len(row))
		}
		parts = append(parts, stringify(row[idx]))
	}
	return strings.Join(parts, "|"), nil
}

// rowToMap row 转 map，跳过 ignore 列。
// 这里把所有值统一转 Go 原生类型（string / int64 / float64 / bool / []byte）；
// time.Time 转 RFC3339 字符串便于 JSON。
func rowToMap(row []any, columns []columnInfo, ignore map[string]struct{}) map[string]any {
	m := make(map[string]any, len(columns))
	for i, c := range columns {
		if i >= len(row) {
			break
		}
		if _, skip := ignore[c.Name]; skip {
			continue
		}
		m[c.Name] = normalize(row[i])
	}
	return m
}

// normalize 把 driver 给的各种原始类型统一成 JSON-friendly。
func normalize(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case time.Time:
		if x.IsZero() {
			return nil
		}
		return x.UTC().Format(time.RFC3339Nano)
	case []byte:
		// MySQL VARCHAR / TEXT 在 driver 里常以 []byte 出现 → 转 string
		// 二进制数据（BLOB）也变 string，反序列化时按需解码
		return string(x)
	default:
		return v
	}
}

// stringify 把任意值转成稳定字符串，给 PK / 索引 value 用。
func stringify(v any) string {
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case int32:
		return strconv.FormatInt(int64(x), 10)
	case int:
		return strconv.FormatInt(int64(x), 10)
	case uint64:
		return strconv.FormatUint(x, 10)
	case uint32:
		return strconv.FormatUint(uint64(x), 10)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 32)
	case bool:
		if x {
			return "1"
		}
		return "0"
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	default:
		return fmt.Sprintf("%v", x)
	}
}
