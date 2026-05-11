// oauth2-server — OAuth 2.0 client_credentials JWT 颁发服务。
//
// 启动:
//   OAUTH2_ADDR=:8087 \
//   OAUTH2_ISSUER=https://oauth.payment.local \
//   OAUTH2_AUDIENCE=payment-api \
//   OAUTH2_ADMIN_TOKEN=$(head -c 32 /dev/urandom | base64) \
//   OAUTH2_KEY_PATH=./oauth-rsa.pem \
//   go run ./cmd/server
//
// 端口/路由:
//   POST /oauth2/token             — 颁发 access_token
//   POST /oauth2/introspect        — 验签 + 解析
//   POST /oauth2/revoke            — 撤销 (jti 黑名单)
//   GET  /.well-known/jwks.json    — 公钥 (resource server 拉去缓存)
//   GET  /.well-known/openid-configuration — Discovery
//   /admin/* — 内网管理 (X-Admin-Token)
//
// 资源服务集成:
//   1. 启动时 GET /.well-known/jwks.json 拉公钥 (本地缓存 1h)
//   2. 每个请求 Authorization: Bearer <token>
//   3. 本地 RS256 验签 + 检 exp/iss/aud
//   4. 用 claims.scope 做 RBAC

package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"reconcile-system/packages/oauth2-server/internal/adminhttp"
	"reconcile-system/packages/oauth2-server/internal/domain"
	"reconcile-system/packages/oauth2-server/internal/jwks"
	"reconcile-system/packages/oauth2-server/internal/store"

	_ "github.com/go-sql-driver/mysql"
	"go.uber.org/zap"
)

func main() {
	log, _ := zap.NewProduction()
	defer log.Sync()

	addr := envOr("OAUTH2_ADDR", ":8087")
	issuer := envOr("OAUTH2_ISSUER", "https://oauth.payment.local")
	audience := envOr("OAUTH2_AUDIENCE", "payment-api")
	keyPath := envOr("OAUTH2_KEY_PATH", "./oauth-rsa.pem")
	adminToken := envOr("OAUTH2_ADMIN_TOKEN", "")
	ttlSec, _ := strconv.Atoi(envOr("OAUTH2_TOKEN_TTL_SEC", "3600"))
	ttl := time.Duration(ttlSec) * time.Second

	if adminToken == "" {
		log.Warn("OAUTH2_ADMIN_TOKEN empty — admin endpoints DISABLED. set one for client provisioning")
	}

	ks := jwks.NewKeyStore(issuer, audience)
	if err := ks.LoadOrGenerate(keyPath); err != nil {
		log.Fatal("load/generate RSA key", zap.Error(err))
	}
	log.Info("RSA key ready",
		zap.String("kid", ks.Active().KID),
		zap.String("path", keyPath))

	var s store.Store
	var mem *store.MemoryStore
	if dsn := os.Getenv("OAUTH2_MYSQL_DSN"); dsn != "" {
		db, err := sql.Open("mysql", dsn)
		if err != nil {
			log.Fatal("open mysql", zap.Error(err))
		}
		db.SetMaxOpenConns(20)
		db.SetMaxIdleConns(5)
		db.SetConnMaxLifetime(30 * time.Minute)
		if err := db.Ping(); err != nil {
			log.Fatal("mysql ping", zap.Error(err))
		}
		s = store.NewMySQLStore(db)
		log.Info("using MySQL store", zap.String("driver", "mysql"))
	} else {
		mem = store.NewMemoryStore()
		seedDevClients(log, mem)
		s = mem
		log.Warn("using in-memory store (not for HA prod). set OAUTH2_MYSQL_DSN to enable MySQL")
	}

	srv := adminhttp.NewServer(log, ks, s, issuer, audience, adminToken, ttl)

	mux := http.NewServeMux()
	srv.Register(mux)

	h := withAccessLog(log, mux)
	server := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// 后台任务: revocation GC + key purge
	stop := make(chan struct{})
	go bgTasks(log, s, mem, ks, stop)

	go func() {
		log.Info("oauth2-server listening",
			zap.String("addr", addr),
			zap.String("issuer", issuer),
			zap.String("aud", audience),
			zap.Duration("token_ttl", ttl))
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal("server failed", zap.Error(err))
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Info("shutting down")
	close(stop)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
}

// bgTasks 周期清 revocation + 老 retired key。
func bgTasks(log *zap.Logger, s store.Store, mem *store.MemoryStore, ks *jwks.KeyStore, stop <-chan struct{}) {
	t := time.NewTicker(15 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			gcRev := 0
			if mem != nil {
				gcRev = mem.GCRevoked()
			} else if mysql, ok := s.(*store.MySQLStore); ok {
				if n, err := mysql.GCRevoked(); err == nil {
					gcRev = int(n)
				}
			}
			gcKey := ks.PurgeRetired()
			if gcRev > 0 || gcKey > 0 {
				log.Info("bg gc", zap.Int("revoked", gcRev), zap.Int("keys", gcKey))
			}
		}
	}
}

// seedDevClients 开发环境种 3 个客户端 (生产环境删此函数)。
// 仅当 OAUTH2_DEV_SEED=1 时启用。
func seedDevClients(log *zap.Logger, m *store.MemoryStore) {
	if os.Getenv("OAUTH2_DEV_SEED") != "1" {
		return
	}
	seeds := []struct {
		clientID  string
		secret    string
		name      string
		ownerType string
		ownerID   string
		scopes    string
	}{
		{"mer_demo_merchant_01", "dev_secret_merchant_001",
			"Demo Merchant 01", "merchant", "merchant_001",
			"charge:write charge:read refund:write refund:read"},
		{"svc_payment_gateway", "dev_secret_payment_gateway",
			"Internal: payment-gateway", "service", "payment-gateway",
			"charge:write charge:read refund:write refund:read dispute:read"},
		{"svc_billing_system", "dev_secret_billing_system",
			"Internal: billing-system", "service", "billing-system",
			"charge:read refund:read"},
	}
	for _, sd := range seeds {
		hash, _ := store.HashSecret(sd.secret)
		c := &domain.Client{
			ClientID:      sd.clientID,
			SecretHash:    hash,
			SecretLast4:   sd.secret[len(sd.secret)-4:],
			Name:          sd.name,
			OwnerType:     domain.ClientType(sd.ownerType),
			OwnerID:       sd.ownerID,
			AllowedScopes: sd.scopes,
			Status:        "active",
		}
		_ = m.PutClient(c)
		log.Warn("DEV SEED: created client",
			zap.String("client_id", sd.clientID),
			zap.String("client_secret", sd.secret))
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// withAccessLog 简易访问日志中间件。
func withAccessLog(log *zap.Logger, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := &statusRecorder{ResponseWriter: w, status: 200}
		h.ServeHTTP(ww, r)
		log.Info("http",
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path),
			zap.Int("status", ww.status),
			zap.Duration("dur", time.Since(start)),
			zap.String("ip", r.RemoteAddr))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}
