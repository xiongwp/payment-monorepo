package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/accounting-system/internal/adminhttp"
	commonutil "github.com/accounting-system/internal/common"
	grpcserver "github.com/accounting-system/internal/grpc"
	"github.com/accounting-system/internal/idgen"
	"github.com/accounting-system/internal/reconcile"
	"github.com/accounting-system/internal/infrastructure/cache"
	"github.com/accounting-system/internal/infrastructure/database"
	kafkamq "github.com/accounting-system/internal/infrastructure/kafka"
	"github.com/accounting-system/internal/infrastructure/logging"
	"github.com/accounting-system/internal/infrastructure/sharding"
	"github.com/accounting-system/internal/metrics"
	"github.com/accounting-system/internal/repository"
	"github.com/accounting-system/internal/service"
	"github.com/prometheus/client_golang/prometheus"
	promdto "github.com/prometheus/client_model/go"
	"github.com/spf13/viper"
	"github.com/xiongwp/payment-util/configcenter"
	"github.com/xiongwp/payment-util/serviceregistry"
	"github.com/xiongwp/payment-util/trace"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/fx"
	"go.uber.org/zap"
	gormlogger "gorm.io/gorm/logger"
)

func main() {
	metrics.Register()

	otelShutdown, otelErr := trace.InitOTel(context.Background(), "accounting-system", os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	if otelErr != nil {
		fmt.Fprintln(os.Stderr, "otel init:", otelErr)
	}
	defer func() {
		if otelShutdown != nil {
			_ = otelShutdown(context.Background())
		}
	}()

	app := fx.New(
		fx.Provide(
			NewConfig,
			logging.NewLoggers,
			AppLoggerFromLoggers,
			NewDatabaseManager,
			NewShardingRouter,
		),
		fx.Module("repository",
			fx.Decorate(func(l *logging.Loggers) *zap.Logger { return l.Repository }),
			fx.Provide(
				repository.NewAccountRepository,
				repository.NewTransactionRepository,
				repository.NewTccRepository,
				repository.NewTccCoordinatorRepository,
				repository.NewTransactionRuleRepository,
				repository.NewTransactionOrderRepository,
				repository.NewFreezeOrderRepository,
				repository.NewFreezeCompensateOutboxRepository,
				repository.NewSettlementOutboxRepository,
				repository.NewTrialBalanceRepository,
				repository.NewBatchOrderRepository,
				repository.NewBalanceBufferRepository,
				repository.NewHotAccountRepository,
				repository.NewBufferAccountRepository,
				repository.NewAccountBusinessTypeRepository,
				repository.NewServiceInstanceRepository,
				repository.NewDayCutControlRepository,
				repository.NewBalanceSnapshotRepository,
				repository.NewVoucherRepository,
				repository.NewAsyncTaskRepository,
				idgen.NewIDGeneratorFromManager,
			),
		),
		fx.Module("service",
			fx.Decorate(func(l *logging.Loggers) *zap.Logger { return l.Service }),
			fx.Provide(
				NewConfigCenterClient,
				service.NewAccountingService,
				service.NewHotPathEnabler,
				service.NewTccService,
				service.NewAsyncTaskService,
				service.NewDayCutService,
				service.NewTransactionService,
				service.NewSystemConfigService,
				service.NewTrialBalanceService,
				service.NewFreezeService,
				service.NewAdjustmentService,
				service.NewCutDateProvider,
				service.NewDayCutScheduler,
			),
		),
		fx.Provide(service.NewTccRecoveryWorker),
		fx.Provide(func(svc service.AccountingService) service.ConfirmRetrier { return svc }),
		fx.Provide(NewTccArchiveWorkerFromConfig),
		fx.Provide(func(svc service.AccountingService) service.FlushIntervalProvider { return svc }),
		fx.Provide(service.NewBufferedBalanceWorker),
		fx.Provide(service.NewIntegrityCheckWorker),
		fx.Provide(service.NewFreezeCompensateOutboxWorker),
		fx.Provide(NewReconcileWorkerFromConfig),
		fx.Provide(grpcserver.NewServer),
		fx.Provide(NewAdminHTTPConfig),
		fx.Provide(func(m *database.Manager) adminhttp.HealthPinger { return m }),
		fx.Provide(adminhttp.NewServer),
		fx.Provide(NewEtcdClient),
		fx.Invoke(
			StartGRPCServer,
			StartAdminHTTPServer,
			StartServiceRegistrar,
			StartTccRecovery,
			StartTccArchive,
			StartBufferedBalanceWorker,
			StartConfigSyncWorker,
			StartDBPoolMetricsCollector,
			StartOutboxBackpressureController,
			StartDayCutResumeAtStartup,
			StartDayCutScheduler,
			StartIntegrityCheckScheduler,
			StartFreezeCompensateOutboxWorker,
			StartReconcileWorker,
			SetupHotPath,
		),
	)
	app.Run()
}

func AppLoggerFromLoggers(l *logging.Loggers) *zap.Logger { return l.App }

func NewConfig() (*viper.Viper, error) {
	v := viper.New()
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath("./config")
	v.AddConfigPath(".")
	v.AutomaticEnv()
	_ = v.BindEnv("env")
	if err := v.ReadInConfig(); err != nil {
		return nil, err
	}
	if err := assertProdSafety(v); err != nil {
		return nil, err
	}
	return v, nil
}

// assertProdSafety accounting-system prod 校验：
//   - auth.allow_unauthenticated 必须 false（账务服务无鉴权 = 任何人能改余额）
//   - 必须配 mTLS 或 token（mesh 内服务调本服务必须鉴权）
//   - 数据库 DSN 不能含弱密码（password / 123456 等明显默认值）
func assertProdSafety(v *viper.Viper) error {
	env := strings.ToLower(strings.TrimSpace(v.GetString("env")))
	if env != "prod" && env != "production" {
		return nil
	}
	if v.GetBool("auth.allow_unauthenticated") {
		return fmt.Errorf("PROD-SAFETY: accounting-system auth.allow_unauthenticated=true is forbidden in env=prod (anyone could mutate balances)")
	}
	// 弱密码检查：DSN 含 :password@ 或 :123456@ 等明显默认值 → 拒绝
	weakPatterns := []string{":password@", ":123456@", ":root@", ":admin@", ":test@"}
	for _, dsnKey := range []string{"database.meta.dsn"} {
		dsn := strings.ToLower(v.GetString(dsnKey))
		for _, p := range weakPatterns {
			if strings.Contains(dsn, p) {
				return fmt.Errorf("PROD-SAFETY: %s contains weak credential pattern %q in env=prod", dsnKey, p)
			}
		}
	}
	// config-center 是全平台动态配置强依赖；prod 必填 endpoint。
	return configcenter.AssertProdMandatory(v)
}

func NewDatabaseManager(v *viper.Viper, loggers *logging.Loggers, logger *zap.Logger) (*database.Manager, error) {
	var shardCfgs []database.DBConfig
	if err := v.UnmarshalKey("database.databases", &shardCfgs); err != nil {
		return nil, err
	}
	var metaCfg database.DBConfig
	if err := v.UnmarshalKey("database.meta_database", &metaCfg); err != nil {
		return nil, err
	}
	var gormLog gormlogger.Interface
	if loggers != nil {
		gormLog = logging.NewGormLogger(loggers.Repository, loggers.Performance, loggers.SlowQueryMs)
	}
	var mgr *database.Manager
	var err error
	if metaCfg.DSN != "" {
		mgr, err = database.NewManagerWithMeta(metaCfg, shardCfgs, gormLog)
	} else {
		logger.Warn("meta_database not configured, falling back to accounting_db_0 for meta queries")
		mgr, err = database.NewManager(shardCfgs, gormLog)
	}
	if err != nil {
		return nil, err
	}
	logger.Info("database ready", zap.Int("shards", mgr.DBCount()))

	// 暴露每个 shard 的连接池指标（accounting_db_*{db="shardN"|"meta"}）。
	// 接近 max_open 时报警；wait_seconds_total rate 是 pool exhaustion 信号。
	metrics.RegisterDBStats(mgr.SQLDBs())
	if v.GetBool("database.warm_pool_on_start") {
		warmCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		go func() {
			defer cancel()
			mgr.WarmPools(warmCtx)
			logger.Info("database pools warmed (max-open conns established)")
		}()
	}
	return mgr, nil
}

func NewShardingRouter(v *viper.Viper) *sharding.Router {
	dbCount := v.GetInt("database.shard_count")
	tableCount := v.GetInt("database.table_count")
	if dbCount == 0 {
		dbCount = 10
	}
	if tableCount == 0 {
		tableCount = 100
	}
	tablePerDB := tableCount / dbCount
	if tablePerDB == 0 {
		tablePerDB = 10
	}
	return sharding.NewRouterWithConfig(dbCount, tablePerDB)
}

func StartGRPCServer(lc fx.Lifecycle, srv *grpcserver.Server, v *viper.Viper, logger *zap.Logger) {
	port := v.GetInt("server.grpc_port")
	if port == 0 {
		port = 50051
	}
	metricsPort := v.GetString("server.metrics_port")
	if metricsPort == "" {
		metricsPort = "9090"
	}
	loadShed := grpcserver.LoadShedConfig{
		MaxInflight:   v.GetInt("server.load_shed.max_inflight"),
		RatePerSecond: v.GetInt("server.load_shed.rate_per_second"),
		Burst:         v.GetInt("server.load_shed.burst"),
	}
	// 服务-到-服务鉴权 token 优先级：env > yaml。生产应通过 K8s Secret 注入 env，
	// 不要把 secret 提交进 config.yaml。空 = warn-only（拦截器仍挂着但放行）。
	svcToken := os.Getenv("ACCOUNTING_SERVICE_TOKEN")
	if svcToken == "" {
		svcToken = v.GetString("server.grpc_service_token")
	}
	srv.SetServiceToken(svcToken)
	maxRPCDurationSec := v.GetInt("server.max_rpc_duration_seconds")
	maxRPCDuration := time.Duration(maxRPCDurationSec) * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			metrics.StartServer(metricsPort, logger)
			if loadShed.MaxInflight > 0 || loadShed.RatePerSecond > 0 {
				logger.Info("gRPC load shedding enabled",
					zap.Int("max_inflight", loadShed.MaxInflight),
					zap.Int("rate_per_second", loadShed.RatePerSecond),
					zap.Int("burst", loadShed.Burst),
				)
			}
			if maxRPCDuration > 0 {
				logger.Info("gRPC per-RPC timeout enabled", zap.Duration("max_rpc_duration", maxRPCDuration))
			}
			go func() {
				if err := srv.ListenAndServe(ctx, port, loadShed, maxRPCDuration); err != nil {
					logger.Error("gRPC server stopped", zap.Error(err))
				}
			}()
			return nil
		},
		OnStop: func(stopCtx context.Context) error {
			// gRPC graceful shutdown：拒新连接 + 等 in-flight RPC 完成。
			// 超时 25s（fx 默认 onstop 超时 30s，留余量给其它 OnStop hook）。
			gsCtx, gsCancel := context.WithTimeout(stopCtx, 25*time.Second)
			defer gsCancel()
			err := srv.Stop(gsCtx)
			cancel() // 兜底：让 ctx.Done() 路径也能 unblock
			return err
		},
	})
}

