package repo

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
)

// embeddedMetaMigration is the bundled meta-DB migration. It's the same file
// shipped under database/metadb/migrations/ but embedded into the binary so
// a deployment that doesn't ship the `database/` dir still gets the
// self-healing schema apply at startup.
//
// Everything inside uses `CREATE TABLE IF NOT EXISTS`, so running it on a
// fully up-to-date DB is a no-op.
//
//go:embed schema_migration.sql
var embeddedMetaMigration string

// shardTableBases 列出所有按 100 分片的业务表前缀；ApplyShadowTables 据此对每个
// (shard, base) 建一份 _shadow 副本。新增分片表时这里要同步登记，
// 同时 database/orderdb/scripts/generate.sh 的 SHADOW_BASES 也要保持一致。
var shardTableBases = []string{
	"payment_intent",
	"charge",
	"refund",
	"pay_action",
	"dispute",
	"dispute_event",
	"inbound_webhook",
	"notify_log",
	"accounting_outbox",
}

// metaShadowTables order_meta 库需要建影子副本的表清单。
// 与 database/metadb/init/init_shadow.sql 一一对应。
// leaf_alloc 也复制：让 shadow 流量从 leaf_alloc_shadow 取号，避免压测消耗主号段。
var metaShadowTables = []string{
	"leaf_alloc",
	"webhook_deliveries",
	"admin_audit_log",
	"gl_account",
	"gl_transaction",
	"gl_entry",
}

// ApplyMetaShadowTables 在 order_meta 上给每张需要影子的 meta 表创建 _shadow 副本。
// 启动期幂等兜底（init_shadow.sql 早跑过则全是 no-op）。
func (m *Manager) ApplyMetaShadowTables(ctx context.Context, logger *zap.Logger) error {
	if logger == nil {
		logger = zap.NewNop()
	}
	logger = logger.Named("meta-shadow-migrator")
	if m.meta == nil {
		return fmt.Errorf("ApplyMetaShadowTables: meta DB not configured")
	}

	created, skipped, failed := 0, 0, 0
	start := time.Now()
	for _, base := range metaShadowTables {
		shadowTbl := base + "_shadow"
		mainExists, err := tableExists(ctx, m.meta, m.metaName, base)
		if err != nil {
			failed++
			logger.Warn("introspect main failed", zap.String("tbl", base), zap.Error(err))
			continue
		}
		if !mainExists {
			skipped++ // init.sql 还没跑或者该表本部署不需要
			continue
		}
		ddl := fmt.Sprintf("CREATE TABLE IF NOT EXISTS `%s` LIKE `%s`", shadowTbl, base)
		if err := m.meta.WithContext(ctx).Exec(ddl).Error; err != nil {
			failed++
			logger.Warn("create LIKE failed", zap.String("tbl", shadowTbl), zap.Error(err))
			continue
		}
		created++
	}
	logger.Info("meta shadow tables ensured",
		zap.Int("created_or_existing", created),
		zap.Int("skipped_no_main", skipped),
		zap.Int("failed", failed),
		zap.Duration("duration", time.Since(start)))
	return nil
}

// ApplyShadowTables 启动期对每张业务分片表创建 _shadow 副本：
//
//	CREATE TABLE IF NOT EXISTS payment_intent_42_shadow LIKE payment_intent_42
//
// 行为：
//   - 主表不存在 → 跳过（init 流程未跑完 / 该业务表不在本 shard）
//   - 影子表已存在 → 跳过
//   - 单表错误不阻断其他表
//
// LIKE 复制结构（含索引），不复制 trigger / FK（业务表本就没用）。运行期 shadow
// 流量经 Router.TableName(ctx,…) 直接落到 _shadow 表，与主表 schema 同步演进。
func (m *Manager) ApplyShadowTables(ctx context.Context, tablePerDB int, logger *zap.Logger) error {
	if logger == nil {
		logger = zap.NewNop()
	}
	logger = logger.Named("shadow-migrator")

	created, skipped, failed := 0, 0, 0
	start := time.Now()
	for shardIdx, db := range m.shards {
		dbName := m.shardNames[shardIdx]
		for tIdx := 0; tIdx < tablePerDB; tIdx++ {
			global := shardIdx*tablePerDB + tIdx
			suffix := fmt.Sprintf("%02d", global)
			for _, base := range shardTableBases {
				main := fmt.Sprintf("%s_%s", base, suffix)
				shadowTbl := main + "_shadow"

				mainExists, err := tableExists(ctx, db, dbName, main)
				if err != nil {
					failed++
					logger.Warn("shadow migrator: introspect main failed",
						zap.String("db", dbName), zap.String("tbl", main), zap.Error(err))
					continue
				}
				if !mainExists {
					skipped++
					continue
				}
				ddl := fmt.Sprintf("CREATE TABLE IF NOT EXISTS `%s` LIKE `%s`", shadowTbl, main)
				if err := db.WithContext(ctx).Exec(ddl).Error; err != nil {
					failed++
					logger.Warn("shadow migrator: create LIKE failed",
						zap.String("db", dbName), zap.String("tbl", shadowTbl), zap.Error(err))
					continue
				}
				created++
			}
		}
	}
	logger.Info("shadow tables ensured",
		zap.Int("created_or_existing", created),
		zap.Int("skipped_no_main", skipped),
		zap.Int("failed", failed),
		zap.Duration("duration", time.Since(start)))
	return nil
}

