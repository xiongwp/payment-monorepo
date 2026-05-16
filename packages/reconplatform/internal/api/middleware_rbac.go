// middleware_rbac.go — SEC-1: 基于 header 的简易 RBAC + 审计日志.
//
// 角色 (从 HTTP header X-User-Role 取, 由上游 auth gateway 注入):
//   - viewer:  只读. GET 所有端点 OK; 写操作 → 403.
//   - editor:  读 + 写规则 (POST/PUT/PATCH /api/v1/scripts), 不能 DELETE.
//   - admin:   全部 + RBAC 配置.
//   - <empty>: 视为 viewer (生产环境应在 ingress 强制注入此 header).
//
// 审计日志:
//   每条写操作 (POST/PUT/PATCH/DELETE) 落 Redis LIST recon:audit:log (LPUSH + LTRIM 1000),
//   含 actor / method / path / status / timestamp / body (限 4KB).
//
// 不上 sqlite / Postgres 表 — 简单的 Redis 列表足够,运营 admin /admin/audit 看最近 N 条;
// 长期 retention 可推 Kafka audit topic (下一阶段).
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// Role 角色枚举.
type Role string

const (
	RoleViewer Role = "viewer"
	RoleEditor Role = "editor"
	RoleAdmin  Role = "admin"
)

// roleRank 用于比较: admin > editor > viewer.
func roleRank(r Role) int {
	switch r {
	case RoleAdmin:
		return 3
	case RoleEditor:
		return 2
	case RoleViewer:
		return 1
	}
	return 0
}

// rolesEnabled 默认开 RBAC. 通过 ENV RECON_RBAC=off 关闭 (单测 / dev).
var rolesEnabled atomic.Bool

// SetRBACEnabled 开/关 RBAC 检查. 启动期由 main 设定.
func SetRBACEnabled(on bool) { rolesEnabled.Store(on) }

// WithRBAC 把 handler 包装成 RBAC + audit 中间件.
//
// 用法 (server.Mount):
//   mux.HandleFunc("/api/v1/scripts/", api.WithRBAC(rdb, s.scriptsByID, logger))
//
// 路径白名单 (无需鉴权) 由 isPublic() 判定 — /admin HTML + /healthz + 静态.
func WithRBAC(rdb redis.UniversalClient, next http.HandlerFunc, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 公开端点直接放行 (HTML 页面靠 cookie/SSO 拦, 不在本层重复).
		if isPublic(r.URL.Path) {
			next(w, r)
			return
		}
		role := Role(strings.ToLower(r.Header.Get("X-User-Role")))
		if role == "" {
			role = RoleViewer
		}
		if !roleAllowed(role, r.Method) {
			http.Error(w, "forbidden: role "+string(role)+" cannot "+r.Method+" "+r.URL.Path, http.StatusForbidden)
			return
		}

		// 写操作: 拷 body 用于 audit, 然后还原
		var bodyDump []byte
		isWrite := r.Method == http.MethodPost ||
			r.Method == http.MethodPut ||
			r.Method == http.MethodPatch ||
			r.Method == http.MethodDelete
		if isWrite && r.Body != nil {
			bodyDump, _ = io.ReadAll(io.LimitReader(r.Body, 4096))
			r.Body = io.NopCloser(bytes.NewReader(bodyDump))
		}

		// Wrap response writer 捕获 status
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next(rec, r)

		// 写操作落审计 (best-effort, 不阻塞)
		if isWrite && rdb != nil {
			actor := r.Header.Get("X-User-Email")
			if actor == "" {
				actor = r.Header.Get("X-User-Id")
			}
			if actor == "" {
				actor = "anonymous"
			}
			go writeAudit(rdb, logger, auditEntry{
				Actor:     actor,
				Role:      string(role),
				Method:    r.Method,
				Path:      r.URL.Path,
				Status:    rec.status,
				Timestamp: time.Now().UTC(),
				BodyLen:   len(bodyDump),
				Body:      truncBytes(bodyDump, 1024),
			})
		}
	}
}

// isPublic 不走 RBAC / audit 的路径 (HTML 页面, 健康检查).
func isPublic(p string) bool {
	switch {
	case p == "/healthz", p == "/readyz", p == "/metrics":
		return true
	case strings.HasPrefix(p, "/admin/"), p == "/admin":
		return true
	case strings.HasPrefix(p, "/static/"):
		return true
	}
	return false
}

// roleAllowed 当前角色能否做该 method.
//
// GET / HEAD 全员可;
// POST / PUT / PATCH: editor / admin;
// DELETE: 仅 admin.
func roleAllowed(role Role, method string) bool {
	if !rolesEnabled.Load() {
		return true
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return roleRank(role) >= roleRank(RoleViewer)
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		return roleRank(role) >= roleRank(RoleEditor)
	case http.MethodDelete:
		return roleRank(role) >= roleRank(RoleAdmin)
	}
	return false
}

// statusRecorder 捕获 response status code.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.status = code
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wrote {
		s.status = 200
		s.wrote = true
	}
	return s.ResponseWriter.Write(b)
}

// auditEntry 单条审计.
type auditEntry struct {
	Actor     string    `json:"actor"`
	Role      string    `json:"role"`
	Method    string    `json:"method"`
	Path      string    `json:"path"`
	Status    int       `json:"status"`
	Timestamp time.Time `json:"ts"`
	BodyLen   int       `json:"body_len"`
	Body      string    `json:"body,omitempty"` // 截断后的 body, JSON 字符串
}

// auditRedisKey LIST key.
const auditRedisKey = "recon:audit:log"

// auditMaxKeep 最近 N 条保留 (LTRIM).
const auditMaxKeep = 1000

// writeAudit LPUSH + LTRIM 一气呵成 (失败仅 warn).
func writeAudit(rdb redis.UniversalClient, logger *zap.Logger, e auditEntry) {
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pipe := rdb.Pipeline()
	pipe.LPush(ctx, auditRedisKey, b)
	pipe.LTrim(ctx, auditRedisKey, 0, auditMaxKeep-1)
	if _, err := pipe.Exec(ctx); err != nil {
		if logger != nil {
			logger.Warn("audit write failed", zap.Error(err))
		}
	}
}

func truncBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "...(truncated)"
}

// auditList GET /api/v1/admin/audit — 拉最近 N 条审计.
//
// admin 角色才能看 (敏感).
func (s *Server) auditList(w http.ResponseWriter, r *http.Request) {
	if s.rdb == nil {
		http.Error(w, "redis not configured", http.StatusServiceUnavailable)
		return
	}
	role := Role(strings.ToLower(r.Header.Get("X-User-Role")))
	if rolesEnabled.Load() && role != RoleAdmin {
		http.Error(w, "forbidden: admin only", http.StatusForbidden)
		return
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		_, _ = jsonScanInt(v, &limit)
	}
	if limit <= 0 || limit > auditMaxKeep {
		limit = 100
	}
	raw, err := s.rdb.LRange(r.Context(), auditRedisKey, 0, int64(limit-1)).Result()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]auditEntry, 0, len(raw))
	for _, s := range raw {
		var e auditEntry
		if json.Unmarshal([]byte(s), &e) == nil {
			out = append(out, e)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// jsonScanInt 简易 string → int (不引入 strconv 在本文件).
func jsonScanInt(s string, out *int) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	*out = n
	return n, nil
}
