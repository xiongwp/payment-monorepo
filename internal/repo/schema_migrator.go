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
