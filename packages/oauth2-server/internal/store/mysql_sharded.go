// mysql_sharded.go — 分库分表版 MySQL store.
//
// 跟 mysql.go (单库版) 不同:
//   - 持有 map[int]*sql.DB — 每个 db_index 一个独立连接池
//   - 所有 query 先经 router 选 (db, table), 再拼 SQL
//   - 跨 shard 操作 (List / GCRevoked) 用 fan-out + merge
//
// DSN 配置 (env):
//   OAUTH2_DB_DSN_PREFIX = "user:pwd@tcp(mysql-cluster:3306)/oauth2_db_"
//   → 自动连接 oauth2_db_0..9 (拼 db_index 后缀)
//
// 也可以全部库同 host (单实例多 DB), 或不同 host (跨实例真分片) — 看 DSN 模板。

package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"reconcile-system/packages/oauth2-server/internal/domain"
	"reconcile-system/packages/oauth2-server/internal/sharding"
)

// ShardedMySQLStore 分库分表实现.
type ShardedMySQLStore struct {
	router *sharding.Router
	dbs    map[int]*sql.DB // db_index → connection pool
}

// NewShardedMySQLStore 用 10 个独立连接池初始化.
//   dsnFor: 给 db_index 返回完整 DSN (e.g. "user:pass@tcp(host:3306)/oauth2_db_3?charset=utf8mb4&parseTime=true")
//
// 调用方控:
//   - 单实例多 DB: 10 个 DSN 只换 db_name 部分
//   - 跨实例真分片: 每个 DSN 换 host
func NewShardedMySQLStore(dsnFor func(dbIndex int) string) (*ShardedMySQLStore, error) {
	router := sharding.NewRouter()
	dbs := make(map[int]*sql.DB, router.DBCount())
	for i := 0; i < router.DBCount(); i++ {
		dsn := dsnFor(i)
		db, err := sql.Open("mysql", dsn)
		if err != nil {
			return nil, fmt.Errorf("open db_%d: %w", i, err)
		}
		db.SetMaxOpenConns(20)
		db.SetMaxIdleConns(5)
		db.SetConnMaxLifetime(30 * time.Minute)
		if err := db.Ping(); err != nil {
			return nil, fmt.Errorf("ping db_%d: %w", i, err)
		}
		dbs[i] = db
	}
	return &ShardedMySQLStore{router: router, dbs: dbs}, nil
}

// Close 关全部连接池.
func (s *ShardedMySQLStore) Close() error {
	for _, db := range s.dbs {
		_ = db.Close()
	}
	return nil
}

// dbForKey 拿 client_id/jti/actor 对应的 (db, tableName).
func (s *ShardedMySQLStore) dbForClient(clientID string) (*sql.DB, string) {
	dbName, tableName := s.router.ClientsTable(clientID)
	dbIdx, _ := strconv.Atoi(dbName[len("oauth2_db_"):])
	return s.dbs[dbIdx], tableName
}
func (s *ShardedMySQLStore) dbForJTI(jti string) (*sql.DB, string) {
	dbName, tableName := s.router.RevokedTokensTable(jti)
	dbIdx, _ := strconv.Atoi(dbName[len("oauth2_db_"):])
	return s.dbs[dbIdx], tableName
}

// ─── Client 接口 ──────────────────────────────────────────────────────

func (s *ShardedMySQLStore) GetClient(clientID string) (*domain.Client, error) {
	db, table := s.dbForClient(clientID)
	row := db.QueryRow(fmt.Sprintf(`
		SELECT id, client_id, secret_hash, secret_last4, name,
		       owner_type, owner_id, allowed_scopes, COALESCE(allowed_ips,''),
		       rate_limit_rps, status, expires_at, last_used_at,
		       created_at, updated_at
		  FROM %s WHERE client_id=?`, table), clientID)
	c := &domain.Client{}
	var expiresAt, lastUsedAt sql.NullTime
	var ownerType string
	if err := row.Scan(&c.ID, &c.ClientID, &c.SecretHash, &c.SecretLast4, &c.Name,
		&ownerType, &c.OwnerID, &c.AllowedScopes, &c.AllowedIPs,
		&c.RateLimitRPS, &c.Status, &expiresAt, &lastUsedAt,
		&c.CreatedAt, &c.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrClientNotFound
		}
		return nil, err
	}
	c.OwnerType = domain.ClientType(ownerType)
	if expiresAt.Valid {
		c.ExpiresAt = &expiresAt.Time
	}
	if lastUsedAt.Valid {
		c.LastUsedAt = &lastUsedAt.Time
	}
	return c, nil
}

