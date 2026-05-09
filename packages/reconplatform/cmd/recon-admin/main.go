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
// 启动顺序：
//
//	1. Redis ping（fail-fast）
//	2. config-center client（prod fail-fast；dev 不可达走 yaml fallback）
//	3. 拉 cdc.sources + cdc.ttl（从 config-center / yaml fallback）
//	4. ttl.Reload + meta.Syncer goroutine
//	5. CDC manager.Reload(sources) → 启 N 个 binlog Runner（每 shard 一个 channel）
//	6. yaegi loader + 启动期 reload 已存脚本
//	7. scheduler goroutine（cron + Streams）
//	8. HTTP server（admin web + API + metrics）
//	9. config-center OnChange("cdc.sources" / "cdc.ttl") → 热更
//
// env 变量：
//
//	RECON_REDIS_ADDR             redis:6379
//	RECON_HTTP_PORT              8080
//	RECON_CONFIGCENTER_ENDPOINT  http://config-center:9691
//	RECON_ENV                    dev / prod
//	RECON_CDC_SOURCES_YAML       /app/configs/cdc.sources.yaml （dev fallback 路径）
//	RECON_META_SYNC_INTERVAL     5m
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/go-sql-driver/mysql" // meta.Syncer 用 sql.Open("mysql", ...)
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"github.com/xiongwp/payment-util/configcenter"
	"go.uber.org/zap"

	"reconcile-system/internal/api"
	"reconcile-system/internal/cdc"
	"reconcile-system/internal/meta"
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
	// Starlark 引擎默认带 json / time / math / strings / regex / recon 6 个 builtin
	// module，脚本通过 load("@<module>", "func") 引入。
	// 运行时往 engine 上 RegisterModule(name, dict) 可以动态加新 host 包，
	// admin web /api/v1/script/symbols 端点会自动反映新加包，前端补齐立即识别。
	scriptEngine := script.NewEngine(0)
	loader := script.NewLoader(scriptEngine)
	logger.Info("starlark engine wired",
		zap.Strings("builtin_modules", scriptEngine.ModuleNames()))

	// ─── config-center client（prod fail-fast；dev 不可达 → nil 走 yaml fallback）──
	ccCli := newConfigCenterClient(logger)
	if ccCli != nil {
		defer ccCli.Close()
	}

	// ─── ctx ────────────────────────────────────────────────
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// ─── 拉 sources + ttl 初始快照（config-center 优先；不可达走 yaml）──
	cfg, src := loadCDCConfig(ctx, ccCli, logger)
	logger.Info("CDC config loaded",
		zap.String("source", src),
		zap.Int("services", len(cfg.Sources)),
		zap.Int("ttl_entries", len(cfg.TTL)))

	// ─── TTL provider（每表过期时间，OnChange 热更）──
	ttl := cdc.NewTTLProvider()
	if invalid := ttl.Reload(cfg.TTL); len(invalid) > 0 {
		logger.Warn("ttl config has invalid entries", zap.Strings("invalid", invalid))
	}

	// ─── Meta syncer（information_schema → Redis；给编辑器 autocomplete）──
	syncer := meta.NewSyncer(rdb, logger)
	syncer.Reload(toMetaSources(cfg.Sources), cdc.IndexKeysOf(cfg.Sources))
	go func() {
		interval := envDuration("RECON_META_SYNC_INTERVAL", 5*time.Minute)
		if err := syncer.Run(ctx, interval); err != nil && !errors.Is(err, context.Canceled) {
			logger.Warn("meta syncer exited", zap.Error(err))
		}
	}()

	// ─── CDC publisher + manager（每 shard 一个 binlog channel）──
	publisher := cdc.NewPublisher(rdb, ttl, logger)
	cdcMgr := cdc.NewManager(publisher, nil /* canal 自带 schema cache，不需要 SchemaProvider */, logger)

	// 全局加几个内置 enricher / filter（业务可以再扩展）：
	cdcMgr.AddGlobalEnricher(cdc.TraceIDEnricher{})           // trace_id → 提到顶层 Indexes
	cdcMgr.AddGlobalFilter(cdc.SkipShadowRowsFilter{})        // 影子流量数据不入 recon
	cdcMgr.AddGlobalEnricher(cdc.MaskPIIEnricher{Cols: []string{"phone", "email", "id_card"}})

	// ─── 启动期 reload 已存的脚本 ────────────────────────────
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

	// ─── Reload watcher（多实例热刷 via Redis Pub/Sub）─────────
	// admin 在副本 A 保存脚本 → store.PublishReload 广播 → 所有副本的
	// watcher 收到事件 → 从 Redis 拉最新代码 → loader.Upsert，无需重启。
	reloadWatcher := script.NewReloadWatcher(rdb, scriptStore, loader, logger)
	go reloadWatcher.Run(ctx)
	logger.Info("script reload watcher started")

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

	// ─── 启 CDC binlog runners ───────────────────────────────
	cdcMgr.Reload(ctx, cfg.Sources)
	logger.Info("CDC manager reloaded with sources",
		zap.Int("sources", len(cfg.Sources)))

	// ─── OnChange 热更（config-center → cdc.sources / cdc.ttl）──
	if ccCli != nil {
		ccCli.OnChange("cdc.sources", func(v *configcenter.ConfigValue) {
			if v == nil || v.Value == "" {
				logger.Warn("cdc.sources OnChange: empty value, ignoring")
				return
			}
			var payload struct {
				Sources []cdc.Source `json:"sources"`
			}
			if err := json.Unmarshal([]byte(v.Value), &payload); err != nil {
				logger.Warn("cdc.sources OnChange: parse failed",
					zap.Error(err), zap.Int64("version", v.Version))
				return
			}
			for i := range payload.Sources {
				if err := payload.Sources[i].Normalize(); err != nil {
					logger.Warn("cdc.sources OnChange: source invalid",
						zap.Int("idx", i), zap.Error(err))
					return // 拒绝整批新配置，保持旧 runner 跑
				}
				payload.Sources[i].Password = os.ExpandEnv(payload.Sources[i].Password)
				payload.Sources[i].User = os.ExpandEnv(payload.Sources[i].User)
			}
			cdcMgr.Reload(ctx, payload.Sources)
			syncer.Reload(toMetaSources(payload.Sources), cdc.IndexKeysOf(payload.Sources))
			logger.Info("cdc.sources reloaded",
				zap.Int64("version", v.Version),
				zap.Int("sources", len(payload.Sources)))
		})

		ccCli.OnChange("cdc.ttl", func(v *configcenter.ConfigValue) {
			if v == nil || v.Value == "" {
				logger.Warn("cdc.ttl OnChange: empty value, ignoring")
				return
			}
			var ttlMap map[string]string
			if err := json.Unmarshal([]byte(v.Value), &ttlMap); err != nil {
				logger.Warn("cdc.ttl OnChange: parse failed", zap.Error(err))
				return
			}
			if invalid := ttl.Reload(ttlMap); len(invalid) > 0 {
				logger.Warn("cdc.ttl OnChange: invalid entries (kept previous values for those keys)",
					zap.Strings("invalid", invalid))
			}
			logger.Info("cdc.ttl reloaded",
				zap.Int64("version", v.Version),
				zap.Int("entries", len(ttlMap)))
		})
		logger.Info("config-center OnChange wired (cdc.sources / cdc.ttl)")
	}

	<-ctx.Done()
	logger.Info("shutting down")
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	_ = srv.Shutdown(shutCtx)
	cdcMgr.Stop()
	logger.Info("shutdown complete")
}

