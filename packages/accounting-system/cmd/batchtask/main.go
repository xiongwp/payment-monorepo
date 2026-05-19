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

	kitexclient "github.com/cloudwego/kitex/client"

	accountingv1 "reconcile-system/packages/accounting-system/kitex_gen/accounting/v1"
	accountingadminservice "reconcile-system/packages/accounting-system/kitex_gen/accounting/v1/accountingadminservice"
	"github.com/spf13/viper"
	clientv3 "go.etcd.io/etcd/client/v3"
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

	// 连接 Kitex: accounting-system 切 Kitex 后, 不再走 grpc.ClientConn.
	// Kitex 内部自管 connection pool + round_robin LB, 不需要 round_robin service config.
	adminClient, err := dialAccountingAdmin(cfg)
	if err != nil {
		log.Fatalf("[batchtask] kitex dial accounting-system failed: %v", err)
	}

	if *flagRunOnce {
		runOnce(adminClient, cfg)
		return
	}

	// 多 pod 部署时各 task type 都用 leader election 卡单实例；
	// 不同 type 对应不同 leader key，可在 N 个 batchtask 副本间分布。
	runDaemon(adminClient, cfg)
}

// dialAccountingAdmin Kitex client → accounting-system AccountingAdminService.
//
// etcd resolver TODO (kitexutil.NewEtcdResolver), 当前用 direct addr.
func dialAccountingAdmin(cfg BatchTaskConfig) (accountingadminservice.Client, error) {
	endpoint := cfg.GrpcAddr
	log.Printf("[batchtask] kitex dial %s (endpoint=%s, registry=%v)",
		cfg.RegistryService, endpoint, cfg.RegistryEndpoints)
	return accountingadminservice.NewClient("accounting-system",
		kitexclient.WithHostPorts(endpoint),
		kitexclient.WithRPCTimeout(time.Duration(cfg.DialTimeoutSeconds)*time.Second),
	)
}

// leaderEtcdClient builds (or returns nil) a shared etcd client for leader
// election. 与 dialAccountingService 共用的 etcd cluster；nil 表示禁用 election。
func leaderEtcdClient(cfg BatchTaskConfig) *clientv3.Client {
	if len(cfg.RegistryEndpoints) == 0 {
		return nil
	}
	// TODO: serviceregistry 已退役, leader election etcd client 需要在 main 单独构造
	// (clientv3.New) 再传给本函数. 当前暂返 nil → RunLeaderLoop 退化为直接 task (单 pod OK).
	return nil
}

// ─── 单次模式 ──────────────────────────────────────────────────────────────────

// runOnce executes the configured task(s) once and exits.
// If --task-type is specified, only that task runs; otherwise all tasks run once.
func runOnce(client accountingadminservice.Client, cfg BatchTaskConfig) {
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
func runDaemon(client accountingadminservice.Client, cfg BatchTaskConfig) {
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
func runTaskLoop(ctx context.Context, client accountingadminservice.Client, cfg TaskConfig) {
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

func executeTask(ctx context.Context, client accountingadminservice.Client, cfg TaskConfig) {
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

func executeProcessAsyncTasks(ctx context.Context, client accountingadminservice.Client, cfg TaskConfig) {
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

func executeDayCutWatchdog(ctx context.Context, client accountingadminservice.Client, cfg TaskConfig) {
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

func executeListManualTasks(ctx context.Context, client accountingadminservice.Client, cfg TaskConfig) {
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

func executeRecoverStuckTasks(ctx context.Context, client accountingadminservice.Client, cfg TaskConfig) {
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