func NewAdminHTTPConfig(v *viper.Viper) adminhttp.Config {
	port := v.GetInt("server.port")
	if port == 0 {
		port = 8888
	}
	grpcPort := v.GetInt("server.grpc_port")
	if grpcPort == 0 {
		grpcPort = 50051
	}
	return adminhttp.Config{
		Host:      v.GetString("server.instance_host"),
		AdminPort: port,
		GRPCPort:  grpcPort,
		AuthToken: v.GetString("server.admin_http_token"),
	}
}

func StartAdminHTTPServer(lc fx.Lifecycle, srv *adminhttp.Server, logger *zap.Logger) {
	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			return srv.Start(ctx)
		},
		OnStop: func(stopCtx context.Context) error {
			srv.BeginDrain()
			time.Sleep(2 * time.Second)
			waited, remaining := srv.WaitDrain(stopCtx, 25*time.Second)
			logger.Info("admin http: drain finished",
				zap.Duration("waited", waited),
				zap.Float64("remaining_inflight", remaining),
			)
			cancel()
			return srv.Stop(stopCtx)
		},
	})
}

func waitWithDeadline(stopCtx context.Context, name string, logger *zap.Logger, wait func()) {
	done := make(chan struct{})
	go func() {
		wait()
		close(done)
	}()
	select {
	case <-done:
		logger.Debug("worker drained", zap.String("worker", name))
	case <-stopCtx.Done():
		logger.Warn("worker drain timed out, exiting anyway", zap.String("worker", name))
	case <-time.After(5 * time.Second):
		logger.Warn("worker drain safety timeout 5s, exiting anyway", zap.String("worker", name))
	}
}

