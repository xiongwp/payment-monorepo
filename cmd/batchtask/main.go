// cmd/batchtask — 批处理定时任务调度器
//
// 两种运行模式：
//
//  1. 守护模式（默认）：内部 ticker 按配置间隔循环调用各 AdminService RPC
//     用于 Docker / systemd 长驻进程部署。
//     启动: ./accounting-batchtask
//
//  2. 单次模式（--run-once）：执行指定任务一次后退出，由 crond / Linux crontab 按计划调用。
//     启动: ./accounting-batchtask --run-once [--task-type=TYPE] [--batch-size=N] ...
//     示例: ./accounting-batchtask --run-once --task-type=process_async_tasks --batch-size=100
//
// 支持的 task-type:
//   process_async_tasks   — 处理 pending/failed 异步任务
//   day_cut_watchdog      — 检测并恢复卡住的日切分片
//   list_manual_tasks     — 打印需要人工介入的任务清单
//   recover_stuck_tasks   — 将长时间卡在 PROCESSING 的异步任务重置为 FAILED（服务器重启恢复）
//
// 通过 config.yaml batchtask 节配置守护模式参数（grpc_addr、各任务 interval 等）。
// 单次模式下 grpc_addr 从 --grpc-addr 标志或 config.yaml 读取。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os/signal"
	"syscall"
	"time"

	accountingv1 "github.com/xiongwp/accounting-grpc-api/gen/accounting/v1"
	"github.com/spf13/viper"
	"github.com/xiongwp/payment-util/serviceregistry"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"os"
)

// ─── 配置结构 ──────────────────────────────────────────────────────────────────

// TaskConfig holds the configuration for a single scheduled task.
type TaskConfig struct {
	// Type identifies which admin RPC to call.
	// Supported: "process_async_tasks", "day_cut_watchdog", "list_manual_tasks"
	Type string `mapstructure:"type"`

	// IntervalSeconds is how often to call this RPC in daemon mode (default 60).
	IntervalSeconds int `mapstructure:"interval_seconds"`

	// BatchSize applies to process_async_tasks (default 100).
	BatchSize int `mapstructure:"batch_size"`

	// StuckThresholdSeconds applies to day_cut_watchdog (default 300).
	StuckThresholdSeconds int `mapstructure:"stuck_threshold_seconds"`

	// Limit applies to list_manual_tasks (default 50).
	Limit int `mapstructure:"limit"`
}

// BatchTaskConfig is the top-level config for this binary.
type BatchTaskConfig struct {
	GrpcAddr           string       `mapstructure:"grpc_addr"`
	DialTimeoutSeconds int          `mapstructure:"dial_timeout_seconds"`
	Tasks              []TaskConfig `mapstructure:"tasks"`

	// RegistryEndpoints is the etcd cluster for service discovery + leader
	// election. 留空 → 直连 GRPC_ADDR + 不做 leader election（dev / 单 pod 模式）。
	RegistryEndpoints []string `mapstructure:"registry_endpoints"`
	// RegistryService 是要拨号的目标 service 名（默认 accounting-service）。
	RegistryService string `mapstructure:"registry_service"`
	// LeaderTTLSeconds is the etcd session TTL for the leader election (default 10s).
	LeaderTTLSeconds int `mapstructure:"leader_ttl_seconds"`
}

// ─── CLI 标志 ──────────────────────────────────────────────────────────────────

var (
	flagRunOnce       = flag.Bool("run-once", false, "执行一次后退出（由外部 cron 调用）")
	flagTaskType      = flag.String("task-type", "", "单次模式：指定要运行的 task type")
	flagGrpcAddr      = flag.String("grpc-addr", "", "gRPC 服务地址，覆盖 config.yaml")
	flagBatchSize     = flag.Int("batch-size", 0, "process_async_tasks: 每次最多处理的任务数")
	flagStuckThresh   = flag.Int("stuck-threshold", 0, "day_cut_watchdog: 判定卡住的阈值（秒）")
	flagLimit         = flag.Int("limit", 0, "list_manual_tasks: 最多返回的任务数")
	flagDialTimeout   = flag.Int("dial-timeout", 0, "连接 gRPC 服务的超时秒数")
)

