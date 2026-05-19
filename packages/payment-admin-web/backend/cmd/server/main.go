// Command server 是 payment-admin-web 的 BFF：把一组 gRPC 后端（order-core /
// payment-core / kms-manage）聚合成前端易用的 JSON HTTP。
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"github.com/xiongwp/payment-util/kitexutil"

	"github.com/xiongwp/kms-manage/kitex_gen/kms/v1/kmsservice"
	"github.com/xiongwp/order-core/kitex_gen/order/v1/chargeservice"
	"github.com/xiongwp/order-core/kitex_gen/order/v1/disputeservice"
	"github.com/xiongwp/order-core/kitex_gen/order/v1/ledgerservice"
	orderauditservice "github.com/xiongwp/order-core/kitex_gen/order/v1/auditservice"
	"github.com/xiongwp/order-core/kitex_gen/order/v1/paymentintentservice"
	"github.com/xiongwp/order-core/kitex_gen/order/v1/refundservice"
	"github.com/xiongwp/order-core/kitex_gen/order/v1/webhookdeliveryservice"
	"github.com/xiongwp/payment-core/kitex_gen/paymentcore/v1/paymentcoreservice"
	"github.com/xiongwp/risk-manage/kitex_gen/risk/v1/riskservice"
	"github.com/xiongwp/user-merchant-core/kitex_gen/usermerchant/v1/merchantsecretservice"
	"github.com/xiongwp/user-merchant-core/kitex_gen/usermerchant/v1/merchantservice"
	umauditservice "github.com/xiongwp/user-merchant-core/kitex_gen/usermerchant/v1/auditservice"

	"github.com/xiongwp/payment-admin-web/backend/internal/clients"
	"github.com/xiongwp/payment-admin-web/backend/internal/handler"
)