func (s *ShardedMySQLStore) PutClient(c *domain.Client) error {
	db, table := s.dbForClient(c.ClientID)

	// 落 shard 主表
	q := fmt.Sprintf(`
		INSERT INTO %s
		   (client_id, secret_hash, secret_last4, name, owner_type, owner_id,
		    allowed_scopes, allowed_ips, rate_limit_rps, status, expires_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON DUPLICATE KEY UPDATE
		   secret_hash=VALUES(secret_hash), secret_last4=VALUES(secret_last4),
		   name=VALUES(name), allowed_scopes=VALUES(allowed_scopes),
		   allowed_ips=VALUES(allowed_ips), rate_limit_rps=VALUES(rate_limit_rps),
		   status=VALUES(status), expires_at=VALUES(expires_at)`, table)
	if _, err := db.Exec(q, c.ClientID, c.SecretHash, c.SecretLast4, c.Name,
		string(c.OwnerType), c.OwnerID, c.AllowedScopes, c.AllowedIPs,
		c.RateLimitRPS, c.Status, c.ExpiresAt); err != nil {
		return err
	}

	// 同步落全局索引 (放 oauth2_db_0.client_index)
	dbIdx, tblIdx := s.router.RouteByKey(c.ClientID)
	_, _ = s.dbs[0].Exec(`
		INSERT IGNORE INTO oauth2_db_0.client_index (client_id, db_index, table_index)
		VALUES (?,?,?)`, c.ClientID, dbIdx, tblIdx)
	return nil
}

// ListClients 跨 shard fan-out + 合并 (ops 后台用; 量大要分页, 这里简化全拉前 1000).
func (s *ShardedMySQLStore) ListClients() ([]*domain.Client, error) {
	var (
		mu      sync.Mutex
		all     = []*domain.Client{}
		wg      sync.WaitGroup
		errCh   = make(chan error, s.router.DBCount())
	)
	for dbIdx, db := range s.dbs {
		wg.Add(1)
		go func(dbIdx int, db *sql.DB) {
			defer wg.Done()
			// 该 db 上的 10 张分片表
			for tblIdx := dbIdx * s.router.TablePerDB(); tblIdx < (dbIdx+1)*s.router.TablePerDB(); tblIdx++ {
				table := fmt.Sprintf("clients_%02d", tblIdx)
				rows, err := db.Query(fmt.Sprintf(`
					SELECT id, client_id, secret_hash, secret_last4, name,
					       owner_type, owner_id, allowed_scopes, COALESCE(allowed_ips,''),
					       rate_limit_rps, status, expires_at, last_used_at,
					       created_at, updated_at
					FROM %s ORDER BY created_at DESC LIMIT 100`, table))
				if err != nil {
					errCh <- err
					return
				}
				for rows.Next() {
					c := &domain.Client{}
					var expiresAt, lastUsedAt sql.NullTime
					var ownerType string
					if err := rows.Scan(&c.ID, &c.ClientID, &c.SecretHash, &c.SecretLast4, &c.Name,
						&ownerType, &c.OwnerID, &c.AllowedScopes, &c.AllowedIPs,
						&c.RateLimitRPS, &c.Status, &expiresAt, &lastUsedAt,
						&c.CreatedAt, &c.UpdatedAt); err != nil {
						continue
					}
					c.OwnerType = domain.ClientType(ownerType)
					if expiresAt.Valid {
						c.ExpiresAt = &expiresAt.Time
					}
					if lastUsedAt.Valid {
						c.LastUsedAt = &lastUsedAt.Time
					}
					mu.Lock()
					all = append(all, c)
					mu.Unlock()
				}
				_ = rows.Close()
			}
		}(dbIdx, db)
	}
	wg.Wait()
	close(errCh)
	if e, ok := <-errCh; ok && e != nil {
		return all, e // 返已收集到的 + 第一个错
	}
	return all, nil
}

func (s *ShardedMySQLStore) UpdateLastUsed(clientID string, t time.Time) error {
	db, table := s.dbForClient(clientID)
	_, err := db.Exec(fmt.Sprintf(
		`UPDATE %s SET last_used_at=? WHERE client_id=?`, table), t, clientID)
	return err
}

// ─── Revocation 接口 ────────────────────────────────────────────────

func (s *ShardedMySQLStore) Revoke(jti string, exp time.Time) error {
	db, table := s.dbForJTI(jti)
	_, err := db.Exec(fmt.Sprintf(
		`INSERT IGNORE INTO %s (jti, expires_at) VALUES (?, ?)`, table), jti, exp)
	return err
}

func (s *ShardedMySQLStore) IsRevoked(jti string) bool {
	db, table := s.dbForJTI(jti)
	var n int
	err := db.QueryRow(fmt.Sprintf(
		`SELECT COUNT(1) FROM %s WHERE jti=? AND expires_at > NOW()`, table), jti).Scan(&n)
	return err == nil && n > 0
}

// GCRevoked fan-out 清过期 revocation (每分片独立 DELETE; 总 100 张表).
func (s *ShardedMySQLStore) GCRevoked() (int64, error) {
	var (
		mu      sync.Mutex
		total   int64
		wg      sync.WaitGroup
	)
	for dbIdx, db := range s.dbs {
		wg.Add(1)
		go func(dbIdx int, db *sql.DB) {
			defer wg.Done()
			for tblIdx := dbIdx * s.router.TablePerDB(); tblIdx < (dbIdx+1)*s.router.TablePerDB(); tblIdx++ {
				table := fmt.Sprintf("revoked_tokens_%02d", tblIdx)
				r, err := db.Exec(fmt.Sprintf(
					`DELETE FROM %s WHERE expires_at <= NOW() LIMIT 10000`, table))
				if err != nil {
					continue
				}
				if n, err := r.RowsAffected(); err == nil {
					mu.Lock()
					total += n
					mu.Unlock()
				}
			}
		}(dbIdx, db)
	}
	wg.Wait()
	return total, nil
}
