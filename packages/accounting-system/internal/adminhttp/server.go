// Package adminhttp 提供 accounting-system 的 HTTP 内部管理接口。
//
// 用途：多实例部署时，admin-web 通过此接口发现所有实例并定向推送配置变更。
// 与 gRPC 端口分离，仅供内部运维使用，不对外暴露业务 API。
//
// 端点：
//   GET    /admin/instances              返回所有活跃实例列表（供 admin-web 做服务发现）
//   POST   /admin/reload/buffer-accounts 重载本实例的缓冲记账账户配置
//   POST   /admin/reload/hot-accounts    重载本实例的热点账户白名单
//   GET    /admin/health                 健康检查（返回 200 ok）
//
//   GET    /admin/hot-accounts           列出所有热点账户配置
//   POST   /admin/hot-accounts           新增热点账户配置
//   PUT    /admin/hot-accounts/{id}      更新热点账户配置
//   DELETE /admin/hot-accounts/{id}      删除热点账户配置
package adminhttp

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/accounting-system/internal/domain/model"
	"github.com/accounting-system/internal/metrics"
	"github.com/accounting-system/internal/repository"
	"github.com/accounting-system/internal/service"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// subtleConstantTimeCompare 包一层使代码更易读 + 测试。
var subtleConstantTimeCompare = subtle.ConstantTimeCompare

// Server HTTP 内部管理服务器
type Server struct {
	instanceID       string
	host             string
	adminPort        int
	grpcPort         int
	authToken        string // 共享 secret；为空时鉴权关闭（dev/向后兼容）
	instanceRepo     repository.ServiceInstanceRepository
	hotAccountRepo   repository.HotAccountRepository
	ruleRepo         repository.TransactionRuleRepository // 用于 ListAccountTypes
	accountingSvc    service.AccountingService
	dayCutSvc        service.DayCutService                // 用于 /admin/day-cut/resume
	systemConfigSvc  service.SystemConfigService          // wrap config-center SDK；保留 Reload 入口给 ConfigSyncWorker
	tccArchiveWorker *service.TccArchiveWorker            // 支持 admin-web 触发立即归档
	bufferedBalWk    *service.BufferedBalanceWorker       // 支持 /admin/buffered-balance/flush 立即 flush
	pinger           HealthPinger                         // readiness DB ping，可选（nil = 仅检查 draining）
	logger           *zap.Logger
	httpServer       *http.Server

	// draining 由 BeginDrain 设为 true，readiness 探针随后返回 503，K8s/LB 摘流量。
	// 真正退出前需等 metrics.InflightBookingsGauge 归零（WaitDrain）。
	draining atomic.Bool
}

// Config HTTP admin server 配置
type Config struct {
	// Host 本实例的外部可达主机名或 IP（用于注册到 service_instance 表）
	// 若为空，自动使用 os.Hostname()
	Host      string
	AdminPort int // HTTP admin 端口，对应 server.port（默认 8888）
	GRPCPort  int // gRPC 端口，用于注册展示
	// AuthToken 共享 secret token。配置后所有 admin 请求必须带
	//   X-Admin-Token: <token>
	// 头才能通过；缺失或错误返回 401。空 = 关闭鉴权（开发默认）。
	// 优先从 ADMIN_HTTP_TOKEN 环境变量读，没有再 fallback 到此字段。
	AuthToken string
}

// HealthPinger 健康探针接口，被 readiness 探针调用。database.Manager 实现了它。
// 用接口而非具体类型 → adminhttp 不直接 import database 包，保持依赖向下单向。
type HealthPinger interface {
	PingPrimary(ctx context.Context) error
}

