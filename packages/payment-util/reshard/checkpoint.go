// checkpoint.go — CheckpointStore 实现：MySQL + 内存版。
//
// 表 schema:
//
//	CREATE TABLE reshard_checkpoint (
//	  table_name  VARCHAR(64)  NOT NULL,
//	  db_idx      INT          NOT NULL,
//	  tbl_idx     INT          NOT NULL,
//	  last_pk     BIGINT       NOT NULL,
//	  total_rows  BIGINT       NOT NULL DEFAULT 0,
//	  updated_at  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
//	  PRIMARY KEY (table_name, db_idx, tbl_idx)
//	);
//
// 该表放在 meta 库（不分片），所有 service 公用。

package reshard

import (
	"context"
	"database/sql"
	"errors"
	"sync"
)

// MySQLCheckpoint 用 *sql.DB 实现 CheckpointStore。
type MySQLCheckpoint struct {
	db        *sql.DB
	tableName string // 默认 "reshard_checkpoint"
}

// NewMySQLCheckpoint 默认表名 reshard_checkpoint。
func NewMySQLCheckpoint(db *sql.DB) *MySQLCheckpoint {
	return &MySQLCheckpoint{db: db, tableName: "reshard_checkpoint"}
}

// WithTable 自定义表名（多迁移共存场景）。
func (c *MySQLCheckpoint) WithTable(name string) *MySQLCheckpoint {
	c.tableName = name
	return c
}

// Save UPSERT。Atomic：靠 PK 冲突自动回 UPDATE。
func (c *MySQLCheckpoint) Save(ctx context.Context, tableName string, shard [2]int, lastPK, total int64) error {
	q := "INSERT INTO " + c.tableName + " (table_name, db_idx, tbl_idx, last_pk, total_rows) " +
		"VALUES (?, ?, ?, ?, ?) " +
		"ON DUPLICATE KEY UPDATE last_pk = GREATEST(last_pk, VALUES(last_pk)), " +
		"  total_rows = GREATEST(total_rows, VALUES(total_rows))"
	_, err := c.db.ExecContext(ctx, q, tableName, shard[0], shard[1], lastPK, total)
	return err
}

// Load 不存在返 (0, 0, nil) — 视为从头开始。
func (c *MySQLCheckpoint) Load(ctx context.Context, tableName string, shard [2]int) (int64, int64, error) {
	q := "SELECT last_pk, total_rows FROM " + c.tableName +
		" WHERE table_name = ? AND db_idx = ? AND tbl_idx = ?"
	var lastPK, total int64
	err := c.db.QueryRowContext(ctx, q, tableName, shard[0], shard[1]).Scan(&lastPK, &total)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	return lastPK, total, err
}

// MemCheckpoint 内存版（单元测试 / dev / dry-run 用）。
type MemCheckpoint struct {
	mu   sync.Mutex
	data map[string]map[[2]int]checkpointEntry
}

type checkpointEntry struct {
	lastPK int64
	total  int64
}

// NewMemCheckpoint 构造。
func NewMemCheckpoint() *MemCheckpoint {
	return &MemCheckpoint{
		data: make(map[string]map[[2]int]checkpointEntry),
	}
}

func (c *MemCheckpoint) Save(_ context.Context, tableName string, shard [2]int, lastPK, total int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	tbl, ok := c.data[tableName]
	if !ok {
		tbl = make(map[[2]int]checkpointEntry)
		c.data[tableName] = tbl
	}
	cur := tbl[shard]
	if lastPK > cur.lastPK {
		cur.lastPK = lastPK
	}
	if total > cur.total {
		cur.total = total
	}
	tbl[shard] = cur
	return nil
}

func (c *MemCheckpoint) Load(_ context.Context, tableName string, shard [2]int) (int64, int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	tbl, ok := c.data[tableName]
	if !ok {
		return 0, 0, nil
	}
	e := tbl[shard]
	return e.lastPK, e.total, nil
}
