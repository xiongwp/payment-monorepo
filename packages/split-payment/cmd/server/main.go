// Package main — split-payment 服务入口（DB-split Batch 7 极简版）.
//
// 设计原则：split-payment = 资金流状态机执行器
//   1. 读 graph DSL（admin-web 维护）
//   2. 收到 TriggerEvent → translator 翻译成有序 leg 列表
//   3. 严格按定义顺序调 accounting.CreateTransaction
//   4. 记录事件流水到 moneyflow_event_NN 分片表
//
// 删除的功能（vs 老版）：
//   - Kafka event subscriber + refund handler
//   - Saga coordinator + saga step
//   - Stripe-style 业务账本（transfers / fees / payouts / reversals）— accounting 已有
//   - Outbox worker (event_outbox / reversal_retry_outbox)
//   - Hold-period unstick worker / payout cron
//   - Risk gate / FX client（极简版不集成）
//
// 启动顺序（fx Lifecycle）:
//   1. Config / Logger
//   2. DBManager (1 meta + 10 shards) + Router
//   3. Accounting Kitex client
//   4. GraphRepo + EventRepo (mysql 或 memory)
//   5. seed graphs from MONEYFLOW_SEED_DIR (启动期注册 4 大资金流 DSL)
//   6. 启动期 reconcile rules to accounting (graph DSL → transaction_rule)
//   7. 拉起 admin gRPC server (TriggerEvent / SaveGraph / DryRun / GetGraph)
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/cloudwego/kitex/server"
	"go.uber.org/fx"
	"go.uber.org/zap"

	"github.com/xiongwp/split-payment/internal/clients"
	"github.com/xiongwp/split-payment/internal/config"
	"github.com/xiongwp/split-payment/internal/database"
	"github.com/xiongwp/split-payment/internal/domain"
	"github.com/xiongwp/split-payment/internal/grpcsvc"
	"github.com/xiongwp/split-payment/internal/repo"
	"github.com/xiongwp/split-payment/internal/sharding"
	"github.com/xiongwp/split-payment/internal/workflow"

	adminservice "github.com/xiongwp/split-payment/kitex_gen/split_payment/v1/adminservice"
)

func main() {
	fx.New(
		Module,
		fx.Invoke(wireAll),
	).Run()
}

// wireAll 装配业务逻辑 — 接收 fx Providers 给的依赖，组装 repo + 启动 gRPC server。
func wireAll(
	lc fx.Lifecycle,
	cfg *config.Config,
	log *zap.Logger,
	_ zap.AtomicLevel,
	db *sql.DB, // legacy 单 DSN（仅 memory fallback 时为 nil 用）
	dbMgr *database.Manager, // 新分片 manager（含 1 meta + 10 shard 连接池）
	shardRouter *sharding.Router,
	accountingGRPCCli *clients.AccountingGRPCClient,
) error {
	// ─── 1. 构造 repos ────────────────────────────────────────────────
	var (
		graphRepo workflow.GraphRepo
		eventRepo workflow.EventRepo
	)
	if dbMgr != nil {
		graphRepo = repo.NewMySQLGraphRepo(dbMgr)
		eventRepo = repo.NewMySQLRunRepo(dbMgr, shardRouter)
		log.Info("repos: mysql sharded (1 meta + 10 shards)",
			zap.String("meta_dsn", maskDSN(cfg.Database.MetaDB.DSN)),
			zap.Int("shard_count", len(cfg.Database.Shards)))
	} else if db != nil {
		// 仅过渡：老配置只有单 DSN 时，graph 走单 db。新代码应配 meta_database + shards.
		log.Warn("repos: legacy single-DB mode (database.meta_database 未配) —— 推荐切到分片模式")
		return fmt.Errorf("DB-split Batch 7: 不再支持 legacy single-DB；请配 database.meta_database + database.databases[10]")
	} else {
		// 完全 memory 模式，dev 单测用
		mg := repo.NewMemoryGraphRepo()
		seedExampleGraphs(mg, cfg.Seed.GraphDir, log)
		graphRepo = mg
		eventRepo = repo.NewMemoryRunRepo()
		log.Info("repos: memory mode (database.dsn 空)")
	}

	// ─── 2. seed graphs from MONEYFLOW_SEED_DIR ───────────────────────
	// 启动期把 graphs 目录里的 .json 文件 load 进 graph repo，让 trigger 直接用。
	// 用 graphRepo.Save (走完整逻辑) 而不是直接写 mysql。
	if cfg.Seed.GraphDir != "" {
		seedGraphsToRepo(graphRepo, cfg.Seed.GraphDir, log)
	}

	// ─── 3. 启动期 reconcile rule 到 accounting ─────────────────────────
	// 每个 graph 的 edge 派生出一条 transaction_rule，推到 accounting 让它知道
	// (product_code, event_code) → 借/贷科目映射。
	// accounting.http_url 空时跳过（dev / 单仓测）。
	ruleSync := newRuleSyncer(cfg, log)
	if ruleSync != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		reconcileGraphRules(ctx, graphRepo, ruleSync, log)
		cancel()
	}

	// ─── 4. 拉起 admin gRPC server ─────────────────────────────────────
	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			go runAdminGRPCServer(ctx, cfg, log, graphRepo, eventRepo, accountingGRPCCli, ruleSync)
			go runHealthHTTPServer(ctx, cfg, log)
			return nil
		},
		OnStop: func(_ context.Context) error {
			log.Info("shutting down split-payment")
			cancel()
			return nil
		},
	})

	return nil
}