// NewServer 创建 HTTP admin 服务器
func NewServer(
	cfg Config,
	instanceRepo repository.ServiceInstanceRepository,
	hotAccountRepo repository.HotAccountRepository,
	ruleRepo repository.TransactionRuleRepository,
	accountingSvc service.AccountingService,
	dayCutSvc service.DayCutService,
	systemConfigSvc service.SystemConfigService,
	tccArchiveWorker *service.TccArchiveWorker,
	bufferedBalWk *service.BufferedBalanceWorker,
	pinger HealthPinger,
	logger *zap.Logger,
) *Server {
	host := cfg.Host
	if host == "" {
		host = resolveOutboundIP()
	}
	instanceID := fmt.Sprintf("%s:%d", host, cfg.AdminPort)

	// token 优先级：环境变量 > yaml 配置。生产部署一般通过 K8s Secret → env 注入，
	// 不应把 secret 嵌进版本控制的 config.yaml。
	authToken := os.Getenv("ADMIN_HTTP_TOKEN")
	if authToken == "" {
		authToken = cfg.AuthToken
	}
	if authToken == "" {
		// 用 Error 级别打：admin HTTP 包含 fleet 创建 / config 改写 / day-cut 触发等
		// 高权限端点。空 token 在生产环境是严重错误配置；让告警显眼一些。
		logger.Error("admin http: AUTH DISABLED — set ADMIN_HTTP_TOKEN env var or server.admin_http_token in config for production")
	} else {
		logger.Info("admin http: token auth enabled")
	}

	s := &Server{
		instanceID:       instanceID,
		host:             host,
		adminPort:        cfg.AdminPort,
		grpcPort:         cfg.GRPCPort,
		authToken:        authToken,
		instanceRepo:     instanceRepo,
		hotAccountRepo:   hotAccountRepo,
		ruleRepo:         ruleRepo,
		accountingSvc:    accountingSvc,
		dayCutSvc:        dayCutSvc,
		systemConfigSvc:  systemConfigSvc,
		tccArchiveWorker: tccArchiveWorker,
		bufferedBalWk:    bufferedBalWk,
		pinger:           pinger,
		logger:           logger,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/admin/health", s.handleHealth)
	mux.HandleFunc("/admin/health/readiness", s.handleReadiness) // K8s readinessProbe
	mux.HandleFunc("/admin/instances", s.handleListInstances)
	mux.HandleFunc("/admin/reload/buffer-accounts", s.handleReloadBufferAccounts)
	mux.HandleFunc("/admin/reload/hot-accounts", s.handleReloadHotAccounts)
	mux.HandleFunc("/admin/reload/business-types", s.handleReloadBusinessTypes)  // 增加 business_type 后扇出触发
	mux.HandleFunc("/admin/reload/config", s.handleSystemConfigDeprecated)       // v2: 系统配置归 config-center；本端点 410 Gone
	mux.HandleFunc("/admin/reload/transaction-rules", s.handleReloadTransactionRules) // 改完 transaction_rule / account_type_info 后扇出触发
	mux.HandleFunc("/admin/config", s.handleSystemConfigDeprecated)              // v2: 410 Gone, 走 config-center
	mux.HandleFunc("/admin/config/", s.handleSystemConfigDeprecated)             // v2: 410 Gone
	mux.HandleFunc("/admin/hot-accounts/", s.handleHotAccountByID) // PUT/DELETE /{id}
	mux.HandleFunc("/admin/hot-accounts", s.handleHotAccounts)     // GET/POST
	// 系统内部账户管理（平台 / 中间 / 手续费 / 权益 / 中转）
	mux.HandleFunc("/admin/platform-accounts", s.handlePlatformAccount)            // POST 创建单个
	mux.HandleFunc("/admin/platform-accounts/fleet", s.handlePlatformAccountFleet) // POST 批量 100 个（渠道注册）
	mux.HandleFunc("/admin/business-types", s.handleBusinessTypes)                 // GET 列出 / POST 新增 business_type（不建账户）
	mux.HandleFunc("/admin/account-types", s.handleListAccountTypes)               // GET 列出 account_type_info（含 is_platform）
	// TCC 归档
	mux.HandleFunc("/admin/tcc-archive/config", s.handleTccArchiveConfig)          // GET 当前生效的归档配置
	mux.HandleFunc("/admin/tcc-archive/run", s.handleTccArchiveRun)                // POST 立即触发一次归档
	// 平台账户查询（用于 admin-web "平台账户余额" / "平台账户快照"页面）
	mux.HandleFunc("/admin/platform-accounts/balances", s.handlePlatformBalances)   // GET ?business_type=X 返回 100 账户 + 余额
	mux.HandleFunc("/admin/platform-accounts/snapshots", s.handlePlatformSnapshots) // GET ?business_type=X&date=YYYY-MM-DD 返回账户 + 快照
	// Day-cut 卡死分片恢复
	mux.HandleFunc("/admin/day-cut/resume", s.handleDayCutResume) // POST {cut_date, run_id} 重新派发该 run 的 PROCESSING 分片
	// TCC CONFIRMING 半挂起立即恢复（loadtest 后 / 无需等 5min 阈值）
	mux.HandleFunc("/admin/tcc/retry-confirm", s.handleTccRetryConfirm) // POST {threshold_seconds?}
	// 立即触发 buffered balance flush（e2e 测试 / 运维，不等 30s+jitter 周期）
	mux.HandleFunc("/admin/buffered-balance/flush", s.handleBufferedBalanceFlush) // POST
	mux.HandleFunc("/admin/redis/rebuild", s.handleRedisRebuild)                  // POST {as_of?, account_nos?, dry_run?}

	// authMiddleware 包一层：除 /admin/health* 外其他端点都校验 token。
	// /admin/health + /admin/health/readiness 不做鉴权 — k8s probe 不带 header。
	// MaxBytesHandler 限制单请求 body ≤ 1MB，防止恶意 / bug 客户端发超大 JSON OOM 服务。
	// admin endpoints 接收的最大合法请求是 fleet 创建（100 账户 metadata）<< 1MB。
	const maxAdminBodyBytes = 1 << 20 // 1 MB
	handler := http.MaxBytesHandler(s.authMiddleware(mux), maxAdminBodyBytes)

	s.httpServer = &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.AdminPort),
		Handler: handler,
		// ReadHeaderTimeout 防 slowloris：客户端慢慢一字节一字节发 header 不会绕过 ReadTimeout。
		// Go 1.8+ 必须独立设置；缺失被视作中等级别 CWE-400 漏洞。
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		// IdleTimeout 控制 keep-alive 空闲连接保留时长。admin 侧 client（admin-web 单点）
		// 调用频率低，5 秒空闲就关够用，避免大量空闲连接占 fd。
		IdleTimeout: 5 * time.Second,
	}
	return s
}