// StartBufferedBalanceWorker 多 pod leader-gated。
//
// 正确性：BufferedBalanceWorker 内部 LockRow (FOR UPDATE) 已经保证多 pod 并发
// 同 buffer 行不会双 flush（第二个 pod 看到 pending=0 自然 no-op）。
// 但每个 pod 都跑会造成 100 张分表的扫描负载翻 N 倍 → 用 leader election
// 限制全集群只一个跑，节省 DB 资源。leader 挂了下一副本秒级接管。
func StartBufferedBalanceWorker(lc fx.Lifecycle, worker *service.BufferedBalanceWorker, cli *clientv3.Client, logger *zap.Logger) {
	ctx, cancel := context.WithCancel(context.Background())
	id := hostnameOrUnknown()
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			go serviceregistry.RunLeaderLoop(ctx, cli,
				"/leader/accounting-system/buffered-balance",
				id, 10*time.Second,
				func(leaderCtx context.Context) {
					logger.Info("buffered balance worker elected leader", zap.String("identity", id))
					worker.Start(leaderCtx)
					worker.Wait()
					logger.Info("buffered balance worker leadership released", zap.String("identity", id))
				})
			logger.Info("buffered balance worker registered (leader-gated; flush every 30s)")
			return nil
		},
		OnStop: func(stopCtx context.Context) error {
			cancel()
			waitWithDeadline(stopCtx, "buffered_balance", logger, worker.Wait)
			return nil
		},
	})
}

func StartConfigSyncWorker(lc fx.Lifecycle, svc service.AccountingService, sysCfg service.SystemConfigService, ruleRepo repository.TransactionRuleRepository, logger *zap.Logger) {
	const configSyncInterval = 60 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			if err := svc.ReloadRegistry(ctx); err != nil {
				logger.Warn("startup: load type registry failed (fallback to per-request DB lookup)",
					zap.Error(err))
			} else {
				logger.Info("startup: account_type + business_type registry loaded into memory")
			}
			if err := ruleRepo.Reload(ctx); err != nil {
				logger.Warn("startup: load transaction_rule + account_type_info failed (fallback to per-request DB lookup)",
					zap.Error(err))
			} else {
				logger.Info("startup: transaction rules + account types loaded into memory")
			}
			if cnt, err := sysCfg.Reload(ctx); err != nil {
				logger.Warn("startup: load system_config failed (using defaults)", zap.Error(err))
			} else {
				logger.Info("startup: system_config loaded into memory", zap.Int("count", cnt))
			}
			go func() {
				ticker := time.NewTicker(configSyncInterval)
				defer ticker.Stop()
				for {
					select {
					case <-ticker.C:
						if cnt, err := svc.ReloadBufferAccountConfig(ctx); err != nil {
							logger.Warn("config sync: reload buffer account config failed", zap.Error(err))
						} else {
							logger.Debug("config sync: buffer account config reloaded", zap.Int("count", cnt))
						}
						if cnt, err := svc.ReloadHotAllowlist(ctx); err != nil {
							logger.Warn("config sync: reload hot allowlist failed", zap.Error(err))
						} else {
							logger.Debug("config sync: hot allowlist reloaded", zap.Int("count", cnt))
						}
						if err := svc.ReloadRegistry(ctx); err != nil {
							logger.Warn("config sync: reload type registry failed", zap.Error(err))
						}
						if cnt, err := sysCfg.Reload(ctx); err != nil {
							logger.Warn("config sync: reload system_config failed", zap.Error(err))
						} else {
							logger.Debug("config sync: system_config reloaded", zap.Int("count", cnt))
						}
						// transaction_rule + account_type_info：60s 兜底全表 reload。
						// 主路径是 admin /admin/reload/transaction-rules 即时扇出，
						// 这里只是网络分区 / fanout 漏掉的实例兜底。
						if err := ruleRepo.Reload(ctx); err != nil {
							logger.Warn("config sync: reload transaction rules failed", zap.Error(err))
						}
					case <-ctx.Done():
						return
					}
				}
			}()
			logger.Info("config sync worker started: reloading buffer/hot account/type registry/system_config every 60s")
			return nil
		},
		OnStop: func(_ context.Context) error {
			cancel()
			return nil
		},
	})
}

