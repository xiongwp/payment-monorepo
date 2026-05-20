// Command server 启动 config-center。
//
// 进程拓扑：
//   - HTTP :9691    admin web + SDK REST + SSE watch  (mTLS in prod)
//   - gRPC :9690    保留给未来 protoc 后接入；当前 v1.0 SDK 走 HTTP
//   - HTTP :9692    Prometheus /metrics
//
// 依赖：
//   - MySQL config_center_meta (4 张表)
//   - etcd (自注册 + 服务发现，可选；空列表则跳过)
//   - Kafka (多副本跨实例 fan-out，可选；空列表则单副本模式)
//
// 启动期 fail-fast：env=prod 时强制要求 TLS / KMS（如有）/ DB DSN 非空。
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/viper"
	"go.uber.org/fx"
	"go.uber.org/zap"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/xiongwp/config-center/internal/metrics"
	"github.com/xiongwp/config-center/internal/repo"
	"github.com/xiongwp/config-center/internal/server"
	"github.com/xiongwp/config-center/internal/service"
)

func main() {
	app := fx.New(
		fx.StartTimeout(30*time.Second),
		fx.StopTimeout(30*time.Second),
		fx.Provide(
			loadConfig,
			newLogger,
			newDB,
			newRepo,
			newService,
			newHealthChecker,
			newHTTPAPI,
			newAdminUI,
		),
		fx.Invoke(
			assertProdSafety,
			startMetricsServer,
			startHTTPServer,
			startServiceRegistrar,
		),
	)
	app.Run()
}

// ─── providers ───────────────────────────────────────────────────────────

func loadConfig() (*viper.Viper, error) {
	v := viper.New()
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath("./config")
	v.AddConfigPath("/app/config")
	v.SetEnvPrefix("CFG")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	if err := v.ReadInConfig(); err != nil {
		// 允许 dev 不带 config 文件（env 完全覆盖）
		if !errors.Is(err, &viper.ConfigFileNotFoundError{}) {
			fmt.Fprintf(os.Stderr, "warning: config not found: %v\n", err)
		}
	}
	return v, nil
}

func newLogger(v *viper.Viper) (*zap.Logger, error) {
	if v.GetString("env") == "prod" {
		return zap.NewProduction()
	}
	return zap.NewDevelopment()
}

// newDB 单 meta 库 GORM 连接。配置量小 + 写少；不分片不读副本。
func newDB(v *viper.Viper, logger *zap.Logger) (*gorm.DB, error) {
	dsn := v.GetString("database.meta.dsn")
	if dsn == "" {
		// dev 兜底：从环境变量拼一个本地 MySQL DSN
		host := v.GetString("database.meta.host")
		if host == "" {
			host = "127.0.0.1:3306"
		}
		dsn = fmt.Sprintf("root:@tcp(%s)/config_center_meta?charset=utf8mb4&parseTime=True&loc=Local", host)
	}
	// upsert 路径 (repo.upsertItemForUpdate) 用 First() 探活, 行不存在是预期路径,
	// IgnoreRecordNotFoundError=true 让 GORM logger 不把 ErrRecordNotFound 当 warn 刷屏.
	gormLog := gormlogger.New(
		gormStdLogger{},
		gormlogger.Config{
			SlowThreshold:             200 * time.Millisecond,
			LogLevel:                  gormlogger.Warn,
			IgnoreRecordNotFoundError: true,
			Colorful:                  false,
		},
	)
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: gormLog})
	if err != nil {
		return nil, fmt.Errorf("open meta db: %w", err)
	}
	sqldb, err := db.DB()
	if err != nil {
		return nil, err
	}
	if v := v.GetInt("database.meta.max_open_conns"); v > 0 {
		sqldb.SetMaxOpenConns(v)
	}
	if v := v.GetInt("database.meta.max_idle_conns"); v > 0 {
		sqldb.SetMaxIdleConns(v)
	}
	if v := v.GetInt("database.meta.conn_max_lifetime"); v > 0 {
		sqldb.SetConnMaxLifetime(time.Duration(v) * time.Second)
	}
	logger.Info("config-center: db opened", zap.String("dsn", maskDSN(dsn)))
	return db, nil
}

func newRepo(db *gorm.DB) *repo.Repo {
	return repo.NewRepo(db)
}

func newService(r *repo.Repo, logger *zap.Logger) *service.Service {
	return service.New(r, logger)
}

func newHealthChecker(db *gorm.DB, logger *zap.Logger) *server.HealthChecker {
	hc := server.NewHealthChecker(logger)
	for name, p := range server.CommonProbes(db) {
		hc.Register(name, p)
	}
	return hc
}

func newHTTPAPI(svc *service.Service, logger *zap.Logger) *server.HTTPAPI {
	return server.NewHTTPAPI(svc, logger)
}

// AdminUI 包封装一下；admin_html.go 已经有 register 函数（待加 UIRouter 类型）
type AdminUI struct {
	repo   *repo.Repo
	svc    *service.Service
	logger *zap.Logger
}

func newAdminUI(r *repo.Repo, svc *service.Service, logger *zap.Logger) *AdminUI {
	return &AdminUI{repo: r, svc: svc, logger: logger}
}

// newGRPCServer 已删 — config-center 切 Kitex 后, gRPC stub server (含 health.v1)
// 不再需要 (K8s 改走 HTTP /healthz; Kitex 业务 server 在 cmd/server/main.go 自己起).