// Start 注册实例、启动心跳、启动 HTTP server（非阻塞）
func (s *Server) Start(ctx context.Context) error {
	inst := &model.ServiceInstance{
		InstanceID:    s.instanceID,
		Host:          s.host,
		HTTPAdminPort: s.adminPort,
		GRPCPort:      s.grpcPort,
		Status:        model.ServiceInstanceStatusAlive,
		StartedAt:     time.Now(),
	}
	if err := s.instanceRepo.Register(ctx, inst); err != nil {
		s.logger.Warn("admin http: register instance failed (non-fatal)", zap.Error(err))
	} else {
		s.logger.Info("admin http: instance registered",
			zap.String("instanceID", s.instanceID),
			zap.String("host", s.host),
			zap.Int("adminPort", s.adminPort))
	}

	// 心跳 goroutine：每 15 秒续约
	go s.heartbeatLoop(ctx)

	// HTTP server goroutine
	go func() {
		s.logger.Info("admin http server listening", zap.String("addr", s.httpServer.Addr))
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logger.Error("admin http server error", zap.Error(err))
		}
	}()
	return nil
}

// Stop 注销实例、关闭 HTTP server
func (s *Server) Stop(ctx context.Context) error {
	if err := s.instanceRepo.Deregister(ctx, s.instanceID); err != nil {
		s.logger.Warn("admin http: deregister instance failed", zap.Error(err))
	}
	return s.httpServer.Shutdown(ctx)
}

func (s *Server) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := s.instanceRepo.Heartbeat(ctx, s.instanceID); err != nil {
				s.logger.Warn("admin http: heartbeat failed", zap.Error(err))
			}
		case <-ctx.Done():
			return
		}
	}
}

// ─── HTTP handlers ─────────────────────────────────────────────────────────