func StartDBPoolMetricsCollector(lc fx.Lifecycle, mgr *database.Manager, logger *zap.Logger) {
	const collectInterval = 10 * time.Second
	collect := func() {
		shards := mgr.GetAllDBs()
		for i, gdb := range shards {
			sqlDB, err := gdb.DB()
			if err != nil {
				continue
			}
			stats := sqlDB.Stats()
			label := fmt.Sprintf("%d", i)
			metrics.DBPoolOpenConns.WithLabelValues(label).Set(float64(stats.OpenConnections))
			metrics.DBPoolInUseConns.WithLabelValues(label).Set(float64(stats.InUse))
			metrics.DBPoolIdleConns.WithLabelValues(label).Set(float64(stats.Idle))
			metrics.DBPoolWaitCount.WithLabelValues(label).Set(float64(stats.WaitCount))
			metrics.DBPoolWaitSeconds.WithLabelValues(label).Set(stats.WaitDuration.Seconds())
			metrics.DBPoolMaxOpenConns.WithLabelValues(label).Set(float64(stats.MaxOpenConnections))
		}
		if mdb, err := mgr.GetMetaDB(); err == nil {
			if len(shards) == 0 || mdb != shards[0] {
				if sqlDB, err := mdb.DB(); err == nil {
					stats := sqlDB.Stats()
					metrics.DBPoolOpenConns.WithLabelValues("meta").Set(float64(stats.OpenConnections))
					metrics.DBPoolInUseConns.WithLabelValues("meta").Set(float64(stats.InUse))
					metrics.DBPoolIdleConns.WithLabelValues("meta").Set(float64(stats.Idle))
					metrics.DBPoolWaitCount.WithLabelValues("meta").Set(float64(stats.WaitCount))
					metrics.DBPoolWaitSeconds.WithLabelValues("meta").Set(stats.WaitDuration.Seconds())
					metrics.DBPoolMaxOpenConns.WithLabelValues("meta").Set(float64(stats.MaxOpenConnections))
				}
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			collect()
			go func() {
				ticker := time.NewTicker(collectInterval)
				defer ticker.Stop()
				for {
					select {
					case <-ticker.C:
						collect()
					case <-ctx.Done():
						return
					}
				}
			}()
			logger.Info("db pool metrics collector started",
				zap.Duration("interval", collectInterval),
				zap.Int("shards", mgr.DBCount()))
			return nil
		},
		OnStop: func(_ context.Context) error {
			cancel()
			return nil
		},
	})
}

func StartDayCutResumeAtStartup(lc fx.Lifecycle, dayCutSvc service.DayCutService, dayCutRepo repository.DayCutControlRepository, router *sharding.Router, logger *zap.Logger) {
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer cancel()
				dates := []string{
					time.Now().Format("2006-01-02"),
					time.Now().AddDate(0, 0, -1).Format("2006-01-02"),
				}
				const stuckThreshold = 60 * time.Second
				for _, cutDate := range dates {
					shards := router.GetAllShards()
					runIDsSeen := make(map[int]struct{})
					for _, shard := range shards {
						if ctx.Err() != nil {
							return
						}
						maxRun, err := dayCutRepo.GetMaxRunID(ctx, shard.DBIndex, shard.TableIndex, cutDate)
						if err != nil || maxRun == 0 {
							continue
						}
						for r := 1; r <= maxRun; r++ {
							runIDsSeen[r] = struct{}{}
						}
					}
					for runID := range runIDsSeen {
						count, err := dayCutSvc.ResumeStuckShards(ctx, cutDate, runID, stuckThreshold)
						if err != nil {
							logger.Warn("startup resume: ResumeStuckShards failed",
								zap.String("cutDate", cutDate),
								zap.Int("runID", runID),
								zap.Error(err))
							continue
						}
						if count > 0 {
							logger.Warn("startup resume: re-dispatched stuck day-cut shards",
								zap.String("cutDate", cutDate),
								zap.Int("runID", runID),
								zap.Int("count", count))
						}
					}
				}
				logger.Info("startup resume: finished scanning for stuck day-cut shards",
					zap.Strings("cutDates", dates),
					zap.Duration("stuckThreshold", stuckThreshold))
			}()
			return nil
		},
	})
}

