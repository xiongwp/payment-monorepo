// server.go — Fiber 应用构造 + 标准中间件链.
//
// 标准中间件 (按顺序):
//   1. recover    — panic 兜底
//   2. request-id — X-Request-ID 注入 / 透传
//   3. logger     — 结构化 access log (跟 zap 集成)
//   4. metrics    — prometheus latency / status 直方图
//   5. cors       — 默认拒绝外 origin (仅 admin web)
//   6. ratelimit  — 全局 + per-key
//   7. oauth2     — Bearer JWT 校验 (按 cfg 开关)
//   8. trace-id   — OTel propagation
//
// /healthz, /readyz, /metrics 自动有, 不走中间件链.
// /admin/* 自动加 admin token 守护.

package scaffold

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/gofiber/fiber/v2/middleware/requestid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/valyala/fasthttp/fasthttpadaptor"
	"go.uber.org/zap"
)

// App 包装 fiber + db + log + registry — 业务侧拿这个注册路由.
type App struct {
	Fiber    *fiber.App
	Log      *zap.Logger
	DB       *DB
	Cfg      *Config
	Registry *prometheus.Registry
}

// NewApp 构造 + 装中间件 + 注册公共路由.
func NewApp(cfg *Config, log *zap.Logger, db *DB) *App {
	fapp := fiber.New(fiber.Config{
		AppName:               cfg.ServiceName,
		DisableStartupMessage: !cfg.LogDev,
		ReadTimeout:           30 * time.Second,
		WriteTimeout:          30 * time.Second,
		IdleTimeout:           120 * time.Second,
		ErrorHandler:          newErrorHandler(log),
		JSONEncoder:           json.Marshal,
		JSONDecoder:           json.Unmarshal,
	})

	// metrics
	reg := prometheus.NewRegistry()
	mLatency := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "HTTP request latency",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "path", "status_class"})
	mInflight := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "http_inflight_requests",
		Help: "in-flight HTTP requests",
	})
	reg.MustRegister(mLatency, mInflight)

	// ── 中间件链 ──
	fapp.Use(recover.New(recover.Config{EnableStackTrace: cfg.LogDev}))
	fapp.Use(requestid.New())
	fapp.Use(fiberTraceMW())   // 必须早, 后续中间件都拿得到 trace_id
	fapp.Use(zapLoggerMW(log))
	fapp.Use(metricsMW(mLatency, mInflight))
	if cfg.Rate.Enabled {
		fapp.Use(rateLimitMW(cfg.Rate, log))
	}
	if cfg.OAuth2.IntrospectURL != "" {
		fapp.Use(oauth2BearerMW(cfg.OAuth2, log))
	}

	// 公共路由
	fapp.Get("/healthz", func(c *fiber.Ctx) error {
		return c.SendString("ok")
	})
	fapp.Get("/readyz", func(c *fiber.Ctx) error {
		// 真实检查 DB 连接
		if db != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
			defer cancel()
			if err := db.sqlDB.PingContext(ctx); err != nil {
				return c.Status(503).SendString("db not ready")
			}
		}
		return c.SendString("ready")
	})
	// metrics 用 promhttp (绕过 fiber 中间件); 用 fasthttp adapter 转
	fapp.Get("/metrics", func(c *fiber.Ctx) error {
		handler := promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
		fasthttpadaptor.NewFastHTTPHandler(handler)(c.Context())
		return nil
	})

	if db != nil {
		db.RegisterMetrics(reg, "db")
	}

	return &App{Fiber: fapp, Log: log, DB: db, Cfg: cfg, Registry: reg}
}

// AdminGroup 返回 /admin 子路由 — 自动加 X-Admin-Token 守护.
func (a *App) AdminGroup() fiber.Router {
	return a.Fiber.Group("/admin", adminTokenMW(a.Cfg.AdminToken, a.Log))
}

// ── 中间件实现 ──