// authMiddleware 校验 X-Admin-Token 请求头。
//
// 豁免路径：
//   - /admin/health           （k8s livenessProbe，没有 header）
//   - /admin/health/readiness （k8s readinessProbe，没有 header）
//   - /admin/instances        （admin-web seed 发现，需保持简单）
//
// 当 s.authToken 为空字符串时本中间件直接放行（dev 模式 / 向后兼容）；
// 但 NewServer 已在启动时打了 WARN 日志提示生产应配置 token。
//
// 比较使用 ConstantTimeCompare 防 timing attack。
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.authToken == "" {
			next.ServeHTTP(w, r)
			return
		}
		// 豁免：探针 + instances 列表
		switch r.URL.Path {
		case "/admin/health", "/admin/health/readiness", "/admin/instances":
			next.ServeHTTP(w, r)
			return
		}
		got := r.Header.Get("X-Admin-Token")
		if subtleConstantTimeCompareString(got, s.authToken) != 1 {
			s.logger.Warn("admin http: unauthorized request rejected",
				zap.String("path", r.URL.Path),
				zap.String("remote", r.RemoteAddr),
			)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized: missing or invalid X-Admin-Token"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// subtleConstantTimeCompareString constant-time string compare (length-safe).
// crypto/subtle 的 byte-slice 版本要求长度相等；先长度比较再逐字节比较。
func subtleConstantTimeCompareString(a, b string) int {
	if len(a) != len(b) {
		return 0
	}
	return subtleConstantTimeCompare([]byte(a), []byte(b))
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintln(w, "ok")
}

// handleReadiness 供 K8s readinessProbe 使用（与 livenessProbe /admin/health 分开）。
// 返回 503 的典型场景：
//  1. 进程接收到 SIGTERM，BeginDrain 已被调用 → 新流量不要再路由过来
//  2. meta DB ping 失败 → 流量进来也只会失败，提前摘流量避免错误率拉高
//
// 基于 livenessProbe（仍是 200）让 K8s 仍认为容器健康（不重启），仅摘 Service endpoints；
// 然后我们在 Stop 里等 InflightBookingsGauge 归零再关进程，避免切流量中的 TCC Confirm
// 被硬杀导致 stuck TRYING。
//
// 设计取舍：只 ping meta DB（300ms 上限），不轮询所有 100 个分库。复合健康检查会让
// probe 自身成为瓶颈，反而降低稳定性。
func (s *Server) handleReadiness(w http.ResponseWriter, r *http.Request) {
	if s.draining.Load() {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintln(w, "draining")
		return
	}
	if s.pinger != nil {
		if err := s.pinger.PingPrimary(r.Context()); err != nil {
			s.logger.Warn("readiness probe: db ping failed", zap.Error(err))
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, "db_ping_failed: %v\n", err)
			return
		}
	}
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintln(w, "ready")
}

// BeginDrain 标记本实例进入 draining 状态：
//  1. 后续 readiness 探针返回 503；K8s 在一个探针周期内把 pod 从 Service endpoints 摘除
//  2. 已进入 gRPC handler 的请求不受影响（继续跑到返回）
//
// 幂等。调用后一般紧跟 WaitDrain。
func (s *Server) BeginDrain() {
	if s.draining.CompareAndSwap(false, true) {
		s.logger.Info("admin http: draining started, readiness probe will return 503")
	}
}

// WaitDrain 阻塞等待 InflightBookingsGauge 归零 or timeout 到期。
// 返回真实等待时长和 drain 结束时的 inflight 余量（>0 表示超时强退）。
// 典型调用：
//
//	srv.BeginDrain()
//	time.Sleep(probePeriod)   // 给 K8s 摘流量的时间
//	srv.WaitDrain(ctx, 30 * time.Second)
//	srv.Stop(ctx)             // 关 HTTP server + deregister
func (s *Server) WaitDrain(ctx context.Context, timeout time.Duration) (time.Duration, float64) {
	start := time.Now()
	deadline := start.Add(timeout)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		inflight := readGaugeValue(metrics.InflightBookingsGauge)
		if inflight == 0 {
			return time.Since(start), 0
		}
		if time.Now().After(deadline) {
			s.logger.Warn("admin http: drain timeout, exiting with inflight > 0",
				zap.Float64("inflight", inflight),
				zap.Duration("waited", time.Since(start)),
			)
			return time.Since(start), inflight
		}
		select {
		case <-ctx.Done():
			return time.Since(start), readGaugeValue(metrics.InflightBookingsGauge)
		case <-ticker.C:
		}
	}
}

// readGaugeValue 读取 prometheus Gauge 当前值（用于 drain 判断）。
// prometheus.Gauge 没有公开 Get()，官方推荐通过 Write(dto.Metric) 读。
func readGaugeValue(g prometheus.Gauge) float64 {
	var m dto.Metric
	if err := g.Write(&m); err != nil {
		return 0
	}
	return m.GetGauge().GetValue()
}

func (s *Server) handleListInstances(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	instances, err := s.instanceRepo.ListAlive(r.Context(), model.ServiceInstanceAliveThreshold)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	type instanceView struct {
		InstanceID    string    `json:"instance_id"`
		Host          string    `json:"host"`
		HTTPAdminPort int       `json:"http_admin_port"`
		GRPCPort      int       `json:"grpc_port"`
		LastHeartbeat time.Time `json:"last_heartbeat"`
		StartedAt     time.Time `json:"started_at"`
	}
	views := make([]instanceView, len(instances))
	for i, inst := range instances {
		views[i] = instanceView{
			InstanceID:    inst.InstanceID,
			Host:          inst.Host,
			HTTPAdminPort: inst.HTTPAdminPort,
			GRPCPort:      inst.GRPCPort,
			LastHeartbeat: inst.LastHeartbeat,
			StartedAt:     inst.StartedAt,
		}
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) handleReloadBufferAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	count, err := s.accountingSvc.ReloadBufferAccountConfig(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"message": "reloaded", "count": count, "instance_id": s.instanceID})
}