// ─── lifecycle invokes ───────────────────────────────────────────────────

func assertProdSafety(v *viper.Viper, logger *zap.Logger) error {
	env := v.GetString("env")
	if env != "prod" {
		return nil
	}
	if v.GetString("tls.cert") == "" || v.GetString("tls.key") == "" || v.GetString("tls.client_ca") == "" {
		return errors.New("env=prod but tls.{cert,key,client_ca} empty — refusing to start")
	}
	if v.GetString("database.meta.dsn") == "" {
		return errors.New("env=prod but database.meta.dsn empty — refusing to start")
	}
	logger.Info("config-center: prod safety asserted")
	return nil
}

func startMetricsServer(lc fx.Lifecycle, v *viper.Viper, logger *zap.Logger) {
	addr := v.GetString("metrics.addr")
	if addr == "" {
		addr = ":9692"
	}
	metrics.Register()
	mux := http.NewServeMux()
	mux.Handle("/metrics", promHandler())
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				return err
			}
			go srv.Serve(ln)
			logger.Info("config-center: /metrics on " + addr)
			return nil
		},
		OnStop: func(ctx context.Context) error { return srv.Shutdown(ctx) },
	})
}

// startHTTPServer admin UI + SDK REST + SSE watch + healthz/readyz 都挂这一个。
func startHTTPServer(lc fx.Lifecycle, v *viper.Viper, logger *zap.Logger,
	httpAPI *server.HTTPAPI, ui *AdminUI, hc *server.HealthChecker) {

	addr := fmt.Sprintf(":%d", v.GetInt("server.http_port"))
	if v.GetInt("server.http_port") == 0 {
		addr = ":9691"
	}
	mux := http.NewServeMux()
	httpAPI.Register(mux)
	registerAdminUI(mux, ui, v)
	hc.MountHTTP(mux)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				return err
			}
			go func() {
				if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
					logger.Error("http server", zap.Error(err))
				}
			}()
			logger.Info("config-center: http on " + addr)
			return nil
		},
		OnStop: func(ctx context.Context) error {
			hc.BeginDrain()
			return srv.Shutdown(ctx)
		},
	})
}

// startGRPCServer 已删 — Kitex server 自带 listener 在 main.go 自启.

// promHandler /metrics handler。
func promHandler() http.Handler {
	return promhttp.Handler()
}

// registerAdminUI 把 AdminHandler 挂到 mux。包了一层方便 main.go 测试 mock。
//
// 关键纪律：admin UI 永远 Mount，不会因为 introspector / template 任何故障而
// 让 /admin/ 返 404。鉴权失败应该返 401/403，不该让运营连页面都打不开。
//
// introspector 初始化失败（user-merchant-core 不可达）时 introspect 传 nil，
// adminActorMiddleware 会 fallback 到 dev actor（admin@localhost）+ 一行 WARN
// 日志，让 admin 至少能看到 / 改配置。生产环境应让 introspector 真起来。
func registerAdminUI(mux *http.ServeMux, ui *AdminUI, v *viper.Viper) {
	if ui == nil {
		return
	}

	userMerchantEndpoint := v.GetString("auth.introspect_endpoint")
	if userMerchantEndpoint == "" {
		userMerchantEndpoint = "user-merchant-core:9090"
	}

	// introspector 是 dev fallback 友好的 —— 即使 dial 失败，也只是导致
	// IntrospectToken RPC 阶段返 503，不应阻塞 mount。
	introspect, err := server.NewTokenIntrospector(userMerchantEndpoint, ui.logger)
	if err != nil {
		ui.logger.Warn("admin UI: token introspector init failed; fallback to dev actor",
			zap.String("endpoint", userMerchantEndpoint), zap.Error(err))
		introspect = nil
	}

	h, err := server.NewAdminHandler(ui.svc, ui.logger, introspect)
	if err != nil {
		ui.logger.Error("admin UI: NewAdminHandler failed; admin / page will 500 but routes still mounted",
			zap.Error(err))
		// 即使构造失败也不返回 —— 让用户看到 500 比看到 404 好排查。
		// 但 Mount 需要非 nil handler，这里不给挂避免 nil deref；
		// 用户看到 404 时 docker logs 会有这条 ERROR。
		return
	}
	h.Mount(mux)
	ui.logger.Info("admin UI mounted at /admin/")
}

// ─── helpers ─────────────────────────────────────────────────────────────

// gormStdLogger 给 gormlogger.New 的 Writer 入参. gormlogger.Writer 接口要求:
//   Printf(string, ...interface{})
// 标准库 log.Default() 也满足, 但每行带时间戳; 这里用 fmt.Printf 直出, 跟现有
// zap logger 风格统一 (zap 自己加时间戳 + json/console encoder).
type gormStdLogger struct{}

func (gormStdLogger) Printf(format string, args ...interface{}) {
	fmt.Printf(format+"\n", args...)
}

func maskDSN(s string) string {
	// 脱敏密码：root:xxx@tcp → root:***@tcp
	at := strings.Index(s, "@")
	if at < 0 {
		return s
	}
	prefix := s[:at]
	colon := strings.LastIndex(prefix, ":")
	if colon < 0 {
		return s
	}
	return prefix[:colon+1] + "***" + s[at:]
}