// ─── main ─────────────────────────────────────────────────────────────────────

func main() {
	flag.Parse()

	// 读取 config.yaml
	v := viper.New()
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath("./config")
	v.AddConfigPath(".")
	v.AutomaticEnv()
	if err := v.ReadInConfig(); err != nil {
		log.Printf("[batchtask] warn: config file not found (%v), using defaults/flags", err)
	}

	var cfg BatchTaskConfig
	_ = v.UnmarshalKey("batchtask", &cfg)
	// registry 段在 viper 顶层（与 server 共享同一份 config.yaml）；
	// 把它读到 cfg 上，方便后面统一处理。
	if endpoints := v.GetStringSlice("registry.endpoints"); len(endpoints) > 0 {
		cfg.RegistryEndpoints = endpoints
	}
	if svc := v.GetString("registry.service_name"); svc != "" {
		cfg.RegistryService = svc
	}
	if cfg.RegistryService == "" {
		cfg.RegistryService = "accounting-service"
	}
	if ttl := v.GetInt("registry.ttl_seconds"); ttl > 0 {
		cfg.LeaderTTLSeconds = ttl
	}
	if cfg.LeaderTTLSeconds <= 0 {
		cfg.LeaderTTLSeconds = 10
	}

	// CLI 标志覆盖 config
	if *flagGrpcAddr != "" {
		cfg.GrpcAddr = *flagGrpcAddr
	}
	if *flagDialTimeout > 0 {
		cfg.DialTimeoutSeconds = *flagDialTimeout
	}
	if cfg.GrpcAddr == "" {
		cfg.GrpcAddr = "localhost:50051"
	}
	if cfg.DialTimeoutSeconds <= 0 {
		cfg.DialTimeoutSeconds = 10
	}

	// 默认任务配置（守护模式）
	if len(cfg.Tasks) == 0 {
		cfg.Tasks = []TaskConfig{
			{Type: "process_async_tasks", IntervalSeconds: 60, BatchSize: 100},
			{Type: "day_cut_watchdog", IntervalSeconds: 300, StuckThresholdSeconds: 300},
			{Type: "list_manual_tasks", IntervalSeconds: 600, Limit: 50},
			{Type: "recover_stuck_tasks", IntervalSeconds: 300, StuckThresholdSeconds: 300},
		}
	}

	// 连接 gRPC 服务：优先走 etcd resolver（拿所有活副本 + round_robin），
	// 没配 etcd 退回直连 cfg.GrpcAddr 并加 round_robin service config（DNS 多 IP 也能均摊）。
	dialCtx, dialCancel := context.WithTimeout(context.Background(), time.Duration(cfg.DialTimeoutSeconds)*time.Second)
	defer dialCancel()
	conn, err := dialAccountingService(dialCtx, cfg)
	if err != nil {
		log.Fatalf("[batchtask] dial accounting-system failed: %v", err)
	}
	defer conn.Close()

	adminClient := accountingv1.NewAccountingAdminServiceClient(conn)

	if *flagRunOnce {
		runOnce(adminClient, cfg)
		return
	}

	// 多 pod 部署时各 task type 都用 leader election 卡单实例；
	// 不同 type 对应不同 leader key，可在 N 个 batchtask 副本间分布。
	runDaemon(adminClient, cfg)
}