func (s *Server) handleReloadHotAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	count, err := s.accountingSvc.ReloadHotAllowlist(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"message": "reloaded", "count": count, "instance_id": s.instanceID})
}

// handleReloadBusinessTypes POST /admin/reload/business-types
// 强制重新 load meta DB 的 account_business_type_info + account_type_info 到本地缓存。
// 通常由 admin-web backend 在 RegisterBusinessType 成功后，扇出给所有存活实例调用，
// 保证新 business_type 在 200ms 内对所有实例立即生效（不需要等 60s ConfigSync tick）。
func (s *Server) handleReloadBusinessTypes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := s.accountingSvc.ReloadRegistry(r.Context()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message":     "registry reloaded",
		"instance_id": s.instanceID,
	})
}

// handleReloadTransactionRules POST /admin/reload/transaction-rules
// 全表重新拉取 meta DB 的 transaction_rule + account_type_info 到内存 immutable
// 快照。admin-web 在改完规则后扇出给所有存活实例，0 数据延迟生效。
// 不要靠 5min TTL —— 误差窗口里方向写反的代价是资金错位。
func (s *Server) handleReloadTransactionRules(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.ruleRepo == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "rule repo not wired"})
		return
	}
	if err := s.ruleRepo.Reload(r.Context()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message":     "transaction rules reloaded",
		"instance_id": s.instanceID,
	})
}

// ─── 平台账户（系统内部账户）管理 ─────────────────────────────────────────

// platformAccountCreateReq POST /admin/platform-accounts
// 单点创建一个平台账户（单分片）。
type platformAccountCreateReq struct {
	ReservedID  int64 `json:"reserved_id"`
	AccountType int   `json:"account_type"`
	Currency    string `json:"currency"`
}

// handlePlatformAccount POST /admin/platform-accounts
func (s *Server) handlePlatformAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req platformAccountCreateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body: " + err.Error()})
		return
	}
	if req.Currency == "" {
		req.Currency = "PHP"
	}
	acc, err := s.accountingSvc.CreatePlatformAccount(r.Context(), req.ReservedID, model.AccountType(req.AccountType), req.Currency)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, acc)
}

// platformAccountFleetReq POST /admin/platform-accounts/fleet
// 为一个渠道批量创建 100 个平台账户。要求 business_type 已通过
// POST /admin/business-types 登记，本接口仅创建账户，不建 registry 记录。
type platformAccountFleetReq struct {
	AccountType         int    `json:"account_type"`
	ChannelBusinessType int    `json:"channel_business_type"`
	Currency            string `json:"currency"`
}

// handlePlatformAccountFleet POST /admin/platform-accounts/fleet
func (s *Server) handlePlatformAccountFleet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req platformAccountFleetReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body: " + err.Error()})
		return
	}
	result, err := s.accountingSvc.CreatePlatformAccountFleet(r.Context(), &service.CreatePlatformChannelRequest{
		AccountType:         model.AccountType(req.AccountType),
		ChannelBusinessType: req.ChannelBusinessType,
		Currency:            req.Currency,
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"account_type":          req.AccountType,
		"business_type_code":    result.BusinessTypeCode,
		"channel_business_type": result.ChannelBusinessType,
		"currency":              req.Currency,
		"created":               len(result.Accounts),
		"accounts":              result.Accounts,
	})
}

// registerBusinessTypeReq POST /admin/business-types
// 仅注册到 account_business_type_info，不创建账户。
type registerBusinessTypeReq struct {
	AccountType      int    `json:"account_type"`       // 1-9（1-3 业务，4-9 平台内部）
	BusinessTypeCode string `json:"business_type_code"` // 唯一码名，如 ALIPAY_RECEIVABLE
	Description      string `json:"description"`
	BusinessType     int    `json:"business_type"`      // 0 = 自动分配 [101, 999]
}