// tableExists INFORMATION_SCHEMA 查表是否存在（schema 内）。
func tableExists(ctx context.Context, db *gorm.DB, dbName, tbl string) (bool, error) {
	var n int
	err := db.WithContext(ctx).Raw(
		"SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA=? AND TABLE_NAME=?",
		dbName, tbl,
	).Scan(&n).Error
	return n > 0, err
}

// ApplyShardMigrations runs idempotent ALTER TABLE additions across all 100
// accounting_outbox_NN tables to roll forward old shard DBs that don't yet
// have the claim_token column / idx_claim index.
//
// New deployments get the columns from templates/schema.sql; this function is
// for already-bootstrapped DBs to self-heal on startup. Each statement uses
// INFORMATION_SCHEMA to test column / index existence before issuing the DDL,
// because MySQL 5.7 / 8.0 lack `ALTER TABLE … ADD COLUMN IF NOT EXISTS`.
//
// Errors per-table are logged and counted; we don't abort startup so a single
// stale shard doesn't take the whole service down.
func (m *Manager) ApplyShardMigrations(ctx context.Context, tablePerDB int, logger *zap.Logger) error {
	if logger == nil {
		logger = zap.NewNop()
	}
	logger = logger.Named("shard-migrator")

	migrations := []shardColumnMigration{
		{
			TablePrefix: "accounting_outbox_",
			ColumnName:  "claim_token",
			ColumnDDL:   "ADD COLUMN `claim_token` VARCHAR(64) DEFAULT NULL",
		},
		{
			TablePrefix: "accounting_outbox_",
			IndexName:   "idx_claim",
			IndexDDL:    "ADD KEY `idx_claim` (`claim_token`)",
		},
	}

	applied, skipped, failed := 0, 0, 0
	start := time.Now()
	for shardIdx, db := range m.shards {
		dbName := m.shardNames[shardIdx]
		for tIdx := 0; tIdx < tablePerDB; tIdx++ {
			global := shardIdx*tablePerDB + tIdx
			suffix := fmt.Sprintf("%02d", global)
			for _, mig := range migrations {
				tbl := mig.TablePrefix + suffix
				exists, err := mig.exists(ctx, db, dbName, tbl)
				if err != nil {
					failed++
					logger.Warn("shard migration introspect failed",
						zap.String("db", dbName), zap.String("tbl", tbl),
						zap.String("change", mig.what()), zap.Error(err))
					continue
				}
				if exists {
					skipped++
					continue
				}
				ddl := fmt.Sprintf("ALTER TABLE `%s` %s", tbl, mig.changeDDL())
				if err := db.WithContext(ctx).Exec(ddl).Error; err != nil {
					failed++
					logger.Warn("shard migration apply failed",
						zap.String("db", dbName), zap.String("tbl", tbl),
						zap.String("change", mig.what()), zap.Error(err))
					continue
				}
				applied++
			}
		}
	}
	logger.Info("shard migrations applied",
		zap.Int("applied", applied),
		zap.Int("skipped", skipped),
		zap.Int("failed", failed),
		zap.Duration("duration", time.Since(start)))
	return nil
}

