// oauth2-server — OAuth 2.0 client_credentials JWT 颁发服务 — uber/fx 装配.
//
// 端口/路由:
//
//	POST /oauth2/token             — 颁发 access_token
//	POST /oauth2/introspect        — 验签 + 解析
//	POST /oauth2/revoke            — 撤销 (jti 黑名单)
//	GET  /.well-known/jwks.json    — 公钥
//	GET  /.well-known/openid-configuration — Discovery
//	/admin/* — 内网管理 (X-Admin-Token)
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
	"go.uber.org/zap"

	"reconcile-system/packages/oauth2-server/internal/adminhttp"
	"reconcile-system/packages/oauth2-server/internal/domain"
	"reconcile-system/packages/oauth2-server/internal/jwks"
	"reconcile-system/packages/oauth2-server/internal/metrics"
	"reconcile-system/packages/oauth2-server/internal/store"

	_ "github.com/go-sql-driver/mysql"
)

type oauthConfig struct {
	Addr       string
	Issuer     string
	Audience   string
	KeyPath    string
	AdminToken string
	TTL        time.Duration
	DSN        string
}

func main() {
	fx.New(
		fx.Provide(
			newLogger,
			newOAuthConfig,
			newKeyStore,
			newStore,
			newAdminServer,
			newHTTPServer,
		),
		fx.Invoke(
			seedDevClientsIfRequested,
			startHTTPServer,
			startBGTasks,
		),
		fx.WithLogger(func(log *zap.Logger) fxevent.Logger {
			return &fxevent.ZapLogger{Logger: log.Named("fx")}
		}),
	).Run()
}

func newLogger(lc fx.Lifecycle) (*zap.Logger, error) {
	logger, err := zap.NewProduction()
	if err != nil {
		return nil, fmt.Errorf("zap build: %w", err)
	}
	lc.Append(fx.Hook{OnStop: func(_ context.Context) error { _ = logger.Sync(); return nil }})
	return logger, nil
}

func newOAuthConfig(log *zap.Logger) *oauthConfig {
	ttlSec, _ := strconv.Atoi(envOr("OAUTH2_TOKEN_TTL_SEC", "3600"))
	cfg := &oauthConfig{
		Addr:       envOr("OAUTH2_ADDR", ":8087"),
		Issuer:     envOr("OAUTH2_ISSUER", "https://oauth.payment.local"),
		Audience:   envOr("OAUTH2_AUDIENCE", "payment-api"),
		KeyPath:    envOr("OAUTH2_KEY_PATH", "./oauth-rsa.pem"),
		AdminToken: envOr("OAUTH2_ADMIN_TOKEN", ""),
		TTL:        time.Duration(ttlSec) * time.Second,
		DSN:        os.Getenv("OAUTH2_MYSQL_DSN"),
	}
	if cfg.AdminToken == "" {
		log.Warn("OAUTH2_ADMIN_TOKEN empty — admin endpoints DISABLED")
	}
	return cfg
}

func newKeyStore(cfg *oauthConfig, log *zap.Logger) (*jwks.KeyStore, error) {
	ks := jwks.NewKeyStore(cfg.Issuer, cfg.Audience)
	if err := ks.LoadOrGenerate(cfg.KeyPath); err != nil {
		log.Error("load/generate RSA key failed", zap.String("path", cfg.KeyPath), zap.Error(err))
		return nil, fmt.Errorf("ks.LoadOrGenerate: %w", err)
	}
	log.Info("RSA key ready", zap.String("kid", ks.Active().KID), zap.String("path", cfg.KeyPath))
	return ks, nil
}

func newStore(cfg *oauthConfig, lc fx.Lifecycle, log *zap.Logger) (store.Store, error) {
	if cfg.DSN == "" {
		mem := store.NewMemoryStore()
		log.Warn("using in-memory store (not for HA prod); set OAUTH2_MYSQL_DSN to enable MySQL")
		return mem, nil
	}
	db, err := sql.Open("mysql", cfg.DSN)
	if err != nil {
		log.Error("open mysql failed", zap.Error(err))
		return nil, fmt.Errorf("sql.Open: %w", err)
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err := db.Ping(); err != nil {
		log.Error("mysql ping failed", zap.Error(err))
		return nil, fmt.Errorf("ping: %w", err)
	}
	log.Info("using MySQL store")
	lc.Append(fx.Hook{OnStop: func(_ context.Context) error { return db.Close() }})
	return store.NewMySQLStore(db), nil
}

func newAdminServer(cfg *oauthConfig, ks *jwks.KeyStore, s store.Store, log *zap.Logger) *adminhttp.Server {
	return adminhttp.NewServer(log, ks, s, cfg.Issuer, cfg.Audience, cfg.AdminToken, cfg.TTL)
}

func newHTTPServer(cfg *oauthConfig, srv *adminhttp.Server, log *zap.Logger) *http.Server {
	mux := http.NewServeMux()
	srv.Register(mux)
	mux.Handle("/metrics", promhttp.Handler())
	return &http.Server{
		Addr:              cfg.Addr,
		Handler:           withAccessLog(log, mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

func startHTTPServer(lc fx.Lifecycle, srv *http.Server, cfg *oauthConfig, log *zap.Logger) {
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			log.Info("oauth2-server listening",
				zap.String("addr", srv.Addr),
				zap.String("issuer", cfg.Issuer),
				zap.String("aud", cfg.Audience),
				zap.Duration("token_ttl", cfg.TTL))
			go func() {
				if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
					log.Error("server failed", zap.Error(err))
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			shutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			return srv.Shutdown(shutCtx)
		},
	})
}

// startBGTasks 周期清 revocation + 老 retired key.
func startBGTasks(lc fx.Lifecycle, s store.Store, ks *jwks.KeyStore, log *zap.Logger) {
	stop := make(chan struct{})
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			go bgTasks(log, s, ks, stop)
			return nil
		},
		OnStop: func(_ context.Context) error { close(stop); return nil },
	})
}

func bgTasks(log *zap.Logger, s store.Store, ks *jwks.KeyStore, stop <-chan struct{}) {
	t := time.NewTicker(15 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			gcRev := 0
			if mem, ok := s.(*store.MemoryStore); ok {
				gcRev = mem.GCRevoked()
			} else if mysql, ok := s.(*store.MySQLStore); ok {
				if n, err := mysql.GCRevoked(); err == nil {
					gcRev = int(n)
				} else {
					log.Warn("mysql GCRevoked failed", zap.Error(err))
				}
			}
			gcKey := ks.PurgeRetired()
			if gcRev > 0 || gcKey > 0 {
				log.Info("bg gc", zap.Int("revoked", gcRev), zap.Int("keys", gcKey))
			}
			if kp := ks.Active(); kp != nil {
				metrics.ActiveKeyAgeSeconds.Set(time.Since(kp.CreatedAt).Seconds())
			}
			if list, err := s.ListClients(); err == nil {
				metrics.ClientsRegistered.Set(float64(len(list)))
			}
		}
	}
}

func seedDevClientsIfRequested(s store.Store, log *zap.Logger) {
	if os.Getenv("OAUTH2_DEV_SEED") != "1" {
		return
	}
	mem, ok := s.(*store.MemoryStore)
	if !ok {
		log.Warn("OAUTH2_DEV_SEED=1 but store is not MemoryStore; skipping seed")
		return
	}
	seeds := []struct {
		clientID, secret, name, ownerType, ownerID, scopes string
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
		_ = mem.PutClient(c)
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