// handleBusinessTypes 分发 GET / POST /admin/business-types
//   GET  → 列出全部注册
//   POST → 新登记一条 business_type（不建账户；账户另走 Fleet）
func (s *Server) handleBusinessTypes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		rows, err := s.accountingSvc.ListBusinessTypes(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, rows)
	case http.MethodPost:
		var req registerBusinessTypeReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body: " + err.Error()})
			return
		}
		info, err := s.accountingSvc.RegisterBusinessType(r.Context(),
			model.AccountType(req.AccountType),
			req.BusinessTypeCode,
			req.Description,
			req.BusinessType)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, info)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handlePlatformBalances GET /admin/platform-accounts/balances?business_type=101&currency=PHP
// 返回 (business_type, currency) 对应的 100 个平台账户及当前余额（user_id 0..99）。
// currency 必填（多币种 fleet 已是常态，汇总没意义）。
func (s *Server) handlePlatformBalances(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	btStr := r.URL.Query().Get("business_type")
	if btStr == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "business_type is required"})
		return
	}
	bt, err := strconv.Atoi(btStr)
	if err != nil || bt < 1 || bt > 999 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "business_type must be int in [1, 999]"})
		return
	}
	currency := r.URL.Query().Get("currency")
	if currency == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "currency is required"})
		return
	}
	accounts, err := s.accountingSvc.ListPlatformAccountsByBusinessType(r.Context(), model.AccountBusinessType(bt), currency)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"business_type": bt,
		"currency":      currency,
		"count":         len(accounts),
		"accounts":      accounts,
	})
}

// handleListAccountTypes GET /admin/account-types
// 返回 account_type_info 全表。前端读 is_platform 字段来决定哪些 account_type 是
// 平台内部类型。比硬编码 4-9 更易扩展：加新类型只需在 meta DB 补一行。
func (s *Server) handleListAccountTypes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.ruleRepo == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "rule repo not wired"})
		return
	}
	rows, err := s.ruleRepo.ListAccountTypes(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// handleTccArchiveConfig GET /admin/tcc-archive/config
// 返回当前 TccArchiveWorker 的运行时配置。
func (s *Server) handleTccArchiveConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.tccArchiveWorker == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "tcc archive worker not wired"})
		return
	}
	cfg := s.tccArchiveWorker.Config()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"interval_seconds": int(cfg.Interval.Seconds()),
		"retention_days":   int(cfg.Retention.Hours() / 24),
		"batch_size":       cfg.BatchSize,
	})
}

// handleTccArchiveRun POST /admin/tcc-archive/run
// 立即触发一次归档扫描（异步），返回 202 立刻返回。
// 独立 context + 10min timeout，不受请求 ctx 取消影响。
func (s *Server) handleTccArchiveRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.tccArchiveWorker == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "tcc archive worker not wired"})
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		s.tccArchiveWorker.ArchiveNow(ctx)
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{
		"status":  "running",
		"message": "TCC archive triggered; check accounting_tcc_archived_total / accounting_tcc_archive_errors_total metrics",
	})
}