// StartDayCutScheduler 启动后台 DayCutScheduler。每 30s 检查 system_config 决定
// 是否触发当天的日切。OnStop 取消 ctx 让循环优雅退出。
//
// OnStart 时还会一次性 seed day_cut.* 4 个 key 到 system_config（已存在的不覆盖），
// 这样新部署 / 老部署都能在 /system-config 页面立即看到这些 key 的默认值。
// StartDayCutScheduler 多 pod leader-gated。
//
// 正确性：日切扫描 / 快照入唯一索引 (account_no, cut_date) 是幂等的，
// 但多 pod 同时跑会重复扫所有账户，浪费 IO 且可能延迟其它在线请求。
// leader election 限制全集群只一个跑。
//
// 注意：SeedDayCutDefaults（一次性 seed system_config 默认值）必须每 pod 都跑，
// 不要 leader-gate（system_config 表层 ON DUPLICATE KEY 幂等，多 pod 跑无害）。
func StartDayCutScheduler(lc fx.Lifecycle, sched *service.DayCutScheduler, cfgSvc service.SystemConfigService, cli *clientv3.Client, logger *zap.Logger) {
	ctx, cancel := context.WithCancel(context.Background())
	id := hostnameOrUnknown()
	lc.Append(fx.Hook{
		OnStart: func(startupCtx context.Context) error {
			// seed 是幂等 INSERT IGNORE 系统配置，每 pod 都跑无害且必要（每个新副本启动都需要）。
			service.SeedDayCutDefaults(startupCtx, cfgSvc, logger)
			go serviceregistry.RunLeaderLoop(ctx, cli,
				"/leader/accounting-system/day-cut-scheduler",
				id, 10*time.Second,
				func(leaderCtx context.Context) {
					logger.Info("day cut scheduler elected leader", zap.String("identity", id))
					sched.Run(leaderCtx)
					logger.Info("day cut scheduler leadership released", zap.String("identity", id))
				})
			logger.Info("day cut scheduler registered (leader-gated)")
			return nil
		},
		OnStop: func(_ context.Context) error {
			cancel()
			return nil
		},
	})
}

func StartOutboxBackpressureController(lc fx.Lifecycle, srv *grpcserver.Server, v *viper.Viper, logger *zap.Logger) {
	originalMax := int64(v.GetInt("server.load_shed.max_inflight"))
	if originalMax <= 0 {
		logger.Info("outbox backpressure disabled (load_shed.max_inflight = 0)")
		return
	}
	highThreshold := int64(v.GetInt("outbox_backpressure.high_threshold"))
	if highThreshold <= 0 {
		highThreshold = 5000
	}
	lowThreshold := int64(v.GetInt("outbox_backpressure.low_threshold"))
	if lowThreshold <= 0 {
		lowThreshold = 1000
	}
	shrinkRatio := v.GetFloat64("outbox_backpressure.shrink_ratio")
	if shrinkRatio <= 0 || shrinkRatio >= 1 {
		shrinkRatio = 0.5
	}
	shrunkMax := int64(float64(originalMax) * shrinkRatio)
	if shrunkMax < 1 {
		shrunkMax = 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			go func() {
				ticker := time.NewTicker(5 * time.Second)
				defer ticker.Stop()
				shrunk := false
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						pending := readGaugeValueLocal(metrics.OutboxPendingGauge)
						if !shrunk && pending > float64(highThreshold) {
							srv.SetMaxInflight(shrunkMax)
							shrunk = true
							logger.Warn("outbox backpressure ENGAGED: shrinking max_inflight",
								zap.Float64("pending", pending),
								zap.Int64("highThreshold", highThreshold),
								zap.Int64("from", originalMax),
								zap.Int64("to", shrunkMax),
							)
						} else if shrunk && pending < float64(lowThreshold) {
							srv.SetMaxInflight(originalMax)
							shrunk = false
							logger.Info("outbox backpressure RELEASED: restoring max_inflight",
								zap.Float64("pending", pending),
								zap.Int64("lowThreshold", lowThreshold),
								zap.Int64("restored", originalMax),
							)
						}
					}
				}
			}()
			logger.Info("outbox backpressure controller started",
				zap.Int64("originalMax", originalMax),
				zap.Int64("shrunkMax", shrunkMax),
				zap.Int64("highThreshold", highThreshold),
				zap.Int64("lowThreshold", lowThreshold))
			return nil
		},
		OnStop: func(_ context.Context) error {
			cancel()
			return nil
		},
	})
}

func readGaugeValueLocal(g prometheus.Gauge) float64 {
	var m promdto.Metric
	if err := g.Write(&m); err != nil {
		return 0
	}
	return m.GetGauge().GetValue()
}

func StartTccRecovery(lc fx.Lifecycle, worker *service.TccRecoveryWorker, cli *clientv3.Client, logger *zap.Logger) {
	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			// Leader election：多 pod 下只能有一个 pod 跑 recovery worker。
			// 否则两个 pod 都扫 stuck TRYING branches，可能在 caller 还在
			// 迭代 Confirm 多 branch 期间抢先 Cancel 未确认的 branch，
			// 导致同一笔 TCC 事务的双分录 entry 一半 confirmed 一半 cancelled
			// → 试算不平 (Trial balance imbalance)。
			//
			// cli 为 nil（registry.endpoints 没配）时 RunLeaderLoop 退化为
			// 直接跑 task，单 pod / dev 模式无感。
			id := hostnameOrUnknown()
			go serviceregistry.RunLeaderLoop(ctx, cli,
				"/leader/accounting-system/tcc-recovery",
				id, 10*time.Second,
				func(leaderCtx context.Context) {
					logger.Info("tcc recovery worker elected leader",
						zap.String("identity", id))
					worker.Start(leaderCtx)
					worker.Wait()
					logger.Info("tcc recovery worker leadership released",
						zap.String("identity", id))
				})
			logger.Info("tcc recovery worker registered (leader-gated; scans every 30s)")
			return nil
		},
		OnStop: func(stopCtx context.Context) error {
			cancel()
			waitWithDeadline(stopCtx, "tcc_recovery", logger, worker.Wait)
			return nil
		},
	})
}

func hostnameOrUnknown() string {
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return "unknown"
}

func NewTccArchiveWorkerFromConfig(v *viper.Viper, router *sharding.Router, tccRepo repository.TccRepository, logger *zap.Logger) *service.TccArchiveWorker {
	cfg := service.TccArchiveConfig{}
	if s := v.GetInt("tcc_archive.interval_seconds"); s > 0 {
		cfg.Interval = time.Duration(s) * time.Second
	}
	if d := v.GetInt("tcc_archive.retention_days"); d > 0 {
		cfg.Retention = time.Duration(d) * 24 * time.Hour
	}
	if b := v.GetInt("tcc_archive.batch_size"); b > 0 {
		cfg.BatchSize = b
	}
	return service.NewTccArchiveWorkerWithConfig(router, tccRepo, logger, cfg)
}