func main() {
	port := envOrDefault("PORT", "9190")
	// Kitex 切换后, 各下游服务由 *service.NewClient("<svc>") 自行解决
	// (etcd resolver / endpoint 由 Kitex 内置 + REGISTRY_ENDPOINTS 环境变量).
	// 老的 mustDial *grpc.ClientConn 已删.

	deps := clients.Deps{
		PI:              kitexutil.MustKitexClient(paymentintentservice.NewClient("order-core")),
		Charge:          kitexutil.MustKitexClient(chargeservice.NewClient("order-core")),
		Refund:          kitexutil.MustKitexClient(refundservice.NewClient("order-core")),
		Merchant:        kitexutil.MustKitexClient(merchantservice.NewClient("user-merchant-core")),
		Audit:           kitexutil.MustKitexClient(orderauditservice.NewClient("order-core")),
		WebhookDelivery: kitexutil.MustKitexClient(webhookdeliveryservice.NewClient("order-core")),
		Ledger:          kitexutil.MustKitexClient(ledgerservice.NewClient("order-core")),
		Dispute:         kitexutil.MustKitexClient(disputeservice.NewClient("order-core")),
		MerchantSecret:    kitexutil.MustKitexClient(merchantsecretservice.NewClient("user-merchant-core")),
		UserMerchantAudit: kitexutil.MustKitexClient(umauditservice.NewClient("user-merchant-core")),
		PCore:           kitexutil.MustKitexClient(paymentcoreservice.NewClient("payment-core")),
		KMS:             kitexutil.MustKitexClient(kmsservice.NewClient("kms-manage")),
		Risk:            kitexutil.MustKitexClient(riskservice.NewClient("risk-manage")),
	}

	orderH := handler.NewOrderHandler(deps)
	channelH := handler.NewChannelHandler(deps)
	kmsH := handler.NewKMSHandler(deps)
	dashH := handler.NewDashboardHandler(deps)
	appH := handler.NewAppHandler(deps)
	opsH := handler.NewOpsHandler(deps)
	merchantH := handler.NewMerchantHandler(deps)
	auditH := handler.NewAuditHandler(deps)
	whDelivH := handler.NewWebhookDeliveryHandler(deps, deps.WebhookDelivery)
	ledgerH := handler.NewLedgerHandler(deps, deps.Ledger)
	disputeH := handler.NewDisputeHandler(deps, deps.Dispute)
	mchSecretH := handler.NewMerchantSecretHandler(deps, deps.MerchantSecret)
	userMerchantAuditH := handler.NewUserMerchantAuditHandler(deps)
	traceGraphH := handler.NewTraceGraphHandler()
	riskH := handler.NewRiskHandler()

	r := mux.NewRouter()
	api := r.PathPrefix("/api").Subrouter()
	api.Use(
		handler.CORSMiddleware,
		handler.LoggingMiddleware,
		handler.BodyLimitMiddleware,
		handler.RateLimitMiddleware(
			envInt("ADMIN_RATE_RPS", 0),
			envInt("ADMIN_RATE_BURST", 0),
			envInt("ADMIN_RATE_PER_IP_RPS", 0),
			envInt("ADMIN_RATE_PER_IP_BURST", 0),
		),
		handler.AuthMiddleware(os.Getenv("ADMIN_BEARER_TOKEN")),
		handler.AuditMiddleware(deps.Audit),
	)

	// dashboard
	api.HandleFunc("/dashboard/summary", dashH.Summary).Methods("GET", "OPTIONS")

	// orders
	api.HandleFunc("/orders", orderH.List).Methods("GET", "OPTIONS")
	api.HandleFunc("/orders/{id}", orderH.Retrieve).Methods("GET", "OPTIONS")
	api.HandleFunc("/orders/{id}/charges", orderH.ListCharges).Methods("GET", "OPTIONS")
	api.HandleFunc("/orders/{id}/refunds", orderH.ListRefunds).Methods("GET", "OPTIONS")

	// merchants (onboarding + KYC)
	api.HandleFunc("/merchants", merchantH.List).Methods("GET", "OPTIONS")
	api.HandleFunc("/merchants", merchantH.Create).Methods("POST", "OPTIONS")
	api.HandleFunc("/merchants/{id}", merchantH.Get).Methods("GET", "OPTIONS")
	api.HandleFunc("/merchants/{id}", merchantH.Update).Methods("PATCH", "OPTIONS")
	api.HandleFunc("/merchants/{id}/rotate-key", merchantH.RotateAPIKey).Methods("POST", "OPTIONS")
	api.HandleFunc("/merchants/{id}/kyc/{action}", merchantH.KYCTransition).Methods("POST", "OPTIONS")
	api.HandleFunc("/merchants/{id}/documents", merchantH.ListDocuments).Methods("GET", "OPTIONS")
	api.HandleFunc("/merchants/{id}/documents", merchantH.AddDocument).Methods("POST", "OPTIONS")
	api.HandleFunc("/merchants/documents/{doc_id}/review", merchantH.ReviewDocument).Methods("POST", "OPTIONS")
	api.HandleFunc("/merchants/{id}/audits", merchantH.ListAudits).Methods("GET", "OPTIONS")

	// merchant channel secrets (wave G)
	api.HandleFunc("/merchants/{id}/secrets", mchSecretH.List).Methods("GET", "OPTIONS")
	api.HandleFunc("/merchants/{id}/secrets", mchSecretH.Put).Methods("POST", "OPTIONS")
	api.HandleFunc("/merchants/{id}/secrets/{channel}/{field_name}", mchSecretH.Delete).Methods("DELETE", "OPTIONS")

	// audit log (admin actions)
	api.HandleFunc("/audit", auditH.List).Methods("GET", "OPTIONS")
	// user-merchant-core 的独立审计链（每次 mutation 自动写入 + 链式 row_hash）
	api.HandleFunc("/user-merchant/audits", userMerchantAuditH.List).Methods("GET", "OPTIONS")

	// trace_id 关联图: Jaeger spans + Loki logs 聚合成 timeline + dependency graph
	api.HandleFunc("/trace/{trace_id}/graph", traceGraphH.Graph).Methods("GET", "OPTIONS")

	// outbound webhook deliveries (wave C)
	api.HandleFunc("/webhooks/deliveries", whDelivH.List).Methods("GET", "OPTIONS")
	api.HandleFunc("/webhooks/deliveries/{id}/retry", whDelivH.Retry).Methods("POST", "OPTIONS")
	api.HandleFunc("/webhooks/test-send", whDelivH.TestSend).Methods("POST", "OPTIONS")

	// ledger (wave H)
	api.HandleFunc("/ledger/accounts", ledgerH.ListAccounts).Methods("GET", "OPTIONS")
	api.HandleFunc("/ledger/accounts/{id}", ledgerH.GetAccount).Methods("GET", "OPTIONS")
	api.HandleFunc("/ledger/entries", ledgerH.ListEntries).Methods("GET", "OPTIONS")
	api.HandleFunc("/ledger/transactions", ledgerH.ListTransactions).Methods("GET", "OPTIONS")
	api.HandleFunc("/ledger/transactions/{id}", ledgerH.GetTransaction).Methods("GET", "OPTIONS")

	// disputes (wave D)
	api.HandleFunc("/disputes", disputeH.List).Methods("GET", "OPTIONS")
	api.HandleFunc("/disputes/{pi_id}/{id}", disputeH.Get).Methods("GET", "OPTIONS")
	api.HandleFunc("/disputes/{pi_id}/{id}/events", disputeH.Events).Methods("GET", "OPTIONS")
	api.HandleFunc("/disputes/{pi_id}/{id}/evidence", disputeH.SubmitEvidence).Methods("POST", "OPTIONS")
	api.HandleFunc("/disputes/{pi_id}/{id}/concede", disputeH.Concede).Methods("POST", "OPTIONS")
	api.HandleFunc("/disputes/{pi_id}/{id}/cancel", disputeH.Cancel).Methods("POST", "OPTIONS")
	api.HandleFunc("/disputes/{pi_id}/{id}/simulate", disputeH.Simulate).Methods("POST", "OPTIONS")

	// channel routing config (payment-core routes — read-only for now)
	api.HandleFunc("/channels/routes/probe", channelH.ProbeRoute).Methods("POST", "OPTIONS")
	api.HandleFunc("/channels/webhook-test", channelH.WebhookTest).Methods("POST", "OPTIONS")

	// kms-manage
	api.HandleFunc("/kms/keys", kmsH.List).Methods("GET", "OPTIONS")
	api.HandleFunc("/kms/encrypt", kmsH.Encrypt).Methods("POST", "OPTIONS")
	api.HandleFunc("/kms/decrypt", kmsH.Decrypt).Methods("POST", "OPTIONS")

	// 模拟商户 App（收银台下单 + 支付）
	api.HandleFunc("/app/create-intent", appH.CreateIntent).Methods("POST", "OPTIONS")
	api.HandleFunc("/app/confirm", appH.Confirm).Methods("POST", "OPTIONS")
	api.HandleFunc("/app/intent/{id}", appH.RetrieveIntent).Methods("GET", "OPTIONS")

	// ── 运营监控 ──────────────────────────────────────────────
	api.HandleFunc("/ops/health", opsH.HealthOverview).Methods("GET", "OPTIONS")
	api.HandleFunc("/ops/risk/rules", opsH.ListRiskRules).Methods("GET", "OPTIONS")
	api.HandleFunc("/ops/risk/reload", opsH.ReloadRiskRules).Methods("POST", "OPTIONS")
	api.HandleFunc("/ops/webhooks/stats", opsH.WebhookStats).Methods("GET", "OPTIONS")
	api.HandleFunc("/ops/config", opsH.ConfigOverview).Methods("GET", "OPTIONS")

	// ── 风控运营平台（review queue / outcome feedback / decision audit）──
	// 顺序敏感：static path 必须先于 {id} 注册，否则会被 path-var 吞掉
	api.HandleFunc("/risk/reviews/decide", riskH.DecideReview).Methods("POST", "OPTIONS")
	api.HandleFunc("/risk/reviews/claim", riskH.ClaimReview).Methods("POST", "OPTIONS")
	api.HandleFunc("/risk/reviews/release", riskH.ReleaseReview).Methods("POST", "OPTIONS")
	api.HandleFunc("/risk/reviews/escalate", riskH.EscalateReview).Methods("POST", "OPTIONS")
	api.HandleFunc("/risk/reviews/note", riskH.AddReviewNote).Methods("POST", "OPTIONS")
	api.HandleFunc("/risk/reviews/by-assignee", riskH.ReviewsByAssignee).Methods("GET", "OPTIONS")
	api.HandleFunc("/risk/reviews/overdue", riskH.ReviewsOverdue).Methods("GET", "OPTIONS")
	api.HandleFunc("/risk/reviews", riskH.ListReviews).Methods("GET", "OPTIONS")
	api.HandleFunc("/risk/reviews/{id}", riskH.GetReview).Methods("GET", "OPTIONS")
	api.HandleFunc("/risk/outcomes/recent", riskH.RecentOutcomes).Methods("GET", "OPTIONS")
	api.HandleFunc("/risk/outcomes", riskH.RecordOutcome).Methods("POST", "OPTIONS")
	api.HandleFunc("/risk/outcomes/{id}", riskH.GetOutcomes).Methods("GET", "OPTIONS")
	api.HandleFunc("/risk/decisions", riskH.ListDecisions).Methods("GET", "OPTIONS")
	api.HandleFunc("/risk/decisions/search", riskH.DecisionSearch).Methods("GET", "OPTIONS")
	api.HandleFunc("/risk/decisions/explain", riskH.DecisionExplain).Methods("POST", "OPTIONS")
	api.HandleFunc("/risk/audit/chain/verify", riskH.AuditChainVerify).Methods("POST", "OPTIONS")
	api.HandleFunc("/risk/whoami", riskH.Whoami).Methods("GET", "OPTIONS")
	api.HandleFunc("/risk/reviews/decide-bulk", riskH.DecideReviewBulk).Methods("POST", "OPTIONS")
	api.HandleFunc("/risk/extsignal/push", riskH.ExtSignalPush).Methods("POST", "OPTIONS")
	api.HandleFunc("/risk/extsignal/get", riskH.ExtSignalGet).Methods("GET", "OPTIONS")
	api.HandleFunc("/risk/extsignal/stats", riskH.ExtSignalStats).Methods("GET", "OPTIONS")
	api.HandleFunc("/risk/dashboard/summary", riskH.DashboardSummary).Methods("GET", "OPTIONS")
	api.HandleFunc("/risk/dashboard/recall", riskH.DashboardRecall).Methods("GET", "OPTIONS")
	api.HandleFunc("/risk/rules/mode", riskH.RuleSetMode).Methods("POST", "OPTIONS")
	api.HandleFunc("/risk/rules/list", riskH.RulesList).Methods("GET", "OPTIONS")
	api.HandleFunc("/risk/rules/update", riskH.RulesUpdate).Methods("POST", "OPTIONS")
	api.HandleFunc("/risk/rules/delete", riskH.RulesDelete).Methods("POST", "OPTIONS")
	api.HandleFunc("/risk/rules/simulate", riskH.RulesSimulate).Methods("POST", "OPTIONS")
	api.HandleFunc("/risk/rules/export", riskH.RulesExport).Methods("GET", "OPTIONS")
	api.HandleFunc("/risk/rules/import", riskH.RulesImport).Methods("POST", "OPTIONS")
	api.HandleFunc("/risk/rules/audit", riskH.RulesAudit).Methods("GET", "OPTIONS")
	api.HandleFunc("/risk/rules/insights", riskH.RulesInsights).Methods("GET", "OPTIONS")
	api.HandleFunc("/risk/rules/overlap", riskH.RulesOverlap).Methods("GET", "OPTIONS")
	api.HandleFunc("/risk/dashboard/cohort", riskH.DashboardCohort).Methods("GET", "OPTIONS")
	api.HandleFunc("/risk/dashboard/cohort/timeseries", riskH.DashboardCohortTimeseries).Methods("GET", "OPTIONS")
	api.HandleFunc("/risk/mlscore/abtest", riskH.MLScoreABTest).Methods("GET", "OPTIONS")
	api.HandleFunc("/risk/mlscore/override", riskH.MLScoreOverrideGet).Methods("GET", "OPTIONS")
	api.HandleFunc("/risk/mlscore/override/set", riskH.MLScoreOverrideSet).Methods("POST", "OPTIONS")
	api.HandleFunc("/risk/mlscore/override/clear", riskH.MLScoreOverrideClear).Methods("POST", "OPTIONS")
	api.HandleFunc("/risk/mlscore/challengers", riskH.ChallengersList).Methods("GET", "OPTIONS")
	api.HandleFunc("/risk/mlscore/challengers/register", riskH.ChallengerRegister).Methods("POST", "OPTIONS")
	api.HandleFunc("/risk/mlscore/challengers/promote", riskH.ChallengerPromote).Methods("POST", "OPTIONS")
	api.HandleFunc("/risk/mlscore/challengers/drop", riskH.ChallengerDrop).Methods("POST", "OPTIONS")

	// MF-2 + SP-AC-7: Money Flow Designer 通过 gRPC 调 split-payment AdminService.
	moneyflowH := handler.NewMoneyflowHandler()
	api.HandleFunc("/moneyflow/_health", moneyflowH.Health).Methods("GET", "OPTIONS")
	// /api/moneyflow/* (graphs / dry-run) → handler 内部按 path 路由到 gRPC 方法
	api.PathPrefix("/moneyflow/").HandlerFunc(moneyflowH.Proxy)
	// SP-13 Stripe-style 资源 API (accounts / transfers / fees / payouts) 暂时下线:
	// split-payment 转为纯 gRPC 内部服务后, 这些外部 HTTP 端点要么挪到独立服务,
	// 要么走 BFF gRPC bridge 重做. 当前路径直接 404, 等独立 gRPC 服务上线再补.
	// /moneyflow → designer HTML; /moneyflow/resources → 资源管理 (SP-13)
	r.HandleFunc("/moneyflow", moneyflowH.Designer).Methods("GET")
	r.HandleFunc("/moneyflow/", moneyflowH.Designer).Methods("GET")
	r.HandleFunc("/moneyflow/resources", moneyflowH.Resources).Methods("GET")
	r.HandleFunc("/moneyflow/resources/", moneyflowH.Resources).Methods("GET")
	// SP-3D React Flow 重写版
	r.HandleFunc("/moneyflow/v2", moneyflowH.DesignerV2).Methods("GET")
	r.HandleFunc("/moneyflow/v2/", moneyflowH.DesignerV2).Methods("GET")

	// SP-AC-4 accounting meta proxy (Designer picker / Rule 管理页用)
	acctH := handler.NewAccountingMetaHandler()
	api.PathPrefix("/accounting/").HandlerFunc(acctH.Proxy)

	// SP-AC-5 TransactionRule 管理页
	r.HandleFunc("/moneyflow/rules", moneyflowH.Rules).Methods("GET")
	r.HandleFunc("/moneyflow/rules/", moneyflowH.Rules).Methods("GET")

	// health
	r.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	addr := fmt.Sprintf(":%s", port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 14, // 16 KB — admin doesn't need more
	}
	log.Printf("payment-admin-web backend listening on %s", addr)
	log.Printf("  order-core         → %s", orderAddr)
	log.Printf("  payment-core       → %s", paymentCoreAddr)
	log.Printf("  kms-manage         → %s", kmsAddr)
	log.Printf("  risk-manage        → %s", riskAddr)
	log.Printf("  user-merchant-core → %s", userMerchantAddr)

	// wave M: graceful shutdown. SIGINT/SIGTERM initiates a 15s drain; in-flight
	// requests finish or get canceled by context. Prevents K8s rollouts from
	// aborting an admin action mid-flight.
	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errCh:
		log.Fatalf("http server exit: %v", err)
	case sig := <-sigCh:
		log.Printf("received %s — draining for 15s", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("graceful shutdown: %v", err)
		}
		// Kitex client 无显式 Close — resolver / 连接由 Kitex runtime 管.
		log.Printf("shutdown complete")
	}
}

// envInt reads an int from an environment variable or returns the fallback.
// Used for rate-limit knobs; zero/empty/invalid all fall through to defaults
// inside RateLimitMiddleware, so no error handling needed here.
func envInt(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n := 0
	for _, c := range v {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// mustDial / splitCSV 已删 — Kitex 切换后 *_GRPC_ADDR + REGISTRY_ENDPOINTS 环境变量
// 由 Kitex client 自己读取.

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
