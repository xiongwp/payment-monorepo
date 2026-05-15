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
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"github.com/xiongwp/payment-util/mtls"
	"github.com/xiongwp/payment-util/serviceregistry"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	kmsv1 "github.com/xiongwp/kms-manage/api/proto/kms/v1"
	orderv1 "github.com/xiongwp/order-core/api/proto/order/v1"
	paymentcorev1 "github.com/xiongwp/payment-core/api/proto/paymentcore/v1"
	riskv1 "github.com/xiongwp/risk-manage/api/proto/risk/v1"
	usermerchantv1 "github.com/xiongwp/user-merchant-core/api/proto/usermerchant/v1"

	"github.com/xiongwp/payment-admin-web/backend/internal/clients"
	"github.com/xiongwp/payment-admin-web/backend/internal/handler"
)

func main() {
	orderAddr := envOrDefault("ORDER_GRPC_ADDR", "127.0.0.1:9091")
	paymentCoreAddr := envOrDefault("PAYMENT_CORE_GRPC_ADDR", "127.0.0.1:9090")
	kmsAddr := envOrDefault("KMS_GRPC_ADDR", "127.0.0.1:9290")
	riskAddr := envOrDefault("RISK_GRPC_ADDR", "127.0.0.1:9490")
	userMerchantAddr := envOrDefault("USER_MERCHANT_GRPC_ADDR", "127.0.0.1:9191")
	port := envOrDefault("PORT", "9190")

	// REGISTRY_ENDPOINTS（逗号分隔 etcd:2379,...）：BFF 通过 etcd resolver
	// 拨号到所有副本 + round_robin LB；空就退回直连各 *_GRPC_ADDR 环境变量。
	registry := splitCSV(os.Getenv("REGISTRY_ENDPOINTS"))

	orderConn := mustDial(registry, "order-core", orderAddr)
	paymentConn := mustDial(registry, "payment-core", paymentCoreAddr)
	kmsConn := mustDial(registry, "kms-manage", kmsAddr)
	riskConn := mustDial(registry, "risk-manage", riskAddr)
	userMerchantConn := mustDial(registry, "user-merchant-core", userMerchantAddr)
	defer orderConn.Close()
	defer paymentConn.Close()
	defer kmsConn.Close()
	defer riskConn.Close()
	defer userMerchantConn.Close()

	deps := clients.Deps{
		PI:              orderv1.NewPaymentIntentServiceClient(orderConn),
		Charge:          orderv1.NewChargeServiceClient(orderConn),
		Refund:          orderv1.NewRefundServiceClient(orderConn),
		Merchant:        usermerchantv1.NewMerchantServiceClient(userMerchantConn),
		Audit:           orderv1.NewAuditServiceClient(orderConn),
		WebhookDelivery: orderv1.NewWebhookDeliveryServiceClient(orderConn),
		Ledger:          orderv1.NewLedgerServiceClient(orderConn),
		Dispute:         orderv1.NewDisputeServiceClient(orderConn),
		MerchantSecret:    usermerchantv1.NewMerchantSecretServiceClient(userMerchantConn),
		UserMerchantAudit: usermerchantv1.NewAuditServiceClient(userMerchantConn),
		PCore:           paymentcorev1.NewPaymentCoreServiceClient(paymentConn),
		KMS:             kmsv1.NewKMSServiceClient(kmsConn),
		Risk:            riskv1.NewRiskServiceClient(riskConn),
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
		// Close gRPC conns so downstream balancers stop routing to us.
		for name, c := range map[string]*grpc.ClientConn{
			"order-core": orderConn, "payment-core": paymentConn,
			"kms-manage": kmsConn, "risk-manage": riskConn,
			"user-merchant-core": userMerchantConn,
		} {
			if err := c.Close(); err != nil {
				log.Printf("close %s: %v", name, err)
			}
		}
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

// mustDial 优先走 etcd resolver（多副本场景）；endpoints 空就退回直连 fallbackAddr。
// fallbackAddr 用于本地 dev / 单实例部署 / etcd 故障兜底。
//
// 启动期还会做一道**注册探测**：REGISTRY_ENDPOINTS 配了，但 etcd 上 0 个
// <service>/* 注册条目时，自动降级到 fallbackAddr 直连，避免出现 round_robin
// balancer "no children to pick from" 的红错（kms-manage / risk-manage 等
// 容器还没起 / 起来但还没注册就常踩这个）。等 service 真注册了，下次 BFF
// 重启会自动切回 etcd resolver 模式（也可挂热重载，目前先 boot-time 兜底）。
//
// mTLS 模式：MTLS_SERVER_CERT/KEY/CA 配了 → 使用 mTLS credentials；
// 缺配或 INSECURE_DIAL=1（dev only）→ insecure mode。
// 生产必须有证书，否则 panic。
func mustDial(registry []string, service, fallbackAddr string) *grpc.ClientConn {
	// Load mTLS config; fail-fast in production if certs missing
	mtlsCfg, err := mtls.LoadFromEnv()
	if err != nil {
		log.Fatalf("mtls config: %v", err)
	}

	var creds grpc.DialOption
	if mtlsCfg.InsecureDev || (mtlsCfg.ServerCertPath == "" && mtlsCfg.ServerKeyPath == "" && mtlsCfg.CACertPath == "") {
		// Dev/test mode: no mTLS certs configured
		creds = grpc.WithTransportCredentials(insecure.NewCredentials())
	} else {
		// mTLS mode: load credentials
		tlsCreds, cerr := mtlsCfg.ClientCredentials()
		if cerr != nil {
			log.Fatalf("failed to load mTLS credentials for %s: %v", service, cerr)
		}
		creds = grpc.WithTransportCredentials(tlsCreds)
	}

	keepalive := grpc.WithKeepaliveParams(keepalive.ClientParameters{
		Time:                30 * time.Second,
		Timeout:             10 * time.Second,
		PermitWithoutStream: true,
	})

	if len(registry) > 0 {
		// 探测 etcd 上有没有 <service>/* 注册条目；2s 超时不挡 BFF 启动。
		registered, err := serviceregistry.HasRegisteredInstances(registry, service, 2*time.Second)
		if err != nil {
			log.Printf("[bff] WARN probe etcd for %q failed: %v；继续按 fallback 直连", service, err)
		}
		if !registered {
			log.Printf("[bff] WARN %q etcd 无注册条目，降级直连 %s（待该服务起来并注册到 etcd 后重启 BFF 会自动切回 etcd resolver）",
				service, fallbackAddr)
			target := fallbackAddr
			if !strings.Contains(target, "://") {
				target = "dns:///" + target
			}
			conn, derr := grpc.NewClient(target,
				creds,
				keepalive,
				grpc.WithDefaultServiceConfig(`{
					"loadBalancingConfig":[{"round_robin":{}}],
					"healthCheckConfig":{"serviceName":""},
					"methodConfig":[{
						"name":[{}],
						"retryPolicy":{
							"maxAttempts":3,
							"initialBackoff":"0.1s",
							"maxBackoff":"1s",
							"backoffMultiplier":2,
							"retryableStatusCodes":["UNAVAILABLE"]
						}
					}]
				}`),
			)
			if derr != nil {
				log.Fatalf("dial %s fallback (%s): %v", service, target, derr)
			}
			return conn
		}
		conn, err := serviceregistry.DialFromEndpoints(registry, service,
			creds,
			keepalive,
		)
		if err != nil {
			log.Fatalf("dial %s via etcd %v: %v", service, registry, err)
		}
		log.Printf("[bff] dialed %s via etcd %v", service, registry)
		return conn
	}
	// dns 显式前缀触发 gRPC 内置 DNS resolver；不写默认是 passthrough（不 re-resolve），
	// 后端容器重启 / 副本切换 / 启动顺序错时会卡在 "no children to pick from"。
	target := fallbackAddr
	if !strings.Contains(target, "://") {
		target = "dns:///" + target
	}
	conn, err := grpc.NewClient(target,
		creds,
		keepalive,
		// round_robin 多副本均衡 + 30s DNS re-resolve (resolveNowFreq is internal,
		// 但 idle 连接重建会触发 re-resolve)。配 healthCheck 让 unhealthy backend 自动剔除。
		grpc.WithDefaultServiceConfig(`{
			"loadBalancingConfig":[{"round_robin":{}}],
			"healthCheckConfig":{"serviceName":""},
			"methodConfig":[{
				"name":[{}],
				"retryPolicy":{
					"maxAttempts":3,
					"initialBackoff":"0.1s",
					"maxBackoff":"1s",
					"backoffMultiplier":2,
					"retryableStatusCodes":["UNAVAILABLE"]
				}
			}]
		}`),
	)
	if err != nil {
		log.Fatalf("dial %s (%s): %v", service, target, err)
	}
	return conn
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	out := make([]string, 0, 4)
	for _, p := range strings.Split(s, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