// StartFreezeCompensateOutboxWorker 兜底 UnfreezeAndDebit Phase 2 失败 +
// inline 补偿也失败的场景，保证最终一致性。leader-gated 避免多 pod 并发对
// 同一 voucher 跑两次 compensate（虽然 ClaimPending CAS 已能挡，但减少 DB
// 锁竞争）。
func StartFreezeCompensateOutboxWorker(lc fx.Lifecycle, w *service.FreezeCompensateOutboxWorker, cli *clientv3.Client, logger *zap.Logger) {
	ctx, cancel := context.WithCancel(context.Background())
	id := hostnameOrUnknown()
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			go serviceregistry.RunLeaderLoop(ctx, cli,
				"/leader/accounting-system/freeze-compensate",
				id, 10*time.Second,
				func(leaderCtx context.Context) {
					logger.Info("freeze compensate worker elected leader", zap.String("identity", id))
					w.Start(leaderCtx)
					w.Wait()
					logger.Info("freeze compensate worker leadership released", zap.String("identity", id))
				})
			logger.Info("freeze compensate outbox worker registered (leader-gated)")
			return nil
		},
		OnStop: func(_ context.Context) error {
			cancel()
			return nil
		},
	})
}

// StartIntegrityCheckScheduler 每日跑一次完整账整性扫描（cut_date 一致性 +
// 同 voucher 借贷恒等）。第一次扫紧随启动 60s 跑（让上线后立刻能发现历史
// 脏数据），之后每 24h 跑一次。
//
// 多 pod 下用 etcd leader election 保证只有一个 pod 在跑，避免重复扫表压垮 DB。
func StartIntegrityCheckScheduler(lc fx.Lifecycle, w *service.IntegrityCheckWorker, cli *clientv3.Client, logger *zap.Logger) {
	ctx, cancel := context.WithCancel(context.Background())
	id := hostnameOrUnknown()
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			go serviceregistry.RunLeaderLoop(ctx, cli,
				"/leader/accounting-system/integrity-check",
				id, 10*time.Second,
				func(leaderCtx context.Context) {
					logger.Info("integrity check scheduler elected leader", zap.String("identity", id))
					// 启动 60s 后跑第一次（避免与启动期初始化竞争）
					select {
					case <-leaderCtx.Done():
						return
					case <-time.After(60 * time.Second):
					}
					if _, _, err := w.Run(leaderCtx); err != nil && leaderCtx.Err() == nil {
						logger.Error("integrity scan failed", zap.Error(err))
					}
					ticker := time.NewTicker(24 * time.Hour)
					defer ticker.Stop()
					for {
						select {
						case <-leaderCtx.Done():
							return
						case <-ticker.C:
							if _, _, err := w.Run(leaderCtx); err != nil && leaderCtx.Err() == nil {
								logger.Error("integrity scan failed", zap.Error(err))
							}
						}
					}
				})
			return nil
		},
		OnStop: func(_ context.Context) error {
			cancel()
			return nil
		},
	})
}

// StartTccArchive 多 pod leader-gated。
//
// 正确性：archive 是 DELETE WHERE created_at < threshold AND status IN
// (CONFIRMED, CANCELLED) 幂等操作，多 pod 跑结果相同；但每 pod 都扫 100
// 张分表 + DELETE 会撞行锁拖累在线请求。leader election 让全集群只一个跑。
func StartTccArchive(lc fx.Lifecycle, worker *service.TccArchiveWorker, cli *clientv3.Client, logger *zap.Logger) {
	ctx, cancel := context.WithCancel(context.Background())
	id := hostnameOrUnknown()
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			go serviceregistry.RunLeaderLoop(ctx, cli,
				"/leader/accounting-system/tcc-archive",
				id, 10*time.Second,
				func(leaderCtx context.Context) {
					logger.Info("tcc archive worker elected leader", zap.String("identity", id))
					worker.Start(leaderCtx)
					worker.Wait()
					logger.Info("tcc archive worker leadership released", zap.String("identity", id))
				})
			logger.Info("tcc archive worker registered (leader-gated; archive every 6h)")
			return nil
		},
		OnStop: func(stopCtx context.Context) error {
			cancel()
			waitWithDeadline(stopCtx, "tcc_archive", logger, worker.Wait)
			return nil
		},
	})
}

