// mysql.go — MySQL 持久化实现 (HA prod 必须)。
//
// Schema 在 database/init/01_schema.sql。
//
// 用法:
//   db, _ := sql.Open("mysql", dsn)
//   s := store.NewMySQLStore(db)
//   adminhttp.NewServer(log, ks, s, ...)
//
// 比 MemoryStore 多支持:
//   - 多实例共享 (HA)
//   - revocation 跨重启持久
//   - 审计日志
//
// 注: 此文件不直接 import driver, 由 cmd/server main 处用 _ "github.com/go-sql-driver/mysql".

package store

import (
	"database/sql"
	"errors"
	"time"

	"reconcile-system/packages/oauth2-server/internal/domain"
)

// MySQLStore SQL 持久化。
type MySQLStore struct {
	db *sql.DB
}

// NewMySQLStore 构造。
func NewMySQLStore(db *sql.DB) *MySQLStore {
	return &MySQLStore{db: db}
}

// GetClient 按 client_id 查。
func (s *MySQLStore) GetClient(clientID string) (*domain.Client, error) {
	row := s.db.QueryRow(`
		SELECT id, client_id, secret_hash, secret_last4, name,
		       owner_type, owner_id, allowed_scopes, COALESCE(allowed_ips,''),
		       rate_limit_rps, status, expires_at, last_used_at,
		       created_at, updated_at
		  FROM clients WHERE client_id=?`, clientID)
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

// PutClient INSERT or UPDATE。
func (s *MySQLStore) PutClient(c *domain.Client) error {
	_, err := s.db.Exec(`
		INSERT INTO clients
		   (client_id, secret_hash, secret_last4, name, owner_type, owner_id,
		    allowed_scopes, allowed_ips, rate_limit_rps, status, expires_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON DUPLICATE KEY UPDATE
		   secret_hash=VALUES(secret_hash),
		   secret_last4=VALUES(secret_last4),
		   name=VALUES(name),
		   owner_type=VALUES(owner_type),
		   owner_id=VALUES(owner_id),
		   allowed_scopes=VALUES(allowed_scopes),
		   allowed_ips=VALUES(allowed_ips),
		   rate_limit_rps=VALUES(rate_limit_rps),
		   status=VALUES(status),
		   expires_at=VALUES(expires_at)`,
		c.ClientID, c.SecretHash, c.SecretLast4, c.Name,
		string(c.OwnerType), c.OwnerID,
		c.AllowedScopes, c.AllowedIPs,
		c.RateLimitRPS, c.Status, c.ExpiresAt)
	return err
}

// ListClients 列所有 (ops 后台用，量大要分页 — 这里简化全拉)。
func (s *MySQLStore) ListClients() ([]*domain.Client, error) {
	rows, err := s.db.Query(`
		SELECT id, client_id, secret_hash, secret_last4, name,
		       owner_type, owner_id, allowed_scopes, COALESCE(allowed_ips,''),
		       rate_limit_rps, status, expires_at, last_used_at,
		       created_at, updated_at
		  FROM clients ORDER BY created_at DESC LIMIT 1000`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*domain.Client{}
	for rows.Next() {
		c := &domain.Client{}
		var expiresAt, lastUsedAt sql.NullTime
		var ownerType string
		if err := rows.Scan(&c.ID, &c.ClientID, &c.SecretHash, &c.SecretLast4, &c.Name,
			&ownerType, &c.OwnerID, &c.AllowedScopes, &c.AllowedIPs,
			&c.RateLimitRPS, &c.Status, &expiresAt, &lastUsedAt,
			&c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		c.OwnerType = domain.ClientType(ownerType)
		if expiresAt.Valid {
			c.ExpiresAt = &expiresAt.Time
		}
		if lastUsedAt.Valid {
			c.LastUsedAt = &lastUsedAt.Time
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpdateLastUsed best-effort。
func (s *MySQLStore) UpdateLastUsed(clientID string, t time.Time) error {
	_, err := s.db.Exec(`UPDATE clients SET last_used_at=? WHERE client_id=?`, t, clientID)
	return err
}

// Revoke 写黑名单。
func (s *MySQLStore) Revoke(jti string, exp time.Time) error {
	_, err := s.db.Exec(`
		INSERT IGNORE INTO revoked_tokens (jti, expires_at)
		VALUES (?, ?)`, jti, exp)
	return err
}

// IsRevoked 查黑名单。
func (s *MySQLStore) IsRevoked(jti string) bool {
	var n int
	err := s.db.QueryRow(`
		SELECT COUNT(1) FROM revoked_tokens
		 WHERE jti=? AND expires_at > NOW()`, jti).Scan(&n)
	return err == nil && n > 0
}

// GCRevoked 清过期 revocation (cron 调)。
func (s *MySQLStore) GCRevoked() (int64, error) {
	r, err := s.db.Exec(`DELETE FROM revoked_tokens WHERE expires_at <= NOW()`)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}
