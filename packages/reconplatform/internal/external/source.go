// Package external — 外部数据源摄入（银行流水 / 渠道日报 / 卡组对账文件）。
//
// 真实对账 50% 的数据来自外部 — Stripe / 银行 / 渠道 / 卡组每日发对账文件
// （CSV/Excel/JSON over SFTP/HTTPS/S3），需要拿来跟内部 binlog 数据对账。
//
// 架构对比：
//
//	internal CDC (cdc/):
//	  binlog 实时 → row → publisher → recon:event:{svc}:{table}:{pk}
//
//	external file (本包):
//	  cron 拉文件 → CSV parser → row → publisher → recon:event:external:{name}:{pk}
//
// 共用同一个 Redis key 布局（recon:event:* / recon:idx:*），脚本无须区分
// 数据来自 binlog 还是外部文件 — 都是 ctx.get_by_index("trace_id", ...) 即可。
//
// 文件来源：
//
//	SFTP — 最常见，渠道公司每日发文件到指定 sftp 账号
//	S3   — 云原生场景，文件落 s3://bucket/path/yyyymmdd/file.csv
//	Local — dev / 单元测试 / 一次性手工导入
//
// 配置（config-center key=reconplatform/external.sources）：
//
//	sources:
//	  - name: visa_settlement
//	    schedule: "0 2 * * *"             # 每天 02:00
//	    transport:
//	      type: sftp
//	      host: sftp.visa.com
//	      port: 22
//	      user: ${VISA_SFTP_USER}
//	      password: ${VISA_SFTP_PASS}
//	      path: /reports/{{.YYYYMMDD}}/settlement.csv
//	    parser:
//	      type: csv
//	      delimiter: ","
//	      header: true
//	      timezone: UTC
//	    schema:
//	      pk: [transaction_id]
//	      index_columns: [pi_id, merchant_id, settlement_date]
//	      columns:
//	        transaction_id: { type: string }
//	        pi_id:          { type: string }
//	        amount:         { type: int }
//	        currency:       { type: string }
//	        settlement_date:{ type: date }
//
// 脚本侧透明使用：
//
//	external = ctx.get_by_index("pi_id", pi.id, source="visa_settlement")
//	if external and abs(pi.amount - external["amount"]) > 0:
//	    diffs.append({"type": "settlement_amount_mismatch", ...})

package external

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"reconcile-system/internal/cdc"
)

func encodeJSON(v any) ([]byte, error) { return json.Marshal(v) }

// Op 跟 cdc.Op 一样（对脚本透明），但外部数据通常都是 INSERT。
type Op = cdc.Op

const OpInsertExternal = cdc.OpInsert

// Transport 抽象拿文件方式 — sftp / s3 / local。
//
// Path 由调用方算好（templating 处理过）传入；Transport 不再关心路径模板。
type Transport interface {
	Name() string
	// Fetch 拉一份文件 returns reader + cleanup func（关 conn 等）。
	Fetch(ctx context.Context, path string) (io.ReadCloser, error)
	// DefaultPath 提供 Transport 内部存的默认 path（如 sftp.path 配置）；
	// Importer 用 src.PathTpl 优先，否则回退这个。
	DefaultPath() string
}

// Parser 把字节流解析成行。CSV / JSON-NDJSON / Excel 各一种实现。
type Parser interface {
	Name() string
	// Parse 流式 yield 每行 map[string]any（按 schema 类型转）。
	Parse(r io.Reader, schema *Schema, yield func(row map[string]any) error) error
}

// Schema 外部表 schema 声明。
type Schema struct {
	Name          string             `yaml:"name" json:"name"`         // 给脚本 ctx.get_by_index source= 用
	PK            []string           `yaml:"pk" json:"pk"`             // 主键列（拼 redis key）
	IndexColumns  []string           `yaml:"index_columns" json:"index_columns"`
	Columns       map[string]ColumnT `yaml:"columns" json:"columns"`
	IgnoreColumns []string           `yaml:"ignore_columns" json:"ignore_columns,omitempty"`
}

// ColumnT 列类型 — 跟 cdc 用的差不多但简化（只支持 CSV 能表达的）。
type ColumnT struct {
	Type     string `yaml:"type" json:"type"`             // string / int / float / bool / date / datetime
	Format   string `yaml:"format,omitempty" json:"format,omitempty"` // for date/datetime
	Nullable bool   `yaml:"nullable,omitempty" json:"nullable,omitempty"`
}