func SetupHotPath(lc fx.Lifecycle, enabler service.HotPathEnabler,
	accountingSvc service.AccountingService,
	accountRepo repository.AccountRepository,
	transactionRepo repository.TransactionRepository,
	orderRepo repository.TransactionOrderRepository,
	outboxRepo repository.SettlementOutboxRepository,
	hotAccountRepo repository.HotAccountRepository,
	bufferAccountRepo repository.BufferAccountRepository,
	dbManager *database.Manager,
	router *sharding.Router,
	cutDateProvider service.CutDateProvider,
	v *viper.Viper,
	logger *zap.Logger,
) error {
	startupCtx := context.Background()
	if bufCnt, bufErr := accountingSvc.ReloadBufferAccountConfig(startupCtx); bufErr != nil {
		logger.Warn("startup: load buffer account config failed (non-fatal)", zap.Error(bufErr))
	} else {
		logger.Info("startup: buffer account config loaded", zap.Int("count", bufCnt))
	}
	if !v.GetBool("hot_path.enabled") {
		logger.Info("hot_path disabled: using parallel TCC mode (~30K TPS)")
		return nil
	}
	var redisCfg cache.RedisConfig
	if err := v.UnmarshalKey("redis", &redisCfg); err != nil {
		return err
	}
	rdb, err := cache.NewRedisClient(redisCfg, logger)
	if err != nil {
		return err
	}
	balanceCache := cache.NewBalanceCache(rdb, logger)
	brokers := v.GetStringSlice("kafka.brokers")
	var settlementProducer *kafkamq.Producer
	if len(brokers) > 0 {
		settlementTopic := v.GetString("kafka.topics.settlement")
		batchTimeout := v.GetDuration("kafka.producer.batch_timeout")
		if batchTimeout == 0 {
			batchTimeout = 5 * time.Millisecond
		}
		settlementProducer = kafkamq.NewProducer(kafkamq.ProducerConfig{
			Brokers:      brokers,
			Topic:        settlementTopic,
			BatchSize:    v.GetInt("kafka.producer.batch_size"),
			BatchTimeout: batchTimeout,
			MaxAttempts:  v.GetInt("kafka.producer.max_attempts"),
		}, logger)
	}
	allowlist, dbErr := hotAccountRepo.LoadEnabledAccounts(startupCtx)
	if dbErr != nil {
		logger.Warn("hot_path: load allowlist from DB failed, falling back to config file",
			zap.Error(dbErr))
		allowlist = v.GetStringSlice("hot_path.account_allowlist")
	} else if len(allowlist) == 0 {
		cfgList := v.GetStringSlice("hot_path.account_allowlist")
		if len(cfgList) > 0 {
			allowlist = cfgList
			logger.Info("hot_path: DB allowlist empty, using config file allowlist",
				zap.Int("count", len(allowlist)))
		}
	}
	if len(allowlist) == 0 {
		logger.Warn("hot_path enabled but allowlist is empty: no accounts will use hot path")
	}
	outboxWorker := service.NewOutboxWorker(
		outboxRepo,
		orderRepo,
		transactionRepo,
		dbManager,
		router,
		balanceCache,
		cutDateProvider,
		logger,
	)
	workerCtx, workerCancel := context.WithCancel(context.Background())
	enabler.EnableHotPath(workerCtx, balanceCache, settlementProducer, allowlist)
	if len(allowlist) > 0 {
		go warmupHotAccounts(workerCtx, allowlist, accountRepo, balanceCache, router, logger)
	}
	var outboxNotifyProducer *kafkamq.Producer
	if v.GetBool("outbox_push.enabled") && len(brokers) > 0 {
		pushTopic := v.GetString("outbox_push.topic")
		if pushTopic == "" {
			pushTopic = "accounting-outbox-notify"
		}
		pushGroup := v.GetString("outbox_push.consumer_group")
		if pushGroup == "" {
			pushGroup = "accounting-outbox-worker"
		}
		outboxNotifyProducer = kafkamq.NewProducer(kafkamq.ProducerConfig{
			Brokers:      brokers,
			Topic:        pushTopic,
			BatchSize:    1,
			BatchTimeout: 1 * time.Millisecond,
			MaxAttempts:  3,
		}, logger)
		if svc, ok := enabler.(interface{ SetOutboxNotifier(p *kafkamq.Producer) }); ok {
			svc.SetOutboxNotifier(outboxNotifyProducer)
		}
		outboxWorker.StartKafkaConsumer(workerCtx, kafkamq.ConsumerConfig{
			Brokers:        brokers,
			Topic:          pushTopic,
			GroupID:        pushGroup,
			MinBytes:       1,
			MaxBytes:       10 << 20,
			MaxWait:        200 * time.Millisecond,
			CommitInterval: time.Second,
		})
		logger.Info("outbox push enabled",
			zap.String("topic", pushTopic),
			zap.String("group", pushGroup))
	}
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			outboxWorker.Start(workerCtx)
			logger.Info("hot_path enabled: Redis + MySQL Outbox (~150K-200K TPS, 100% fund safe)")
			return nil
		},
		OnStop: func(stopCtx context.Context) error {
			workerCancel()
			waitWithDeadline(stopCtx, "outbox_worker", logger, outboxWorker.Wait)
			if settlementProducer != nil {
				_ = settlementProducer.Close()
			}
			if outboxNotifyProducer != nil {
				_ = outboxNotifyProducer.Close()
			}
			return rdb.Close()
		},
	})
	return nil
}