// handleDayCutResume POST /admin/day-cut/resume
// Body: {"cut_date": "YYYY-MM-DD", "run_id": N, "stuck_threshold_seconds": 0}
// 重新派发指定 (cut_date, run_id) 中处于 PROCESSING 且无 worker 跑的卡死分片。
//
// 用例：
//   - DB 断网 + 重连后，goroutine 已死但 status=PROCESSING，watchdog 5min 才能识别。
//     用此接口手动触发立即恢复。
//   - 跨日历的旧重跑（v5/v6 同时挂起）watchdog 只看 latest run_id 不会处理 v5；
//     此接口可对特定 (cut_date, run_id) 显式恢复。
//
// stuck_threshold_seconds = 0 → 强制恢复全部 PROCESSING（不过滤 updated_at）。
// 立即返回 202，实际派发的恢复在后台 goroutine 中跑。
func (s *Server) handleDayCutResume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.dayCutSvc == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "day-cut service not wired"})
		return
	}
	var req struct {
		CutDate               string `json:"cut_date"`
		RunID                 int    `json:"run_id"`
		StuckThresholdSeconds int    `json:"stuck_threshold_seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body: " + err.Error()})
		return
	}
	if req.CutDate == "" || req.RunID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cut_date and run_id are required"})
		return
	}
	threshold := time.Duration(req.StuckThresholdSeconds) * time.Second
	count, err := s.dayCutSvc.ResumeStuckShards(r.Context(), req.CutDate, req.RunID, threshold)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"status":     "running",
		"cut_date":   req.CutDate,
		"run_id":     req.RunID,
		"dispatched": count,
		"message":    "stuck shards re-dispatched in background; monitor day_cut_control table for completion",
	})
}

// handleSystemConfigDeprecated /admin/config /admin/config/* /admin/reload/config
//
// **v2 改造**：accounting-system 的 system_config 表 + 山寨 reload 已删除；
// 所有写操作改走全平台 config-center 服务（packages/config-center）。本端点
// 一律返 410 Gone + 引导。
func (s *Server) handleSystemConfigDeprecated(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusGone, map[string]string{
		"error": "system_config 已迁到 config-center；请走 PUT /api/v1/configs/accounting-system/<key> " +
			"或 config-center admin web (/admin/ns/accounting-system)",
	})
}

// handleTccRetryConfirm POST /admin/tcc/retry-confirm
// Body: {"threshold_seconds": 0}
//
// 立即重试所有处于 phase=CONFIRMING 且 updated_at 早于 threshold_seconds 的 TCC。
// threshold_seconds = 0（默认）= 不过滤，所有 CONFIRMING 立即重试。
//
// 用例：
//   - loadtest 后想马上跑 day-cut，不想等 TccRecoveryWorker 30s tick × 5min 阈值
//   - 排查某个半挂起 TCC，强制推进
//
// 等价于 TccRecoveryWorker 中的那段 CONFIRMING 恢复逻辑，只是阈值可调。
func (s *Server) handleTccRetryConfirm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.accountingSvc == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "accounting service not wired"})
		return
	}
	var req struct {
		ThresholdSeconds int `json:"threshold_seconds"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req) // body 可选

	// 构造"过滤时间点"：updated_at < before 才会被捞起处理
	before := time.Now()
	if req.ThresholdSeconds > 0 {
		before = before.Add(-time.Duration(req.ThresholdSeconds) * time.Second)
	} else {
		// 0 = 无阈值，把 before 设得足够未来，保证所有 CONFIRMING 都被抓
		before = before.Add(24 * time.Hour)
	}

	count, err := s.accountingSvc.RetryStuckConfirmingTcc(r.Context(), before)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"recovered":         count,
		"threshold_seconds": req.ThresholdSeconds,
		"instance_id":       s.instanceID,
		"message":           "run this endpoint on one instance is enough — RetryStuckConfirmingTcc scans all 100 shards",
	})
}

// handlePlatformSnapshots GET /admin/platform-accounts/snapshots?business_type=101&date=2026-04-15&currency=PHP
// 返回 (business_type, currency) 对应的 100 个平台账户及其 cutDate 当日快照。
// currency 必填。
func (s *Server) handlePlatformSnapshots(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	btStr := r.URL.Query().Get("business_type")
	date := r.URL.Query().Get("date")
	currency := r.URL.Query().Get("currency")
	if btStr == "" || date == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "business_type and date are required"})
		return
	}
	if currency == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "currency is required"})
		return
	}
	bt, err := strconv.Atoi(btStr)
	if err != nil || bt < 1 || bt > 999 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "business_type must be int in [1, 999]"})
		return
	}
	rows, err := s.accountingSvc.ListPlatformSnapshotsByBusinessType(r.Context(), model.AccountBusinessType(bt), date, currency)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"business_type": bt,
		"date":          date,
		"currency":      currency,
		"count":         len(rows),
		"rows":          rows,
	})
}

// ─── 热点账户 CRUD handlers ────────────────────────────────────────────────

// handleHotAccounts 处理 GET /admin/hot-accounts 和 POST /admin/hot-accounts
func (s *Server) handleHotAccounts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listHotAccounts(w, r)
	case http.MethodPost:
		s.createHotAccount(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleHotAccountByID 处理 PUT /admin/hot-accounts/{id} 和 DELETE /admin/hot-accounts/{id}
func (s *Server) handleHotAccountByID(w http.ResponseWriter, r *http.Request) {
	// 从路径中提取 id：/admin/hot-accounts/{id}
	idStr := strings.TrimPrefix(r.URL.Path, "/admin/hot-accounts/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	switch r.Method {
	case http.MethodPut:
		s.updateHotAccount(w, r, id)
	case http.MethodDelete:
		s.deleteHotAccount(w, r, id)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) listHotAccounts(w http.ResponseWriter, r *http.Request) {
	list, err := s.hotAccountRepo.ListAll(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) createHotAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccountNo   string `json:"account_no"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if req.AccountNo == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "account_no is required"})
		return
	}
	cfg := &model.HotAccountConfig{
		AccountNo:   req.AccountNo,
		Enabled:     true,
		Description: req.Description,
	}
	if err := s.hotAccountRepo.Create(r.Context(), cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

func (s *Server) updateHotAccount(w http.ResponseWriter, r *http.Request, id int64) {
	var req struct {
		Enabled     bool   `json:"enabled"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if err := s.hotAccountRepo.Update(r.Context(), id, req.Enabled, req.Description); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "updated"})
}