// newConfigCenterClient prod fail-fast；其它 env 不可达返 nil（本地用 yaml fallback）。
// 跟 cmd/main.go 共用同一份套路，namespace 用 reconplatform。
func newConfigCenterClient(logger *zap.Logger) *configcenter.Client {
	endpoint := envOr("RECON_CONFIGCENTER_ENDPOINT", "http://config-center:9691")
	hostname, _ := os.Hostname()
	rpc := configcenter.NewHTTPClient(endpoint, nil)
	cli, err := configcenter.NewWithRPC(rpc, configcenter.Config{
		Namespace:  "reconplatform",
		InstanceID: hostname,
		Logger:     logger,
	})
	if err != nil {
		env := strings.ToLower(strings.TrimSpace(os.Getenv("RECON_ENV")))
		if env == "prod" || env == "production" {
			logger.Fatal("config-center unreachable in prod (fail-fast)", zap.Error(err))
		}
		logger.Warn("config-center unreachable; falling back to local yaml",
			zap.String("endpoint", endpoint), zap.Error(err))
		return nil
	}
	return cli
}

// loadCDCConfig 拉 sources + ttl 初始快照。
//
//	1. config-center 可达 → 拉 cdc.sources / cdc.ttl
//	2. config-center 不可达 → yaml fallback
//	3. 都失败 → fatal
//
// 返第二个值是 source 描述（log 用："config-center" / "yaml:/app/configs/..."）。
func loadCDCConfig(ctx context.Context, cli *configcenter.Client, logger *zap.Logger) (*cdc.SourcesAndTTL, string) {
	if cli != nil {
		ccAdapter := &configCenterAdapter{c: cli}
		cfg, err := cdc.LoadFromConfigCenter(ctx, ccAdapter)
		if err == nil {
			return cfg, "config-center"
		}
		env := strings.ToLower(strings.TrimSpace(os.Getenv("RECON_ENV")))
		if env == "prod" || env == "production" {
			logger.Fatal("config-center returned no cdc.sources / cdc.ttl in prod", zap.Error(err))
		}
		logger.Warn("config-center cdc.sources fetch failed; trying yaml fallback", zap.Error(err))
	}
	path := envOr("RECON_CDC_SOURCES_YAML", "/app/configs/cdc.sources.yaml")
	cfg, err := cdc.LoadFromFile(path)
	if err != nil {
		logger.Fatal("cdc config load failed (no config-center, no yaml)",
			zap.String("path", path), zap.Error(err))
	}
	return cfg, "yaml:" + path
}

// configCenterAdapter 把 *configcenter.Client 适配成 cdc.ConfigCenterClient
// （cdc 包刻意不依赖 configcenter SDK，所以这里桥接一下）。
type configCenterAdapter struct {
	c *configcenter.Client
}

func (a *configCenterAdapter) GetJSON(ctx context.Context, key string, out any) error {
	v, err := a.c.Get(ctx, key)
	if err != nil {
		return err
	}
	if v == nil || v.Value == "" {
		return errors.New("configcenter: empty value for " + key)
	}
	return json.Unmarshal([]byte(v.Value), out)
}

// toMetaSources 把 []cdc.Source 转成 []meta.Source（meta 用 DSN 拨号 SELECT
// information_schema）。第一个 addr 即可，meta syncer 不需要 binlog 权限。
func toMetaSources(srcs []cdc.Source) []meta.Source {
	ms := cdc.MetaSourcesOf(srcs)
	out := make([]meta.Source, 0, len(ms))
	for _, m := range ms {
		out = append(out, meta.Source{
			Service: m.Service,
			DSN:     m.DSN,
			Schemas: m.Schemas,
		})
	}
	return out
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envDuration(k string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return def
	}
	return d
}