// Source 一个完整的外部源声明（一个对账文件 = 一个 Source）。
type Source struct {
	Name      string         `yaml:"name" json:"name"`
	Schedule  string         `yaml:"schedule" json:"schedule"`             // cron 表达式
	Transport map[string]any `yaml:"transport" json:"transport"`           // 见各 Transport 实现
	ParserCfg map[string]any `yaml:"parser" json:"parser"`                 // 见各 Parser 实现
	Schema    Schema         `yaml:"schema" json:"schema"`
	PathTpl   string         `yaml:"path_template,omitempty" json:"path_template,omitempty"` // 兜底变量 {{.YYYYMMDD}}
}

// Importer 把 transport+parser 组合成可执行的导入器。
type Importer struct {
	src       Source
	transport Transport
	parser    Parser
	rdb       redis.UniversalClient
	log       *zap.Logger
}

// NewImporter 工厂；caller 自己挑 transport/parser 实现注入。
func NewImporter(src Source, t Transport, p Parser, rdb redis.UniversalClient, log *zap.Logger) *Importer {
	if log == nil {
		log = zap.NewNop()
	}
	return &Importer{src: src, transport: t, parser: p, rdb: rdb, log: log}
}

// Run 单次拉取 + 解析 + 写 Redis。
//
// 返导入行数 + error。失败时部分行可能已经写入（按 PK 幂等，再跑安全）。
//
// 路径模板：path_template / transport.path 都能写 {{.YYYYMMDD}} {{.YYYY-MM-DD}}
// {{.YYYYMM}} 等变量，启动时替换成当天值。
func (im *Importer) Run(ctx context.Context) (int, error) {
	t := time.Now().UTC()
	rawPath := im.src.PathTpl
	if rawPath == "" {
		rawPath = im.transport.DefaultPath()
	}
	path := renderPath(rawPath, t)
	im.log.Info("external import start",
		zap.String("source", im.src.Name),
		zap.String("path", path))

	rc, err := im.transport.Fetch(ctx, path)
	if err != nil {
		return 0, fmt.Errorf("fetch %s: %w", path, err)
	}
	defer rc.Close()

	count := 0
	err = im.parser.Parse(rc, &im.src.Schema, func(row map[string]any) error {
		// 1. 拼 PK 字符串
		pk, err := buildPK(row, im.src.Schema.PK)
		if err != nil {
			return err
		}
		// 2. 走 cdc.Publisher 同款路径写 Redis（recon:event:external:{name}:{pk}）
		ev := &cdc.Event{
			Service: "external",
			Schema:  im.src.Name,
			Table:   im.src.Name,
			PK:      pk,
			Op:      cdc.OpInsert,
			After:   row,
			Indexes: extractIndexes(row, im.src.Schema.IndexColumns),
			Ts:      time.Now().UnixMilli(),
		}
		if err := im.publishOne(ctx, ev); err != nil {
			im.log.Warn("publish row failed",
				zap.String("source", im.src.Name),
				zap.String("pk", pk),
				zap.Error(err))
			return nil // 单行失败不阻断整批
		}
		count++
		return nil
	})
	if err != nil {
		return count, fmt.Errorf("parse %s: %w", path, err)
	}
	im.log.Info("external import done",
		zap.String("source", im.src.Name),
		zap.Int("rows", count))
	return count, nil
}

// publishOne 直接走 Redis 写，不复用 cdc.Publisher 因为 cdc 那个是给 binlog 设计的
// （多 source/multi-shard 状态机）。这里更简单：1 source 1 file 1 batch。
func (im *Importer) publishOne(ctx context.Context, ev *cdc.Event) error {
	pipe := im.rdb.Pipeline()
	// recon:event:external:<source>:<pk> 存整行 JSON
	key := fmt.Sprintf("recon:event:external:%s:%s", ev.Schema, ev.PK)
	body, _ := encodeJSON(ev)
	pipe.Set(ctx, key, body, 30*24*time.Hour)
	// recon:idx:<idx_col>:<value> SADD "external:<source>:<pk>"
	for col, val := range ev.Indexes {
		idxKey := fmt.Sprintf("recon:idx:%s:%s", col, val)
		pipe.SAdd(ctx, idxKey, fmt.Sprintf("external:%s:%s", ev.Schema, ev.PK))
		pipe.Expire(ctx, idxKey, 30*24*time.Hour)
	}
	// recon:stream:events 同 binlog 也推一份给 SSE 实时看
	streamArgs := &redis.XAddArgs{
		Stream: "recon:stream:events",
		MaxLen: 100000,
		Approx: true,
		Values: map[string]any{
			"svc":   "external",
			"table": ev.Schema,
			"pk":    ev.PK,
			"op":    "INSERT",
			"ts":    ev.Ts,
		},
	}
	pipe.XAdd(ctx, streamArgs)
	_, err := pipe.Exec(ctx)
	return err
}