func warmupHotAccounts(parentCtx context.Context, allowlist []string,
	accountRepo repository.AccountRepository, balanceCache *cache.BalanceCache,
	router *sharding.Router, logger *zap.Logger) {
	ctx, cancel := context.WithTimeout(parentCtx, 30*time.Second)
	defer cancel()
	start := time.Now()
	type shardKey struct{ db, tbl int }
	buckets := make(map[shardKey][]string, 16)
	for _, accNo := range allowlist {
		dbIdx, tblIdx := router.RouteByAccountNo(accNo)
		k := shardKey{dbIdx, tblIdx}
		buckets[k] = append(buckets[k], accNo)
	}
	var warmed, failed atomic.Int64
	var wg sync.WaitGroup
	for k, accNos := range buckets {
		k, accNos := k, accNos
		wg.Add(1)
		go func() {
			defer wg.Done()
			accounts, err := accountRepo.GetAccountsByNos(ctx, accNos, k.db, k.tbl)
			if err != nil {
				logger.Warn("warmup: GetAccountsByNos failed",
					zap.Int("dbIndex", k.db), zap.Int("tableIndex", k.tbl),
					zap.Int("count", len(accNos)), zap.Error(err))
				failed.Add(int64(len(accNos)))
				metrics.WarmAccountFailuresTotal.Add(float64(len(accNos)))
				return
			}
			for _, acc := range accounts {
				categoryInt := 2
				if commonutil.IsAssetOrExpense(acc.AccountCategory) {
					categoryInt = 1
				}
				info := cache.BalanceInfo{
					AccountNo: acc.AccountNo,
					Balance:   strconv.FormatInt(acc.Balance, 10),
					Available: strconv.FormatInt(acc.AvailableBalance, 10),
					Frozen:    strconv.FormatInt(acc.FrozenBalance, 10),
					Version:   acc.Version,
					Category:  categoryInt,
					Status:    int(acc.Status),
				}
				if err := balanceCache.WarmAccount(ctx, info); err != nil {
					logger.Warn("warmup: WarmAccount failed",
						zap.String("accountNo", acc.AccountNo), zap.Error(err))
					failed.Add(1)
					metrics.WarmAccountFailuresTotal.Inc()
					continue
				}
				warmed.Add(1)
			}
		}()
	}
	wg.Wait()
	logger.Info("hot account warmup finished",
		zap.Int("allowlistSize", len(allowlist)),
		zap.Int64("warmed", warmed.Load()),
		zap.Int64("failed", failed.Load()),
		zap.Duration("duration", time.Since(start)),
	)
}

// NewConfigCenterClient 启动期同步 Bind 全平台 config-center；失败 fail-fast。
//
// **替代**：原本 system_config 表 + 山寨 reload 机制；现在用 SDK 走 HTTP+SSE
// 跟 packages/config-center 服务对接，本地 cache 双版本（active + pending）+
// atomic.Pointer 零锁读。
//
// config-center 不可达：
//   - dev: 只 warn，业务用 GetXxx default 兜底（service 层都接受 def 参数）
//   - prod: 拒绝启动（防服务带空 cache 上线读 default 行为不符合预期）
//
// 配置（yaml 或 env）：
//   configcenter.endpoint    = "http://config-center:9691"
//   configcenter.namespace   = "accounting-system"
//   configcenter.instance_id = HOSTNAME / POD_NAME
func NewConfigCenterClient(v *viper.Viper, logger *zap.Logger) (*configcenter.Client, error) {
	endpoint := v.GetString("configcenter.endpoint")
	if endpoint == "" {
		endpoint = "http://config-center:9691"
	}
	namespace := v.GetString("configcenter.namespace")
	if namespace == "" {
		namespace = "accounting-system"
	}
	instanceID := v.GetString("configcenter.instance_id")
	if instanceID == "" {
		instanceID, _ = os.Hostname()
	}
	rpc := configcenter.NewHTTPClient(endpoint, nil)
	cli, err := configcenter.NewWithRPC(rpc, configcenter.Config{
		Namespace:        namespace,
		InstanceID:       instanceID,
		ReconnectBackoff: 1 * time.Second,
		InitTimeout:      10 * time.Second,
		Logger:           logger,
	})
	if err != nil {
		env := strings.ToLower(strings.TrimSpace(v.GetString("env")))
		if env == "prod" || env == "production" {
			return nil, fmt.Errorf("config-center init failed (prod fail-fast): %w", err)
		}
		logger.Warn("config-center init failed; dev mode degrades to all-defaults", zap.Error(err))
		// dev：返一个 nil-cli wrapper；service 层 nil-check 走 def
		return nil, nil
	}
	logger.Info("config-center connected",
		zap.String("endpoint", endpoint),
		zap.String("namespace", namespace),
		zap.String("instance_id", instanceID))
	return cli, nil
}

// NewReconcileWorkerFromConfig 从配置构造 reconcile worker（间隔、endpoint 等可配）
func NewReconcileWorkerFromConfig(
	v *viper.Viper,
	outboxRepo repository.SettlementOutboxRepository,
	dbManager *database.Manager,
	router *sharding.Router,
	logger *zap.Logger,
) *reconcile.ReconcileWorker {
	w := reconcile.NewReconcileWorker(outboxRepo, dbManager, router, logger)

	// 从 config-center key=accounting-system/reconcile.interval 或 yaml 读取间隔
	if intervalSec := v.GetInt("reconcile.interval_seconds"); intervalSec > 0 {
		w.Interval = time.Duration(intervalSec) * time.Second
	}
	if staleHours := v.GetInt("reconcile.stale_threshold_hours"); staleHours > 0 {
		w.StaleThresh = time.Duration(staleHours) * time.Hour
	}

	// order-core endpoint（从 service registry / config 拉）
	if endpoint := v.GetString("reconcile.order_core_endpoint"); endpoint != "" {
		w.SetOrderCoreEndpoint(endpoint)
	}

	return w
}

// StartReconcileWorker 启动 reconcile worker（leader-gated，保证只一个 pod 跑）
func StartReconcileWorker(lc fx.Lifecycle, w *reconcile.ReconcileWorker, cli *clientv3.Client, logger *zap.Logger) {
	ctx, cancel := context.WithCancel(context.Background())
	id := hostnameOrUnknown()
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			go serviceregistry.RunLeaderLoop(ctx, cli,
				"/leader/accounting-system/reconcile",
				id, 10*time.Second,
				func(leaderCtx context.Context) {
					logger.Info("reconcile worker elected leader", zap.String("identity", id))
					w.Start(leaderCtx)
					w.Wait()
					logger.Info("reconcile worker leadership released", zap.String("identity", id))
				})
			logger.Info("reconcile worker registered (leader-gated; scans every 5min by default)")
			return nil
		},
		OnStop: func(_ context.Context) error {
			cancel()
			return nil
		},
	})
}