// runAdminGRPCServer 起 Kitex AdminService gRPC server.
// 处理 TriggerEvent / SaveGraph / GetGraph / DryRun 等 admin / 业务 API。
func runAdminGRPCServer(
	ctx context.Context,
	cfg *config.Config,
	log *zap.Logger,
	graphs workflow.GraphRepo,
	events workflow.EventRepo,
	acctCli *clients.AccountingGRPCClient,
	ruleSync grpcsvc.AccountingRuleSyncer,
) {
	addr := cfg.Server.GRPC.Addr
	if addr == "" {
		addr = "0.0.0.0:9098"
	}
	tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		log.Fatal("invalid grpc addr", zap.String("addr", addr), zap.Error(err))
	}

	// grpcsvc.Server 实现了 adminservice.AdminService 的所有 RPC。
	// Accounting 适配：把 clients.AccountingGRPCClient 包成 grpcsvc 期望的接口。
	// grpcsvc.Server 字段：Graphs / Accounting / RuleSync / OrderReset / Audit / Log
	// OrderReset / Audit 极简版留 nil，TriggerEvent 走 fast-fail（不做 stuck-reset 重试）。
	_ = events // EventRepo 暂时不传给 grpcsvc（TriggerEvent 不直接写 event 表）
	srv := &grpcsvc.Server{
		Graphs:     adaptGraphRepo(graphs),
		Accounting: acctClientAdapter{c: acctCli},
		RuleSync:   ruleSync,
		Log:        log,
	}

	server := server.NewServer(server.WithServiceAddr(tcpAddr))
	if err := adminservice.RegisterService(server, srv); err != nil {
		log.Fatal("register admin service", zap.Error(err))
	}
	log.Info("admin gRPC server listening", zap.String("addr", addr))

	// kitex server 在 ctx 取消时自动 graceful stop
	go func() {
		<-ctx.Done()
		_ = server.Stop()
	}()
	if err := server.Run(); err != nil {
		log.Error("admin gRPC server exited", zap.Error(err))
	}
}

// runHealthHTTPServer 起一个简易 HTTP /healthz 端点（admin / liveness probe 用）。
func runHealthHTTPServer(ctx context.Context, cfg *config.Config, log *zap.Logger) {
	addr := cfg.Server.Admin.Addr
	if addr == "" {
		addr = "0.0.0.0:9099"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	log.Info("admin HTTP server listening", zap.String("addr", addr))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("admin HTTP server exited", zap.Error(err))
	}
}

// ─── helpers ──────────────────────────────────────────────────────────

// adaptGraphRepo 把 workflow.GraphRepo 适配成 grpcsvc.GraphRepo（接口同形态）。
func adaptGraphRepo(g workflow.GraphRepo) grpcsvc.GraphRepo {
	return graphRepoAdapter{inner: g}
}

type graphRepoAdapter struct{ inner workflow.GraphRepo }

func (a graphRepoAdapter) GetByKey(ctx context.Context, key string) (*domain.Graph, error) {
	return a.inner.GetByKey(ctx, key)
}
func (a graphRepoAdapter) Save(ctx context.Context, g *domain.Graph) (int64, error) {
	return a.inner.Save(ctx, g)
}
func (a graphRepoAdapter) List(ctx context.Context, status string) ([]*domain.Graph, error) {
	return a.inner.List(ctx, status)
}
func (a graphRepoAdapter) FindByTrigger(ctx context.Context, event string) ([]*domain.Graph, error) {
	return a.inner.FindByTrigger(ctx, event)
}

// acctClientAdapter 把 clients.AccountingGRPCClient 适配成 grpcsvc.AccountingMetaCaller。
type acctClientAdapter struct{ c *clients.AccountingGRPCClient }

func (a acctClientAdapter) CreateTransaction(ctx context.Context, req *domain.TransactionRequest) (*grpcsvc.AccountingTxResp, error) {
	r, err := a.c.CreateTransaction(ctx, req)
	if err != nil {
		return nil, err
	}
	return &grpcsvc.AccountingTxResp{
		VoucherNo: r.VoucherNo,
		Status:    int8(r.Status),
		Error:     r.ErrorMessage,
	}, nil
}

