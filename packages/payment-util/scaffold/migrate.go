// migrate.go — 简易 migration runner (golang-migrate 风格).
//
// 跟 AutoMigrate 区别:
//   AutoMigrate     — gorm 自动 ALTER TABLE; 没版本 / 没回滚 / dev 用
//   migration       — 版本化 SQL files; 跑过的记 schema_migrations 表; 生产用
//
// 文件命名: migrations/000001_init.up.sql  + 000001_init.down.sql
//                    000002_add_index.up.sql + 000002_add_index.down.sql
//
// 用法 (CLI):
//   ./service migrate up                  跑所有未执行的 up
//   ./service migrate up 5                只跑前 5 个未执行的 up
//   ./service migrate down 1              回滚最近 1 个 (跑对应 down.sql)
//   ./service migrate status              看版本状态
//   ./service migrate force <ver>         强制把版本设到 <ver> (修 dirty 用)
//
// 业务 main.go 支持 migrate 子命令:
//   scaffold.Run(scaffold.Opts{..., MigrationDir: "migrations"})
//
// 自动: argv[1] == "migrate" 时不起 fiber/grpc, 跑 migration 然后 exit.

package scaffold

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
)

// MigrationConfig 控制 migration 行为. scaffold.Opts.MigrationDir 启用.
type MigrationConfig struct {
	Dir            string // "migrations"
	Table          string // 默认 "schema_migrations"
	LockTimeout    time.Duration
}

// migrationStep 单条 migration
type migrationStep struct {
	Version  int64
	Name     string
	UpFile   string
	DownFile string
}

// runMigrate 由 main 检测到 argv "migrate" 后调; 跑完 os.Exit.
func runMigrate(cfg MigrationConfig, dbCfg DBConfig, log *zap.Logger, args []string) {
	if cfg.Dir == "" {
		cfg.Dir = "migrations"
	}
	if cfg.Table == "" {
		cfg.Table = "schema_migrations"
	}
	if cfg.LockTimeout == 0 {
		cfg.LockTimeout = 30 * time.Second
	}

	if dbCfg.DSN == "" {
		log.Fatal("migrate: DB DSN required")
	}

	// 用 raw sql.DB (绕过 GORM, 避免 AutoMigrate 干扰)
	dbDSN := dbCfg.DSN
	db, err := sql.Open("mysql", dbDSN)
	if err != nil {
		log.Fatal("migrate: open db", zap.Error(err))
	}
	defer db.Close()

	if err := ensureMigrationsTable(db, cfg.Table); err != nil {
		log.Fatal("migrate: create table", zap.Error(err))
	}

	steps, err := loadMigrationSteps(cfg.Dir)
	if err != nil {
		log.Fatal("migrate: load steps", zap.Error(err))
	}

	cmd := "status"
	if len(args) > 0 {
		cmd = args[0]
	}
	args = args[min(len(args), 1):]

	switch cmd {
	case "up":
		n := -1 // all
		if len(args) > 0 {
			n, _ = strconv.Atoi(args[0])
		}
		runUp(db, cfg.Table, steps, n, log)
	case "down":
		n := 1
		if len(args) > 0 {
			n, _ = strconv.Atoi(args[0])
		}
		runDown(db, cfg.Table, steps, n, log)
	case "status":
		printStatus(db, cfg.Table, steps, log)
	case "force":
		if len(args) == 0 {
			log.Fatal("migrate force: version required")
		}
		v, _ := strconv.ParseInt(args[0], 10, 64)
		forceVersion(db, cfg.Table, v, log)
	default:
		fmt.Println("usage: migrate up|down|status|force <ver>")
		os.Exit(1)
	}

	os.Exit(0)
}

