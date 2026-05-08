// Command recon-admin 启动 reconplatform 的新一代对账中台：
//
//   - CDC ingester：订阅 13 个业务库 binlog → Redis 事件 + 多维索引
//   - meta syncer：information_schema 定时同步到 Redis（编辑器 autocomplete）
//   - script loader：Yaegi 解释器加载用户对账脚本（admin web 写）
//   - scheduler：cron + Redis Streams 触发脚本
//   - admin web：HTTP API + 单页编辑器（语法检查 / 试运行 / 实时搜索）
//
// 跟老 cmd/main.go 共用同一个 reconplatform 镜像，但二进制独立 —— 部署时
// 同 image 跑两个 container（service 跑老 expr 引擎、admin 跑新对账中台），
// 之后老引擎 retire。
//
// 端点：
//
//	GET  /              redirect → /admin/
//	GET  /admin/        SPA 编辑器
//	*    /api/v1/...    JSON API（脚本 CRUD / 搜索 / meta 浏览 / CDC 状态）
//	GET  /healthz       健康
//	GET  /metrics       Prometheus
//
// env 变量：
//
//	RECON_REDIS_ADDR             redis:6379
//	RECON_HTTP_PORT              8080
//	RECON_CONFIGCENTER_ENDPOINT  http://config-center:9691
//	RECON_ENV                    dev / prod
package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"reconcile-system/internal/api"
	"reconcile-system/internal/cdc"
	"reconcile-system/internal/scheduler"
	"reconcile-system/internal/script"
	"reconcile-system/internal/store"
)

func main() {
	logger, _ := zap.NewProduction()
	defer logger.Sync()

	redisAddr := envOr("RECON_REDIS_ADDR", "localhost:6379")
	httpPort := envOr("RECON_HTTP_PORT", "8080")

	// ─── 基础组件 ─────────────────────────────────────────────
	rdb := redis.NewUniversalClient(&redis.UniversalOptions{
		Addrs: []string{redisAddr},
	})
	defer rdb.Close()

	if err := rdb.Ping(context.Background()).Err(); err != nil {
		logger.Fatal("redis unreachable", zap.String("addr", redisAddr), zap.Error(err))
	}

	searcher := store.NewSearcher(rdb)
	scriptStore := script.NewStore(rdb)
	loader := script.NewLoader()

	// 真 yaegi 后端注入（替换默认 stub）。脚本可 import "recon" 用我们暴露的
	// Context / Diff / Result / EventList。后续要扩展（注入业务自定义包）：
	//   script.SetYaegiBackend(loader, custompack.Symbols)
	script.SetYaegiBackend(loader)
	if err := loader.EnsureYaegiAvailable(); err != nil {
		logger.Fatal("yaegi probe failed", zap.Error(err))
	}
	logger.Info("yaegi backend wired")

	// ─── TTL provider（每表过期时间，从 config-center 拉，OnChange 热更）──
	ttl := cdc.NewTTLProvider()
	// 真实场景下从 config-center key=cdc.ttl 拉 JSON map[string]string
	// 这里给一个内置默认让 dev 起得来
	defaultTTL := map[string]string{
		"default":                                   "30d",
		"*/audit_log":                               "365d",
		"*/outbox":                                  "7d",
		"order-core/payment_intents":                "30d",
		"order-core/charges":                        "30d",
		"accounting-system/account_transaction":     "180d",
		"payment-channel/card_charges":              "365d",
	}
	if invalid := ttl.Reload(defaultTTL); len(invalid) > 0 {
		logger.Warn("ttl config has invalid entries", zap.Strings("invalid", invalid))
	}

	// ─── CDC publisher + manager（实际 Runner 还没接 canal，骨架）──
	publisher := cdc.NewPublisher(rdb, ttl, logger)
	cdcMgr := cdc.NewManager(publisher, nil /* schemaProvider 真接时填 meta.NewProvider */, logger)

	// ─── 启动期 reload 已存的脚本 ────────────────────────────
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	defs, err := scriptStore.ListDefs(ctx, 200)
	if err != nil {
		logger.Warn("load scripts on boot failed (continuing empty)", zap.Error(err))
	}
	for _, def := range defs {
		if err := loader.Upsert(def.ID, def); err != nil {
			logger.Warn("script load failed",
				zap.String("id", def.ID), zap.Error(err))
			continue
		}
	}
	logger.Info("loaded scripts on boot", zap.Int("count", len(defs)))

	// ─── Scheduler（cron + stream）────────────────────────────
	sch := scheduler.New(loader, scriptStore, searcher, rdb, logger)
	go func() {
		if err := sch.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Warn("scheduler exited", zap.Error(err))
		}
	}()

	// ─── HTTP server（admin web + API + metrics）──────────────
	mux := http.NewServeMux()
	apiSrv := api.New(loader, scriptStore, searcher, cdcMgr, logger)
	apiSrv.Mount(mux)

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := rdb.Ping(r.Context()).Err(); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
	})
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/admin/", http.StatusFound)
			return
		}
		http.NotFound(w, r)
	})

	srv := &http.Server{
		Addr:              ":" + httpPort,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		logger.Info("recon-admin: http listening", zap.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http server", zap.Error(err))
		}
	}()

	// CDC manager 启 reload（sources 暂时为空 → 没 runner 跑；真接 canal 后从 config-center 拉 sources reload）
	cdcMgr.Reload(ctx, nil)

	<-ctx.Done()
	logger.Info("shutting down")
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	_ = srv.Shutdown(shutCtx)
	cdcMgr.Stop()
	logger.Info("shutdown complete")
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}