// seedGraphsToRepo 启动期把 dir 下的 .json graphs 全 Save 进 repo。
// 用 graphRepo.Save 走标准路径（自动 bump version + 写 mysql / memory）。
func seedGraphsToRepo(r workflow.GraphRepo, dir string, log *zap.Logger) {
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Info("seed dir not found, skipping", zap.String("dir", dir))
		return
	}
	loaded := 0
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			log.Warn("read seed file", zap.String("file", e.Name()), zap.Error(err))
			continue
		}
		var g domain.Graph
		if err := json.Unmarshal(body, &g); err != nil {
			log.Warn("parse seed graph", zap.String("file", e.Name()), zap.Error(err))
			continue
		}
		if _, err := r.Save(context.Background(), &g); err != nil {
			log.Warn("save seed graph", zap.String("key", g.Key), zap.Error(err))
			continue
		}
		log.Info("seeded graph", zap.String("key", g.Key))
		loaded++
	}
	log.Info("seed done", zap.Int("loaded", loaded), zap.String("dir", dir))
}

// seedExampleGraphs memory 模式专用 seed，跟 seedGraphsToRepo 同逻辑（保留以兼容
// 老的 *repo.MemoryGraphRepo 类型签名）.
func seedExampleGraphs(r *repo.MemoryGraphRepo, dir string, log *zap.Logger) {
	seedGraphsToRepo(r, dir, log)
}

// maskDSN 把 user:pwd@tcp(host:port)/db 里的 pwd 抹掉，仅留 host:port/db 用于 log.
func maskDSN(dsn string) string {
	if dsn == "" {
		return ""
	}
	// 最常见形态: user:pwd@tcp(host:port)/db?...
	at := -1
	for i, c := range dsn {
		if c == '@' {
			at = i
			break
		}
	}
	if at < 0 {
		return "***"
	}
	return "***@" + dsn[at+1:]
}

// newRuleSyncer SaveGraph saga + 启动期 reconcile 用 — 把 graph 派生的
// transaction_rule POST 到 accounting /admin/transaction-rules。
// 配置里 accounting.http_url 空时返 nil（dev 单仓不接 accounting）。
func newRuleSyncer(cfg *config.Config, log *zap.Logger) grpcsvc.AccountingRuleSyncer {
	if cfg.Accounting.HTTPURL == "" {
		log.Warn("split-payment: accounting.http_url empty, SaveGraph saga + reconcile features disabled")
		return nil
	}
	log.Info("split-payment: accounting admin HTTP wired",
		zap.String("accounting_http", cfg.Accounting.HTTPURL))
	return &httpRuleSyncer{baseURL: cfg.Accounting.HTTPURL, log: log}
}

// httpRuleSyncer POST {rules:[...]} 到 accounting /admin/transaction-rules。
type httpRuleSyncer struct {
	baseURL string
	log     *zap.Logger
}

func (h *httpRuleSyncer) UpsertRules(ctx context.Context, rules []grpcsvc.RuleSpec) error {
	if len(rules) == 0 {
		return nil
	}
	body, err := json.Marshal(map[string]any{"rules": rules})
	if err != nil {
		return fmt.Errorf("marshal rules: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST",
		h.baseURL+"/admin/transaction-rules", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post rules: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("accounting upsert-rules HTTP %d", resp.StatusCode)
	}
	return nil
}

func (h *httpRuleSyncer) DeleteRules(ctx context.Context, hashKeys []string) error {
	if len(hashKeys) == 0 {
		return nil
	}
	body, _ := json.Marshal(map[string]any{"hash_keys": hashKeys})
	req, _ := http.NewRequestWithContext(ctx, "DELETE",
		h.baseURL+"/admin/transaction-rules", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("accounting delete-rules HTTP %d", resp.StatusCode)
	}
	return nil
}

// reconcileGraphRules 启动期 self-heal：扫所有 status=active 的 graph，把 rules
// 推到 accounting（防 graph 已存但 rule 漏推的状态漂移）。
func reconcileGraphRules(ctx context.Context, graphs workflow.GraphRepo, sync grpcsvc.AccountingRuleSyncer, log *zap.Logger) {
	list, err := graphs.List(ctx, "active")
	if err != nil {
		log.Warn("startup rule reconcile: list active graphs failed", zap.Error(err))
		return
	}
	if len(list) == 0 {
		log.Info("startup rule reconcile: no active graphs")
		return
	}
	var totalRules, synced, failed int
	for _, g := range list {
		rules := grpcsvc.DeriveRulesFromGraph(g)
		if len(rules) == 0 {
			continue
		}
		if err := sync.UpsertRules(ctx, rules); err != nil {
			log.Warn("startup rule reconcile: upsert failed",
				zap.String("graph_key", g.Key),
				zap.Int("rule_count", len(rules)),
				zap.Error(err))
			failed++
			continue
		}
		totalRules += len(rules)
		synced++
	}
	log.Info("startup rule reconcile complete",
		zap.Int("active_graphs", len(list)),
		zap.Int("synced_graphs", synced),
		zap.Int("failed_graphs", failed),
		zap.Int("total_rules_upserted", totalRules))
}

