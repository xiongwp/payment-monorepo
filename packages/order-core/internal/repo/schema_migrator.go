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