// ─── helpers ──────────────────────────────────────────────────────

// buildPK 按 schema.PK 拼出 redis key 用的复合主键（多列时 _ 分隔）。
func buildPK(row map[string]any, pkCols []string) (string, error) {
	if len(pkCols) == 0 {
		return "", fmt.Errorf("schema.pk is empty")
	}
	parts := make([]string, len(pkCols))
	for i, c := range pkCols {
		v, ok := row[c]
		if !ok || v == nil {
			return "", fmt.Errorf("PK column %q missing", c)
		}
		parts[i] = stringify(v)
	}
	return strings.Join(parts, "_"), nil
}

// extractIndexes 按 schema.index_columns 取出索引值（过滤 nil/空）。
func extractIndexes(row map[string]any, idxCols []string) map[string]string {
	out := map[string]string{}
	for _, c := range idxCols {
		v, ok := row[c]
		if !ok || v == nil {
			continue
		}
		s := stringify(v)
		if s != "" {
			out[c] = s
		}
	}
	return out
}

func stringify(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	case time.Time:
		return x.UTC().Format(time.RFC3339)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// renderPath 替换 path 里的时间模板变量。
func renderPath(path string, t time.Time) string {
	r := strings.NewReplacer(
		"{{.YYYYMMDD}}", t.Format("20060102"),
		"{{.YYYY-MM-DD}}", t.Format("2006-01-02"),
		"{{.YYYYMM}}", t.Format("200601"),
		"{{.YYYY}}", t.Format("2006"),
		"{{.MM}}", t.Format("01"),
		"{{.DD}}", t.Format("02"),
		"{{.HH}}", t.Format("15"),
	)
	return r.Replace(path)
}

// CSVParser 标准 CSV 解析器（最常见的对账文件格式）。
type CSVParser struct {
	Delimiter rune // 默认 ','
	HasHeader bool // 默认 true
	Timezone  string
}

func (CSVParser) Name() string { return "csv" }

func (p CSVParser) Parse(r io.Reader, schema *Schema, yield func(row map[string]any) error) error {
	cr := csv.NewReader(r)
	if p.Delimiter != 0 {
		cr.Comma = p.Delimiter
	} else {
		cr.Comma = ','
	}
	cr.LazyQuotes = true
	cr.FieldsPerRecord = -1 // 容忍每行列数不一致

	var header []string
	if p.HasHeader {
		first, err := cr.Read()
		if err != nil {
			return fmt.Errorf("read header: %w", err)
		}
		header = first
	}

	loc := time.UTC
	if p.Timezone != "" {
		if l, err := time.LoadLocation(p.Timezone); err == nil {
			loc = l
		}
	}

	rowNum := 0
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("row %d: %w", rowNum, err)
		}
		rowNum++
		row := make(map[string]any, len(rec))
		for i, val := range rec {
			var col string
			if i < len(header) {
				col = header[i]
			} else {
				col = fmt.Sprintf("col_%d", i)
			}
			// 按 schema.Columns 类型转换；找不到默认 string
			ct, ok := schema.Columns[col]
			if !ok {
				row[col] = val
				continue
			}
			conv, cerr := convertValue(val, ct, loc)
			if cerr != nil {
				return fmt.Errorf("row %d col %s: %w", rowNum, col, cerr)
			}
			row[col] = conv
		}
		if err := yield(row); err != nil {
			return err
		}
	}
	return nil
}

func convertValue(s string, ct ColumnT, loc *time.Location) (any, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		if ct.Nullable {
			return nil, nil
		}
		// 默认空当 zero-value
		switch ct.Type {
		case "int":
			return 0, nil
		case "float":
			return 0.0, nil
		case "bool":
			return false, nil
		}
		return "", nil
	}
	switch ct.Type {
	case "string", "":
		return s, nil
	case "int":
		return strconv.ParseInt(s, 10, 64)
	case "float":
		return strconv.ParseFloat(s, 64)
	case "bool":
		return strconv.ParseBool(s)
	case "date":
		layout := ct.Format
		if layout == "" {
			layout = "2006-01-02"
		}
		return time.ParseInLocation(layout, s, loc)
	case "datetime":
		layout := ct.Format
		if layout == "" {
			layout = "2006-01-02 15:04:05"
		}
		return time.ParseInLocation(layout, s, loc)
	}
	return s, nil // 未知类型当 string
}
