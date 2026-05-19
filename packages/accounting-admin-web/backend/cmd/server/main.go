package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gorilla/mux"
	accountingservice "github.com/xiongwp/accounting-system/kitex_gen/accounting/v1/accountingservice"
	accountingadminservice "github.com/xiongwp/accounting-system/kitex_gen/accounting/v1/accountingadminservice"
	"github.com/xiongwp/accounting-admin-web/backend/internal/handler"
	"github.com/xiongwp/payment-util/kitexutil"
)

func main() {
	grpcAddr := envOrDefault("GRPC_ADDR", "localhost:50051")
	port := envOrDefault("PORT", "9090")
	// Seed HTTP admin address: any one live accounting-system instance's admin port.
	// Used to discover all instances via GET /admin/instances.
	adminHTTPAddr := envOrDefault("ACCOUNTING_ADMIN_HTTP_ADDR", "http://localhost:8888")

	// REGISTRY_ENDPOINTS 非空 → 走 etcd resolver（联栈多 pod 部署必走，因为
	// 容器去掉 container_name 后 "accounting-service" 跨 compose 项目 DNS 不可解析）；
	// 空 → 退回 grpcAddr 直连。两条路径都用 round_robin LB 在多副本间均摊。
	var registry []string
	if env := strings.TrimSpace(os.Getenv("REGISTRY_ENDPOINTS")); env != "" {
		for _, e := range strings.Split(env, ",") {
			if e = strings.TrimSpace(e); e != "" {
				registry = append(registry, e)
			}
		}
	}
	// Kitex client host:port 由 kitexutil.DefaultHostPorts 统一解析
	// (env ACCOUNTING_SYSTEM_GRPC_ADDR > 默认 accounting-system:50051).
	// grpcAddr / registry 旧 env 仅留 log; 实际拨号走 helper.
	_ = registry
	log.Printf("accounting-system gRPC target (legacy env hint): %s", grpcAddr)

	client := kitexutil.MustKitexClient(accountingservice.NewClient("accounting-system",
		kitexutil.DefaultHostPorts("accounting-system"),
	))
	adminClient := kitexutil.MustKitexClient(accountingadminservice.NewClient("accounting-system",
		kitexutil.DefaultHostPorts("accounting-system"),
	))

	// Build handlers
	accountH := handler.NewAccountHandler(client)
	bookingH := handler.NewBookingHandler(client)
	snapshotH := handler.NewSnapshotHandler(client)
	tccH := handler.NewTccHandler(client)
	dayCutH := handler.NewDayCutHandler(client)
	txH := handler.NewTransactionHandler(client)
	adjH := handler.NewAdjustmentHandler(client)
	trialBalanceH := handler.NewTrialBalanceHandler(client)
	adminH := handler.NewAdminHandler(adminClient)
	instanceH := handler.NewInstanceHandler(adminHTTPAddr)
	rebuildH := handler.NewRedisRebuildHandler(client)

	// Register routes
	r := mux.NewRouter()

	// Health
	r.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, "ok")
	}).Methods(http.MethodGet)

	// Accounts
	r.HandleFunc("/v1/accounts", accountH.CreateAccount).Methods(http.MethodPost)
	r.HandleFunc("/v1/accounts", accountH.GetAccount).Methods(http.MethodGet)
	r.HandleFunc("/v1/accounts/{accountNo}", accountH.GetAccountByNo).Methods(http.MethodGet)

	// Bookings
	r.HandleFunc("/v1/bookings", bookingH.DoubleEntryBooking).Methods(http.MethodPost)

	// Snapshots
	r.HandleFunc("/v1/snapshots/{accountNo}", snapshotH.GetBalanceSnapshot).Methods(http.MethodGet)

	// TCC — /stuck must be registered before /{tccId}
	r.HandleFunc("/v1/tcc/stuck", tccH.ListStuckTcc).Methods(http.MethodGet)
	r.HandleFunc("/v1/tcc/branches/{branchId}/cancel", tccH.CancelTccBranch).Methods(http.MethodPost)
	r.HandleFunc("/v1/tcc/{tccId}", tccH.GetTccStatus).Methods(http.MethodGet)
	r.HandleFunc("/v1/tcc/{tccId}/cancel", tccH.CancelTcc).Methods(http.MethodPost)

	// Day-cut
	r.HandleFunc("/v1/day-cut", dayCutH.TriggerDayCut).Methods(http.MethodPost)
	r.HandleFunc("/v1/day-cut/history", dayCutH.GetDayCutHistory).Methods(http.MethodGet)
	r.HandleFunc("/v1/day-cut/resume", instanceH.DayCutResume).Methods(http.MethodPost) // 恢复卡死分片
	r.HandleFunc("/v1/tcc/retry-confirm", instanceH.TccRetryConfirmNow).Methods(http.MethodPost) // 立即重试 CONFIRMING 半挂起 TCC
	r.HandleFunc("/v1/redis/rebuild", rebuildH.Rebuild).Methods(http.MethodPost)                  // 从 MySQL 重建 Redis 热账户余额（gRPC）

	// 系统通用配置中心（meta DB / system_config）
	r.HandleFunc("/v1/config", instanceH.ListSystemConfig).Methods(http.MethodGet)
	r.HandleFunc("/v1/config", instanceH.UpsertSystemConfig).Methods(http.MethodPost)
	r.HandleFunc("/v1/config/reload", instanceH.ReloadSystemConfig).Methods(http.MethodPost)
	r.HandleFunc("/v1/config/{key}", instanceH.DeleteSystemConfig).Methods(http.MethodDelete)

	// Transactions (list query)
	r.HandleFunc("/v1/transactions", txH.ListTransactions).Methods(http.MethodGet)

	// Adjustment
	r.HandleFunc("/v1/adjustment", adjH.AdjustBalance).Methods(http.MethodPost)

	// Trial balance
	r.HandleFunc("/v1/trial-balance", trialBalanceH.RunTrialBalance).Methods(http.MethodPost)
	r.HandleFunc("/v1/trial-balance/dates", trialBalanceH.ListSnapshotDates).Methods(http.MethodGet)

	// Service instances (multi-instance discovery via HTTP admin)
	r.HandleFunc("/v1/service-instances", instanceH.ListInstances).Methods(http.MethodGet)

	// Hot accounts — all operations proxy to accounting-system HTTP admin
	r.HandleFunc("/v1/hot-accounts/reload", instanceH.ReloadHotAccounts).Methods(http.MethodPost)
	r.HandleFunc("/v1/hot-accounts/{id}", instanceH.UpdateHotAccount).Methods(http.MethodPut)
	r.HandleFunc("/v1/hot-accounts/{id}", instanceH.DeleteHotAccount).Methods(http.MethodDelete)
	r.HandleFunc("/v1/hot-accounts", instanceH.ListHotAccounts).Methods(http.MethodGet)
	r.HandleFunc("/v1/hot-accounts", instanceH.CreateHotAccount).Methods(http.MethodPost)

	// Platform accounts (系统内部账户) — proxy to accounting-system HTTP admin
	r.HandleFunc("/v1/platform-accounts", instanceH.CreatePlatformAccount).Methods(http.MethodPost)
	r.HandleFunc("/v1/platform-accounts/fleet", instanceH.CreatePlatformAccountFleet).Methods(http.MethodPost)
	r.HandleFunc("/v1/platform-accounts/balances", instanceH.PlatformAccountBalances).Methods(http.MethodGet)
	r.HandleFunc("/v1/platform-accounts/snapshots", instanceH.PlatformAccountSnapshots).Methods(http.MethodGet)
	r.HandleFunc("/v1/business-types/reload", instanceH.ReloadBusinessTypes).Methods(http.MethodPost)
	r.HandleFunc("/v1/business-types", instanceH.ListBusinessTypes).Methods(http.MethodGet)
	r.HandleFunc("/v1/business-types", instanceH.RegisterBusinessType).Methods(http.MethodPost)
	// Account type registry（含 is_platform 标志，前端做业务/平台过滤用）
	r.HandleFunc("/v1/account-types", instanceH.ListAccountTypes).Methods(http.MethodGet)
	// TCC 归档
	r.HandleFunc("/v1/tcc-archive/config", instanceH.TccArchiveConfig).Methods(http.MethodGet)
	r.HandleFunc("/v1/tcc-archive/run", instanceH.TccArchiveRun).Methods(http.MethodPost)

	// Buffer accounts — reload fans out to all live instances via HTTP admin
	r.HandleFunc("/v1/buffer-accounts/reload", instanceH.ReloadBufferAccounts).Methods(http.MethodPost)
	r.HandleFunc("/v1/transaction-rules/reload", instanceH.ReloadTransactionRules).Methods(http.MethodPost)
	r.HandleFunc("/v1/buffer-accounts/{id}", adminH.UpdateBufferAccount).Methods(http.MethodPut)
	r.HandleFunc("/v1/buffer-accounts/{id}", adminH.DeleteBufferAccount).Methods(http.MethodDelete)
	r.HandleFunc("/v1/buffer-accounts", adminH.ListBufferAccounts).Methods(http.MethodGet)
	r.HandleFunc("/v1/buffer-accounts", adminH.CreateBufferAccount).Methods(http.MethodPost)

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      r,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	log.Printf("HTTP server listening on :%s (gRPC backend: %s)", port, grpcAddr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