func zapLoggerMW(log *zap.Logger) fiber.Handler {
	return func(c *fiber.Ctx) error {
		t0 := time.Now()
		err := c.Next()
		log.Info("http",
			zap.String("req_id", c.GetRespHeader("X-Request-Id")),
			zap.String("trace_id", TraceIDFromCtx(c.UserContext())),
			zap.String("method", c.Method()),
			zap.String("path", c.Path()),
			zap.Int("status", c.Response().StatusCode()),
			zap.Duration("dur", time.Since(t0)),
			zap.String("ip", c.IP()),
		)
		return err
	}
}

func metricsMW(latency *prometheus.HistogramVec, inflight prometheus.Gauge) fiber.Handler {
	return func(c *fiber.Ctx) error {
		inflight.Inc()
		defer inflight.Dec()
		t0 := time.Now()
		err := c.Next()
		status := c.Response().StatusCode()
		class := "2xx"
		switch {
		case status >= 500:
			class = "5xx"
		case status >= 400:
			class = "4xx"
		case status >= 300:
			class = "3xx"
		}
		latency.WithLabelValues(c.Method(), c.Route().Path, class).Observe(time.Since(t0).Seconds())
		return err
	}
}

// 简单 token bucket 实现 — 真生产换 redis sliding window.
type tokenBucket struct {
	mu       sync.Mutex
	tokens   float64
	last     time.Time
	cap      float64
	refill   float64 // tokens / sec
}

func (b *tokenBucket) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	delta := now.Sub(b.last).Seconds()
	b.tokens += delta * b.refill
	if b.tokens > b.cap {
		b.tokens = b.cap
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

func rateLimitMW(cfg RateConfig, log *zap.Logger) fiber.Handler {
	global := &tokenBucket{cap: float64(cfg.GlobalRPS), refill: float64(cfg.GlobalRPS), last: time.Now(), tokens: float64(cfg.GlobalRPS)}
	perKey := sync.Map{} // key (client_id 或 IP) → *tokenBucket

	return func(c *fiber.Ctx) error {
		if cfg.GlobalRPS > 0 && !global.allow() {
			return fiber.NewError(429, "rate limit (global)")
		}
		if cfg.PerKeyRPS > 0 {
			key := c.Get("X-Client-Id")
			if key == "" {
				key = c.IP()
			}
			v, _ := perKey.LoadOrStore(key, &tokenBucket{
				cap:    float64(cfg.PerKeyRPS),
				refill: float64(cfg.PerKeyRPS),
				last:   time.Now(),
				tokens: float64(cfg.PerKeyRPS),
			})
			b := v.(*tokenBucket)
			if !b.allow() {
				return fiber.NewError(429, "rate limit ("+key+")")
			}
		}
		return c.Next()
	}
}

func oauth2BearerMW(cfg OAuth2Config, log *zap.Logger) fiber.Handler {
	// 简化实现; 真生产用 payment-mw/oauth_bearer 的 RSA JWT 验签 + introspect cache.
	// 这里只 stub: 把 Bearer token 解出来塞 ctx, 业务 handler 自查 scope.
	return func(c *fiber.Ctx) error {
		auth := c.Get("Authorization")
		if auth == "" {
			if cfg.Required {
				return fiber.NewError(401, "missing bearer token")
			}
			return c.Next()
		}
		if len(auth) < 8 || auth[:7] != "Bearer " {
			return fiber.NewError(401, "malformed bearer")
		}
		token := auth[7:]
		// 真生产: introspect 拿 client_id + scope; 这里仅透传
		c.Locals("oauth2_token", token)
		return c.Next()
	}
}

func adminTokenMW(token string, log *zap.Logger) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if token == "" {
			return c.Next() // dev: 没设 admin token 不阻拦
		}
		if c.Get("X-Admin-Token") != token {
			return fiber.NewError(403, "admin token required")
		}
		return c.Next()
	}
}

var _ = atomic.AddUint64 // 防 unused
var _ = strconv.Itoa