func (s *Server) deleteHotAccount(w http.ResponseWriter, r *http.Request, id int64) {
	if err := s.hotAccountRepo.Delete(r.Context(), id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "deleted"})
}

// resolveOutboundIP 返回本机对外可达的 IP 地址。
// 通过向公网 UDP 目标发起"连接"（不实际发送数据包）来确定出口网卡 IP，
// 在容器、虚拟机等多网卡环境下比 os.Hostname() 更准确。
// 若获取失败则回退到 "127.0.0.1"。
func resolveOutboundIP() string {
	// 连接到公网 IP（UDP，不发送数据，仅确定出口接口）
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		// 回退：枚举网卡，取第一个非回环的 IPv4 地址
		ifaces, ierr := net.Interfaces()
		if ierr != nil {
			return "127.0.0.1"
		}
		for _, iface := range ifaces {
			if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
				continue
			}
			addrs, aerr := iface.Addrs()
			if aerr != nil {
				continue
			}
			for _, addr := range addrs {
				var ip net.IP
				switch v := addr.(type) {
				case *net.IPNet:
					ip = v.IP
				case *net.IPAddr:
					ip = v.IP
				}
				if ip == nil || ip.IsLoopback() {
					continue
				}
				if ip4 := ip.To4(); ip4 != nil {
					return ip4.String()
				}
			}
		}
		return "127.0.0.1"
	}
	defer conn.Close()
	localAddr := conn.LocalAddr().(*net.UDPAddr)
	return localAddr.IP.String()
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

// ErrorEnvelope 统一的 admin API 错误响应。前端 / admin-web 一律期望这个 shape
// 替代散落的 `{"error": "..."}` / `{"message": "..."}`；包含机读 code +
// 人读 message + 可选 details。
type ErrorEnvelope struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Details string `json:"details,omitempty"`
}

// writeErr 统一错误响应。code 沿用 HTTP 状态码语义。
//   writeErr(w, 400, "bad_request", "config_key required")
//   writeErr(w, 500, "db_error", err.Error())
func writeErr(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, ErrorEnvelope{Code: status, Message: message, Details: code})
}

// methodNotAllowed 统一处理 405。
func methodNotAllowed(w http.ResponseWriter) {
	writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
}

// handleBufferedBalanceFlush POST /admin/buffered-balance/flush
// 立即跑一次 BufferedBalanceWorker.FlushNow，把所有到期的缓冲 delta 落到 account 表。
// 用途：e2e 测试避免等 30s+jitter；生产运维排查。
func (s *Server) handleBufferedBalanceFlush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.bufferedBalWk == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "buffered balance worker not wired"})
		return
	}
	s.bufferedBalWk.FlushNow(r.Context())
	writeJSON(w, http.StatusOK, map[string]interface{}{"flushed": true})
}

// handleRedisRebuild POST /admin/redis/rebuild
//
// Body:
//
//	{
//	  "as_of":       "2026-04-24T15:00:00Z" | "5m" | "" (=now),
//	  "account_nos": ["010100001-001"]      // 可选，缺省全量启用热账户
//	  "dry_run":     true                   // 可选，只算 diff 不写
//	}
//
// 仅重建 hot_account_config 中 enabled=1 的账户；其他账户 Redis 本来就不存权威态。
func (s *Server) handleRedisRebuild(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		AsOf       string   `json:"as_of"`
		AccountNos []string `json:"account_nos"`
		DryRun     bool     `json:"dry_run"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body: " + err.Error()})
		return
	}
	opts := service.RebuildOptions{
		AccountNos: req.AccountNos,
		DryRun:     req.DryRun,
	}
	if req.AsOf != "" {
		// 支持两种形式：相对时长 ("5m", "2h") 或 RFC3339 时间戳
		if d, err := time.ParseDuration(req.AsOf); err == nil {
			opts.AsOf = time.Now().Add(-d)
		} else if t, err := time.Parse(time.RFC3339, req.AsOf); err == nil {
			opts.AsOf = t
		} else {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("as_of 必须是 duration（如 5m / 2h）或 RFC3339 时间戳；got %q", req.AsOf),
			})
			return
		}
	}
	report, err := s.accountingSvc.RebuildHotAccounts(r.Context(), opts)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, report)
}
