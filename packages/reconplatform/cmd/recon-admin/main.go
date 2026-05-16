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

	"reconcile-system/internal/anomaly"
	"reconcile-system/internal/api"
	"reconcile-system/internal/approval"
	"reconcile-system/internal/archive"
	"reconcile-system/internal/catalog/seed"
	"reconcile-system/internal/cdc"
	"reconcile-system/internal/diffstate"
	"reconcile-system/internal/eod"
	"reconcile-system/internal/external"
	"reconcile-system/internal/invariant"
	"reconcile-system/internal/meta"
	"reconcile-system/internal/notifier"
	"reconcile-system/internal/scheduler"
	"reconcile-system/internal/script"
	"reconcile-system/internal/store"
	"reconcile-system/internal/tracing"
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

	// ─── 启动期种入内建对账规则 (order ↔ channel ↔ accounting 三方对账 8 条)──
	//
	// 行为:
	//   - 首次启动: 8 条都创建 (UpdatedBy = "system:seeder")
	//   - 二次启动: 比 hash, 内建版本变了且用户没改过 → 自动升级
	//   - 用户改过 (UpdatedBy 非 system:*) → 跳过, 尊重定制
	//
	// 升级新版规则只需:
	//   1. 改 internal/catalog/seed/<id>.star + bump meta-version 注释
	//   2. 重启 admin → 自动覆盖未被用户改过的副本
	seed.MustValidate()
	if n, err := seed.SeedBuiltins(ctx, scriptStore, logger, "system:seeder"); err != nil {
		logger.Warn("seed builtins failed (continuing)", zap.Error(err))
	} else if n > 0 {
		logger.Info("seeded builtin rules", zap.Int("count", n))
	}

	// ─── 启动期 reload 已存的脚本 (含刚 seed 的) ────────────────
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

	// ─── Notifier (sink dispatcher) + Suppressor + DLQ + diffstate ──────────────
	dispatcher := notifier.New(logger)
	dlq := notifier.NewRedisDLQ(rdb, logger)
	dispatcher.SetDLQ(dlq)
	suppressor := notifier.NewSuppressor(rdb, 5*time.Minute)
	diffStore := diffstate.New(rdb)

	// 自动 ack 规则引擎：CreateOpen 后立即评估，命中规则的 diff 直接 transition
	// 到 false_positive / resolved，oncall 不打扰。配置走 config-center
	// reconplatform/auto_rules（JSON list）+ OnChange 热更。
	autoRules := diffstate.NewRulesEngine(logger)
	if ccCli != nil {
		if cv, err := ccCli.Get(ctx, "auto_rules"); err == nil && cv != nil && cv.Value != "" {
			_ = autoRules.LoadJSON(cv.Value)
		}
		ccCli.OnChange("auto_rules", func(v *configcenter.ConfigValue) {
			if v != nil {
				_ = autoRules.LoadJSON(v.Value)
			}
		})
	}

	// 注 webhook / 钉钉 / Slack sink — 配置走 config-center
	// (key=reconplatform/notifier.sinks JSON list)。Hot reload 也接 OnChange。
	loadSinksFromConfig(ctx, dispatcher, ccCli, logger)
	if ccCli != nil {
		ccCli.OnChange("notifier.sinks", func(_ *configcenter.ConfigValue) {
			loadSinksFromConfig(ctx, dispatcher, ccCli, logger)
		})
	}

	// 把 notifier + diffstate 接到 Loader.Run 出口：
	//   1. Suppressor.Filter 过一遍（5min 窗口同 type+key 仅发首条）
	//   2. dispatcher.Dispatch 分发到 sinks（失败进 DLQ）
	//   3. diffstate.CreateOpen 落 store 让运营在 admin web 处置
	loader.AddPostRunHook(func(_ *script.Context, r *script.Result) {
		if r == nil || len(r.Diffs) == 0 {
			return
		}
		// 转 notifier 视图
		nDiffs := make([]notifier.Diff, len(r.Diffs))
		for i, d := range r.Diffs {
			nDiffs[i] = notifier.Diff{Type: d.Type, Key: d.Key, Want: d.Want, Got: d.Got, Detail: d.Detail}
		}
		nr := &notifier.RunResult{
			ScriptID: r.ScriptID, RunID: r.RunID, StartedAt: r.StartedAt,
			FinishedAt: r.FinishedAt, Status: r.Status, Error: r.Error,
			TriggeredBy: r.TriggeredBy, Diffs: nDiffs,
		}
		filtered, _ := suppressor.Filter(ctx, nr)
		dispatcher.Dispatch(ctx, filtered)
		// diffstate.CreateOpen 每条 diff 一条 + 立即过自动规则
		for i, d := range r.Diffs {
			detailMap, _ := d.Detail.(map[string]any)
			// 自动把 detail.event.indexes.trace_id 提到 detail.trace_id 顶层 —
			// 这样 ClickHouse 归档 / admin web 详情面板都能直接读到。
			if detailMap != nil {
				if _, ok := detailMap["trace_id"]; !ok {
					if tid := tracing.ExtractTraceID(detailMap); tid != "" {
						detailMap["trace_id"] = tid
					}
				}
			}
			ds := diffstate.Diff{
				ID:        diffstate.IDFor(r.ScriptID, r.RunID, d.Type, d.Key, i),
				ScriptID:  r.ScriptID, RunID: r.RunID,
				Type: d.Type, Key: d.Key, Detail: detailMap,
			}
			if err := diffStore.CreateOpen(ctx, ds); err != nil {
				continue
			}
			// 立即过自动规则；命中就 Transition 到 false_positive / resolved
			autoRules.ApplyOnCreate(ctx, diffStore, &ds)
		}
	})

	// 后台 GC：N 天前 open diff 自动 expire（默认 30d）
	go func() {
		t := time.NewTicker(1 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if n, err := diffStore.ExpireOld(ctx, 30*24*time.Hour); err == nil && n > 0 {
					logger.Info("diffstate GC: expired old open diffs", zap.Int("count", n))
				}
			}
		}
	}()

	// ─── 冷热分层归档（ClickHouse）─────────────────────────
	// 配置走 env：CLICKHOUSE_URL / CLICKHOUSE_USER / CLICKHOUSE_PASSWORD / CLICKHOUSE_DB。
	// 未设置 → 跳过归档，admin /api/v1/diffs/_search 返 501（hot only 模式）。
	var archiver *archive.Archiver
	if chURL := os.Getenv("CLICKHOUSE_URL"); chURL != "" {
		acfg := archive.DefaultConfig()
		acfg.URL = chURL
		if v := os.Getenv("CLICKHOUSE_DB"); v != "" {
			acfg.Database = v
		}
		acfg.User = os.Getenv("CLICKHOUSE_USER")
		acfg.Password = os.Getenv("CLICKHOUSE_PASSWORD")
		archiver = archive.New(acfg, rdb, diffStore, logger)
		// 启动时建表（IF NOT EXISTS 幂等）
		if err := archiver.EnsureSchema(ctx); err != nil {
			logger.Warn("clickhouse schema ensure failed (archive disabled)",
				zap.Error(err))
			archiver = nil
		} else {
			go archiver.Run(ctx)
			logger.Info("archive worker started",
				zap.String("ch_url", acfg.URL),
				zap.Duration("hot_window", acfg.HotWindow))
		}
	} else {
		logger.Info("CLICKHOUSE_URL not set — cold archive disabled (hot-only mode)")
	}

	// ─── HTTP server（admin web + API + metrics）──────────────
	mux := http.NewServeMux()
	// ─── 6 大企业级模块 ────────────────────────────────────────
	// External 文件源（SFTP/CSV 银行流水）— sources 走 config-center 拉
	extMgr := external.NewManager(rdb, logger)
	if ccCli != nil {
		ccAdapter := &configCenterAdapter{c: ccCli}
		var extPayload struct {
			Sources []external.Source `json:"sources"`
		}
		if err := ccAdapter.GetJSON(ctx, "external.sources", &extPayload); err == nil {
			for _, src := range extPayload.Sources {
				tr, err := external.BuildTransport(src.Transport)
				if err != nil {
					logger.Warn("external transport build failed",
						zap.String("name", src.Name), zap.Error(err))
					continue
				}
				pr, err := external.BuildParser(src.ParserCfg)
				if err != nil {
					logger.Warn("external parser build failed",
						zap.String("name", src.Name), zap.Error(err))
					continue
				}
				if err := extMgr.Register(src, tr, pr); err != nil {
					logger.Warn("external register failed",
						zap.String("name", src.Name), zap.Error(err))
				}
			}
		}
	}
	extMgr.Start()
	defer extMgr.Stop(context.Background())

	// Invariant 声明式恒等检查
	invEng := invariant.New(rdb, diffStore, logger)
	if ccCli != nil {
		ccAdapter := &configCenterAdapter{c: ccCli}
		var invPayload struct {
			Invariants []invariant.Spec `json:"invariants"`
		}
		if err := ccAdapter.GetJSON(ctx, "invariants", &invPayload); err == nil && len(invPayload.Invariants) > 0 {
			if err := invEng.LoadSpecs(ctx, invPayload.Invariants); err != nil {
				logger.Warn("invariants load failed", zap.Error(err))
			}
		}
	}
	invEng.Start()
	defer invEng.Stop(context.Background())

	// EOD 日切对账
	eodRunner := eod.NewRunner(rdb, logger)
	// runFn 占位 — 真实实现需要 SQL 拉 T-1 数据 + 跑 catalog 选定脚本。
	// 留给后续接到 batch runner（已存在）+ 限定时间窗即可。
	_ = eodRunner.Schedule(envOr("RECON_EOD_SCHEDULE", "0 2 * * *"),
		func(ctx context.Context, date string) (*eod.Report, error) {
			t0 := time.Now()
			return &eod.Report{
				Date:        date,
				StartedAt:   t0,
				FinishedAt:  time.Now(),
				DurationMs:  time.Since(t0).Milliseconds(),
				Status:      "ok",
				DiffsByType: map[string]int{},
				DiffsBySev:  map[string]int{},
			}, nil
		})
	eodRunner.Start()
	defer eodRunner.Stop(context.Background())

	// Approval 双人复核
	approvalMgr := approval.New(rdb, diffStore, approval.DefaultPolicy())

	// Anomaly 异常检测（小时级 EWMA z-score）
	anomalyDet := anomaly.New(rdb, diffStore, logger)
	if err := anomalyDet.Start(); err != nil {
		logger.Warn("anomaly detector start failed", zap.Error(err))
	}
	defer anomalyDet.Stop(context.Background())

	apiSrv := api.New(loader, scriptStore, searcher, cdcMgr, logger)
	apiSrv.WithDiffStore(diffStore).
		WithDLQ(dlq, dispatcher).
		WithPublisher(publisher).
		WithRedis(rdb)
	if archiver != nil {
		apiSrv.WithArchiver(archiver)
	}
	// OTel/Jaeger 配置 — 没设 JAEGER_UI_URL 时仍可调端点（返空 URL）
	apiSrv.WithTracing(tracing.FromEnv())
	// 6 大企业级模块全部注入
	apiSrv.WithExternalManager(extMgr).
		WithInvariantEngine(invEng).
		WithEODRunner(eodRunner).
		WithApprovalManager(approvalMgr).
		WithAnomalyDetector(anomalyDet)
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

	// ─── SSE BroadcastHub (PERF-3): 单 XREAD fan-out 替代 per-client ───
	// 替换 api.Mount 注册的 /api/v1/events/stream 实现.
	// 路由覆盖逻辑: 同 path 后注册会覆盖前面 — Go ServeMux 实际抛 panic,
	// 因此用 wrapper mux 截到该 path 优先走 hub.
	hub := api.NewBroadcastHub(rdb, logger)
	hub.Start(ctx)
	rootMux := http.NewServeMux()
	rootMux.HandleFunc("/api/v1/events/stream", hub.HandleSSE)
	rootMux.Handle("/", mux)

	// PERF-6: gzip + ETag 中间件包整个 handler tree
	handler := api.WithCompression(rootMux)

	srv := &http.Server{
		Addr:              ":" + httpPort,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		logger.Info("recon-admin: http listening", zap.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http server", zap.Error(err))
		}
	}()

	// ─── 启 CDC binlog runners ───────────────────────────────
	//
	// PIPE-CDC-BRIDGE: 若启了独立 cdc-bridge 进程, 这里跳过 — 同一 MySQL
	// server-id 只能有一个 binlog 消费者. 通过 RECON_ADMIN_DISABLE_CDC=1 关.
	if strings.EqualFold(os.Getenv("RECON_ADMIN_DISABLE_CDC"), "1") ||
		strings.EqualFold(os.Getenv("RECON_ADMIN_DISABLE_CDC"), "true") {
		logger.Info("CDC manager disabled (RECON_ADMIN_DISABLE_CDC=1); " +
			"binlog reading expected to be handled by recon-pipeline cdc-bridge role")
	} else {
		cdcMgr.Reload(ctx, cfg.Sources)
		logger.Info("CDC manager reloaded with sources",
			zap.Int("sources", len(cfg.Sources)))
	}

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
				// ⚠️ 不要用 os.ExpandEnv —— 不支持 bash 风格 ${VAR:-default}，
				// 会把 "RECON_CDC_PASS:-recon_cdc_pwd" 当变量名找空 → 密码空
				// → canal 1045 access denied。必须用 cdc 包内自带的兼容函数。
				payload.Sources[i].Password = cdc.ExpandEnvShellLike(payload.Sources[i].Password)
				payload.Sources[i].User = cdc.ExpandEnvShellLike(payload.Sources[i].User)
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