// dialAccountingService dispatches between etcd-resolver and direct dial.
// 在 etcd 模式下不会使用 cfg.GrpcAddr；直连模式下用 cfg.GrpcAddr。
func dialAccountingService(ctx context.Context, cfg BatchTaskConfig) (*grpc.ClientConn, error) {
	if len(cfg.RegistryEndpoints) > 0 {
		log.Printf("[batchtask] dialing %s via etcd %v", cfg.RegistryService, cfg.RegistryEndpoints)
		return serviceregistry.DialFromEndpoints(cfg.RegistryEndpoints, cfg.RegistryService,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
	}
	log.Printf("[batchtask] dialing %s direct (no registry.endpoints)", cfg.GrpcAddr)
	return grpc.DialContext(ctx, cfg.GrpcAddr, //nolint:staticcheck
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithDefaultServiceConfig(`{"loadBalancingConfig":[{"round_robin":{}}]}`),
	)
}

// leaderEtcdClient builds (or returns nil) a shared etcd client for leader
// election. 与 dialAccountingService 共用的 etcd cluster；nil 表示禁用 election。
func leaderEtcdClient(cfg BatchTaskConfig) *clientv3.Client {
	if len(cfg.RegistryEndpoints) == 0 {
		return nil
	}
	// 复用 DialFromEndpoints 留下的进程级 etcd client；如果还没建（dial 失败的边缘情况），
	// 这里不再重建：返回 nil 让 RunLeaderLoop 退化为直接 task。
	return serviceregistry.SharedEtcdClient()
}

// ─── 单次模式 ──────────────────────────────────────────────────────────────────

// runOnce executes the configured task(s) once and exits.
// If --task-type is specified, only that task runs; otherwise all tasks run once.
func runOnce(client accountingv1.AccountingAdminServiceClient, cfg BatchTaskConfig) {
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
	defer cancel()

	for _, tc := range cfg.Tasks {
		// Filter by --task-type if specified
		if *flagTaskType != "" && tc.Type != *flagTaskType {
			continue
		}
		// Apply CLI flag overrides
		if *flagBatchSize > 0 {
			tc.BatchSize = *flagBatchSize
		}
		if *flagStuckThresh > 0 {
			tc.StuckThresholdSeconds = *flagStuckThresh
		}
		if *flagLimit > 0 {
			tc.Limit = *flagLimit
		}
		executeTask(ctx, client, tc)
	}
}

// ─── 守护模式 ──────────────────────────────────────────────────────────────────

// runDaemon starts a goroutine per task and blocks until SIGINT/SIGTERM.
//
// Multi-pod safe: each task is wrapped by serviceregistry.RunLeaderLoop
// keyed by task type. 不同 type 对应不同 leader key，可在 N 个 batchtask
// 副本间天然分布（pod1 接管 process_async_tasks，pod2 接管 day_cut_watchdog 等）；
// 没配 etcd 时 RunLeaderLoop 退化为直接跑（dev / 单 pod 模式）。
func runDaemon(client accountingv1.AccountingAdminServiceClient, cfg BatchTaskConfig) {
	log.Printf("[batchtask] daemon mode: connected to %s, starting %d tasks", cfg.GrpcAddr, len(cfg.Tasks))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	etcdCli := leaderEtcdClient(cfg)
	identity := hostnameOrUnknown()
	ttl := time.Duration(cfg.LeaderTTLSeconds) * time.Second

	for _, tc := range cfg.Tasks {
		tc := tc
		if tc.IntervalSeconds <= 0 {
			tc.IntervalSeconds = 60
		}
		key := "/leader/accounting-batchtask/" + tc.Type
		go serviceregistry.RunLeaderLoop(ctx, etcdCli, key, identity, ttl, func(leaderCtx context.Context) {
			log.Printf("[batchtask] %s elected leader (%s)", tc.Type, identity)
			runTaskLoop(leaderCtx, client, tc)
			log.Printf("[batchtask] %s leadership released", tc.Type)
		})
	}

	<-ctx.Done()
	log.Println("[batchtask] shutting down")
}

func hostnameOrUnknown() string {
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return "unknown"
}

// runTaskLoop runs a single task on its configured interval until ctx is cancelled.
func runTaskLoop(ctx context.Context, client accountingv1.AccountingAdminServiceClient, cfg TaskConfig) {
	interval := time.Duration(cfg.IntervalSeconds) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	log.Printf("[batchtask] starting task type=%s interval=%s", cfg.Type, interval)
	executeTask(ctx, client, cfg) // run immediately

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			executeTask(ctx, client, cfg)
		}
	}
}

// ─── 任务执行 ──────────────────────────────────────────────────────────────────