func ensureMigrationsTable(db *sql.DB, table string) error {
	_, err := db.Exec(fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			version BIGINT NOT NULL PRIMARY KEY,
			name VARCHAR(255) NOT NULL,
			applied_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			dirty TINYINT(1) NOT NULL DEFAULT 0
		) ENGINE=InnoDB`, table))
	return err
}

// loadMigrationSteps 从目录读 NNNNNN_name.up.sql / NNNNNN_name.down.sql.
func loadMigrationSteps(dir string) ([]migrationStep, error) {
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read dir: %w", err)
	}
	stepMap := map[int64]*migrationStep{}
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		name := f.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		// 解析 NNNNNN_name.{up|down}.sql
		parts := strings.SplitN(name, "_", 2)
		if len(parts) != 2 {
			continue
		}
		ver, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			continue
		}
		rest := parts[1]
		direction := ""
		baseName := ""
		switch {
		case strings.HasSuffix(rest, ".up.sql"):
			direction = "up"
			baseName = strings.TrimSuffix(rest, ".up.sql")
		case strings.HasSuffix(rest, ".down.sql"):
			direction = "down"
			baseName = strings.TrimSuffix(rest, ".down.sql")
		default:
			continue
		}
		step, ok := stepMap[ver]
		if !ok {
			step = &migrationStep{Version: ver, Name: baseName}
			stepMap[ver] = step
		}
		if direction == "up" {
			step.UpFile = filepath.Join(dir, name)
		} else {
			step.DownFile = filepath.Join(dir, name)
		}
	}
	out := make([]migrationStep, 0, len(stepMap))
	for _, s := range stepMap {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

func loadAppliedVersions(db *sql.DB, table string) (map[int64]bool, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT version FROM %s`, table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[int64]bool{}
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		m[v] = true
	}
	return m, rows.Err()
}

func runUp(db *sql.DB, table string, steps []migrationStep, n int, log *zap.Logger) {
	applied, err := loadAppliedVersions(db, table)
	if err != nil {
		log.Fatal("load applied", zap.Error(err))
	}
	count := 0
	for _, s := range steps {
		if applied[s.Version] {
			continue
		}
		if n > 0 && count >= n {
			break
		}
		if s.UpFile == "" {
			log.Warn("skip: missing up file", zap.Int64("version", s.Version))
			continue
		}
		log.Info("applying", zap.Int64("version", s.Version), zap.String("name", s.Name))
		body, err := os.ReadFile(s.UpFile)
		if err != nil {
			log.Fatal("read up", zap.Error(err))
		}
		if err := execSQLScript(db, string(body)); err != nil {
			// 标 dirty
			_, _ = db.Exec(fmt.Sprintf("INSERT INTO %s (version, name, dirty) VALUES (?, ?, 1)", table), s.Version, s.Name)
			log.Fatal("apply failed (marked dirty)", zap.Error(err))
		}
		_, _ = db.Exec(fmt.Sprintf("INSERT INTO %s (version, name, dirty) VALUES (?, ?, 0)", table), s.Version, s.Name)
		count++
	}
	log.Info("up done", zap.Int("applied", count))
}

func runDown(db *sql.DB, table string, steps []migrationStep, n int, log *zap.Logger) {
	applied, err := loadAppliedVersions(db, table)
	if err != nil {
		log.Fatal("load applied", zap.Error(err))
	}
	// 倒序
	count := 0
	for i := len(steps) - 1; i >= 0; i-- {
		if count >= n {
			break
		}
		s := steps[i]
		if !applied[s.Version] {
			continue
		}
		if s.DownFile == "" {
			log.Warn("skip: missing down file", zap.Int64("version", s.Version))
			continue
		}
		log.Info("reverting", zap.Int64("version", s.Version))
		body, err := os.ReadFile(s.DownFile)
		if err != nil {
			log.Fatal("read down", zap.Error(err))
		}
		if err := execSQLScript(db, string(body)); err != nil {
			log.Fatal("revert failed", zap.Error(err))
		}
		_, _ = db.Exec(fmt.Sprintf("DELETE FROM %s WHERE version=?", table), s.Version)
		count++
	}
	log.Info("down done", zap.Int("reverted", count))
}

func printStatus(db *sql.DB, table string, steps []migrationStep, log *zap.Logger) {
	applied, _ := loadAppliedVersions(db, table)
	fmt.Printf("%-12s %-30s %-10s\n", "VERSION", "NAME", "STATUS")
	for _, s := range steps {
		state := "pending"
		if applied[s.Version] {
			state = "applied"
		}
		fmt.Printf("%-12d %-30s %-10s\n", s.Version, s.Name, state)
	}
}

func forceVersion(db *sql.DB, table string, v int64, log *zap.Logger) {
	_, _ = db.Exec(fmt.Sprintf("DELETE FROM %s WHERE version > ?", table), v)
	log.Info("forced", zap.Int64("version", v))
}

// execSQLScript 单 file 多 statement 分号切.
// 注: 不处理 DELIMITER (生产建议 sql 文件用单 ; 不变 DELIMITER).
func execSQLScript(db *sql.DB, script string) error {
	stmts := strings.Split(script, ";")
	for _, s := range stmts {
		s = strings.TrimSpace(s)
		if s == "" || strings.HasPrefix(s, "--") {
			continue
		}
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("exec %q: %w", truncate(s, 120), err)
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// 兼容旧 Go 1.20-
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

var _ = errors.New