// loadSinksFromConfig 从 config-center 拉 notifier.sinks JSON list 注册到 dispatcher。
//
// 配置格式（key=reconplatform/notifier.sinks）：
//
//	[
//	  {"name":"webhook:oncall",  "type":"webhook",  "url":"https://oncall.internal/recon", "headers":{"X-Auth":"..."}},
//	  {"name":"dingtalk:risk",   "type":"dingtalk", "access_token":"abc..."},
//	  {"name":"slack:platform",  "type":"slack",    "url":"https://hooks.slack.com/..."}
//	]
//
// 默认（config 不可达 / key 缺失）：仅 LogSink，diff 进日志不发外部告警。
func loadSinksFromConfig(ctx context.Context, dispatcher *notifier.Dispatcher, cli *configcenter.Client, logger *zap.Logger) {
	if cli == nil {
		logger.Info("notifier: no config-center, only LogSink active")
		return
	}
	cv, err := cli.Get(ctx, "notifier.sinks")
	if err != nil || cv == nil || cv.Value == "" {
		logger.Info("notifier: notifier.sinks not configured, only LogSink active")
		return
	}
	var entries []struct {
		Name        string            `json:"name"`
		Type        string            `json:"type"`
		URL         string            `json:"url"`
		AccessToken string            `json:"access_token"`
		Headers     map[string]string `json:"headers"`
	}
	if err := json.Unmarshal([]byte(cv.Value), &entries); err != nil {
		logger.Warn("notifier: parse notifier.sinks failed", zap.Error(err))
		return
	}
	defaults := []string{"log"}
	for _, e := range entries {
		switch e.Type {
		case "webhook":
			dispatcher.RegisterSink(notifier.NewWebhookSink(e.Name, e.URL, e.Headers))
		case "dingtalk":
			dispatcher.RegisterSink(notifier.NewDingTalkSink(e.Name, e.AccessToken))
		case "slack":
			dispatcher.RegisterSink(notifier.NewSlackSink(e.Name, e.URL))
		default:
			logger.Warn("notifier: unknown sink type, skipping",
				zap.String("name", e.Name), zap.String("type", e.Type))
			continue
		}
		defaults = append(defaults, e.Name)
	}
	dispatcher.SetDefault(defaults)
	logger.Info("notifier: sinks loaded", zap.Strings("sinks", defaults))
}