// shardColumnMigration 描述一次列或索引添加。
// 设置 ColumnName + ColumnDDL 表示加列；设置 IndexName + IndexDDL 表示加索引。
type shardColumnMigration struct {
	TablePrefix string
	ColumnName  string
	ColumnDDL   string // e.g. "ADD COLUMN `claim_token` VARCHAR(64) DEFAULT NULL"
	IndexName   string
	IndexDDL    string // e.g. "ADD KEY `idx_claim` (`claim_token`)"
}

func (s shardColumnMigration) what() string {
	if s.ColumnName != "" {
		return "column:" + s.ColumnName
	}
	return "index:" + s.IndexName
}

func (s shardColumnMigration) changeDDL() string {
	if s.ColumnDDL != "" {
		return s.ColumnDDL
	}
	return s.IndexDDL
}

func (s shardColumnMigration) exists(ctx context.Context, db *gorm.DB, dbName, tbl string) (bool, error) {
	if s.ColumnName != "" {
		var n int
		err := db.WithContext(ctx).Raw(
			"SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND COLUMN_NAME=?",
			dbName, tbl, s.ColumnName,
		).Scan(&n).Error
		return n > 0, err
	}
	var n int
	err := db.WithContext(ctx).Raw(
		"SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND INDEX_NAME=?",
		dbName, tbl, s.IndexName,
	).Scan(&n).Error
	return n > 0, err
}

// ApplyMetaMigration runs the embedded meta-DB migration against the meta
// connection. Called once on startup before any service opens a transaction.
//
// It's safe to call repeatedly — every statement is idempotent. Errors are
// logged per-statement rather than fatal: a CHECK-constraint row in a newer
// MySQL may fail on 5.7 but we still want the working CREATE TABLEs to apply.
func (m *Manager) ApplyMetaMigration(ctx context.Context, logger *zap.Logger) error {
	if m.meta == nil {
		return fmt.Errorf("ApplyMetaMigration: meta DB not configured")
	}
	if strings.TrimSpace(embeddedMetaMigration) == "" {
		return nil
	}
	stmts := splitSQLStatements(embeddedMetaMigration)
	if logger == nil {
		logger = zap.NewNop()
	}
	logger = logger.Named("schema-migrator")

	applied, skipped, failed := 0, 0, 0
	start := time.Now()
	for _, raw := range stmts {
		stmt := strings.TrimSpace(raw)
		if stmt == "" {
			continue
		}
		// Skip pure comments.
		if isOnlyComments(stmt) {
			skipped++
			continue
		}
		if err := runMigrationStmt(ctx, m.meta, stmt); err != nil {
			failed++
			logger.Warn("meta migration statement failed (continuing)",
				zap.String("head", head(stmt, 120)),
				zap.Error(err))
			continue
		}
		applied++
	}
	logger.Info("meta migration applied",
		zap.Int("applied", applied),
		zap.Int("skipped", skipped),
		zap.Int("failed", failed),
		zap.Duration("duration", time.Since(start)))
	if failed > 0 {
		// Don't block startup; admin can inspect logs + re-run manually if
		// something critical failed.
		return nil
	}
	return nil
}

func runMigrationStmt(ctx context.Context, db *gorm.DB, stmt string) error {
	return db.WithContext(ctx).Exec(stmt).Error
}

// splitSQLStatements cuts a SQL script on ';' boundaries. Good enough for our
// DDL (no stored procedures / triggers / delimiters). It respects quoted
// strings so semicolons inside COMMENT '...' survive.
func splitSQLStatements(src string) []string {
	var (
		out     []string
		buf     strings.Builder
		inStr   byte // 0, ', ", `
	)
	for i := 0; i < len(src); i++ {
		c := src[i]
		if inStr == 0 {
			switch c {
			case '\'', '"', '`':
				inStr = c
			case ';':
				out = append(out, buf.String())
				buf.Reset()
				continue
			}
		} else if c == inStr {
			// treat `\'` or doubled `''` inside strings gracefully
			if i > 0 && src[i-1] == '\\' {
				// escaped, stay in string
			} else {
				inStr = 0
			}
		}
		buf.WriteByte(c)
	}
	if rest := strings.TrimSpace(buf.String()); rest != "" {
		out = append(out, rest)
	}
	return out
}

// isOnlyComments returns true if a statement is just SQL comments (-- / #).
func isOnlyComments(stmt string) bool {
	for _, line := range strings.Split(stmt, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if !(strings.HasPrefix(t, "--") || strings.HasPrefix(t, "#")) {
			return false
		}
	}
	return true
}

func head(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