func executeTask(ctx context.Context, client accountingv1.AccountingAdminServiceClient, cfg TaskConfig) {
	callCtx, cancel := context.WithTimeout(ctx, 55*time.Second)
	defer cancel()

	switch cfg.Type {
	case "process_async_tasks":
		executeProcessAsyncTasks(callCtx, client, cfg)
	case "day_cut_watchdog":
		executeDayCutWatchdog(callCtx, client, cfg)
	case "list_manual_tasks":
		executeListManualTasks(callCtx, client, cfg)
	case "recover_stuck_tasks":
		executeRecoverStuckTasks(callCtx, client, cfg)
	default:
		log.Printf("[batchtask] unknown task type: %s", cfg.Type)
	}
}

func executeProcessAsyncTasks(ctx context.Context, client accountingv1.AccountingAdminServiceClient, cfg TaskConfig) {
	batchSize := int32(cfg.BatchSize)
	if batchSize <= 0 {
		batchSize = 100
	}
	resp, err := client.ProcessAsyncTasks(ctx, &accountingv1.ProcessAsyncTasksRequest{BatchSize: batchSize})
	if err != nil {
		log.Printf("[batchtask][process_async_tasks] RPC error: %v", err)
		return
	}
	if resp.Code != 0 {
		log.Printf("[batchtask][process_async_tasks] error code=%d msg=%s", resp.Code, resp.Message)
		return
	}
	if resp.Processed > 0 {
		log.Printf("[batchtask][process_async_tasks] processed=%d", resp.Processed)
	}
}

func executeDayCutWatchdog(ctx context.Context, client accountingv1.AccountingAdminServiceClient, cfg TaskConfig) {
	threshold := int32(cfg.StuckThresholdSeconds)
	if threshold <= 0 {
		threshold = 300
	}
	resp, err := client.DayCutWatchdog(ctx, &accountingv1.DayCutWatchdogRequest{StuckThresholdSeconds: threshold})
	if err != nil {
		log.Printf("[batchtask][day_cut_watchdog] RPC error: %v", err)
		return
	}
	if resp.Code != 0 {
		log.Printf("[batchtask][day_cut_watchdog] error code=%d msg=%s", resp.Code, resp.Message)
		return
	}
	log.Printf("[batchtask][day_cut_watchdog] ok")
}

func executeListManualTasks(ctx context.Context, client accountingv1.AccountingAdminServiceClient, cfg TaskConfig) {
	limit := int32(cfg.Limit)
	if limit <= 0 {
		limit = 50
	}
	resp, err := client.ListManualTasks(ctx, &accountingv1.ListManualTasksRequest{Limit: limit})
	if err != nil {
		log.Printf("[batchtask][list_manual_tasks] RPC error: %v", err)
		return
	}
	if resp.Code != 0 {
		log.Printf("[batchtask][list_manual_tasks] error code=%d msg=%s", resp.Code, resp.Message)
		return
	}
	if len(resp.Tasks) == 0 {
		return
	}
	fmt.Printf("[batchtask][list_manual_tasks] %d task(s) pending manual processing:\n", len(resp.Tasks))
	for _, t := range resp.Tasks {
		fmt.Printf("  task_id=%-20s type=%-20s business_no=%-30s retry=%d error=%s\n",
			t.TaskId, t.TaskType, t.BusinessNo, t.RetryCount, t.ErrorMessage)
	}
}

func executeRecoverStuckTasks(ctx context.Context, client accountingv1.AccountingAdminServiceClient, cfg TaskConfig) {
	threshold := int32(cfg.StuckThresholdSeconds)
	if threshold <= 0 {
		threshold = 300
	}
	resp, err := client.RecoverStuckTasks(ctx, &accountingv1.RecoverStuckTasksRequest{StuckThresholdSeconds: threshold})
	if err != nil {
		log.Printf("[batchtask][recover_stuck_tasks] RPC error: %v", err)
		return
	}
	if resp.Code != 0 {
		log.Printf("[batchtask][recover_stuck_tasks] error code=%d msg=%s", resp.Code, resp.Message)
		return
	}
	if resp.Recovered > 0 {
		log.Printf("[batchtask][recover_stuck_tasks] recovered=%d stuck PROCESSING tasks", resp.Recovered)
	}
}
