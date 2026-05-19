// Command server 启动 kms-manage Kitex 服务 (Protobuf IDL, TTHeader).
//
// 切 Kitex 后 wire 协议跟 gRPC 不互通; 调用方 (card-center / payment-core /
// user-merchant-core / payment-admin-web) 必须同步切到 kmsservice.NewClient.
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/cloudwego/kitex/server"
	"github.com/redis/go-redis/v9"
	"github.com/spf13/viper"
	"github.com/xiongwp/payment-util/configcenter"
	"github.com/xiongwp/payment-util/kitexutil"
	"github.com/xiongwp/payment-util/trace"
	"go.uber.org/fx"
	"go.uber.org/zap"

	kmsv1 "github.com/xiongwp/kms-manage/kitex_gen/kms/v1"
	kmsservice "github.com/xiongwp/kms-manage/kitex_gen/kms/v1/kmsservice"

	"github.com/xiongwp/kms-manage/internal/keystore"
	"github.com/xiongwp/kms-manage/internal/metrics"
	kmsimpl "github.com/xiongwp/kms-manage/internal/server"
	"github.com/xiongwp/kms-manage/internal/service"
)

func main() {
	metrics.Register()

	// OTel 初始化：OTEL_EXPORTER_OTLP_ENDPOINT 空 → no-op；非空 → 走 OTLP gRPC
	// 推 span 到 collector（Jaeger / Tempo / Grafana Agent）。
	otelShutdown, otelErr := trace.InitOTel(context.Background(), "kms-manage", os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
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
			loadConfig,
			newLogger,
			// config-center: rate_limit / auth tokens / SAN whitelist (敏感，建议 TARGETED 推)
			configcenter.FxProvider("kms-manage"),
			newKeystore,
			newRedisClient,
			newKMSSvc,
			newServer,
		),
		fx.Invoke(startGRPC, startMetricsHTTP, startServiceRegistrar),
	)
	app.Run()
}

func loadConfig() (*viper.Viper, error) {
	v := viper.New()
	v.SetEnvPrefix("KMS")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	_ = v.BindEnv("env")
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath("./config")
	v.AddConfigPath(".")
	v.AddConfigPath("/etc/kms-manage")
	if err := v.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if !errors.As(err, &notFound) {
			return nil, err
		}
	}
	if err := assertProdSafety(v); err != nil {
		return nil, err
	}
	return v, nil
}

// assertProdSafety KMS 是密钥服务，prod 安全要求最严：
//  1. auth.allow_unauthenticated 必须 false（任何人能解密 = 全平台密钥泄漏）
//  2. auth.tokens 必须配（mesh 内服务凭 token 调本服务）
//  3. keystore.dir 必须明确（默认 /var/lib/kms-manage/keys 但生产应显式声明）
//  4. **mTLS 三件齐全**（P0-4）：tls.server_cert / tls.server_key / tls.client_ca
//     不齐全 = 退化单向 TLS = 谁都能拿 PAN 加解密，PCI Req 4 / Req 8 不达标。
//  5. **Client identity 白名单非空**（P0-4）：auth.allowed_client_ids 至少 1 项，
//     防止 CA 误签的 cert 也能调 KMS。
func assertProdSafety(v *viper.Viper) error {
	env := strings.ToLower(strings.TrimSpace(v.GetString("env")))
	if env != "prod" && env != "production" {
		return nil
	}
	if v.GetBool("auth.allow_unauthenticated") {
		return fmt.Errorf("PROD-SAFETY: KMS auth.allow_unauthenticated=true is forbidden in env=prod (anyone could decrypt all merchant secrets)")
	}
	if len(v.GetStringSlice("auth.tokens")) == 0 {
		return fmt.Errorf("PROD-SAFETY: KMS auth.tokens must be configured in env=prod (mesh services authenticate with tokens)")
	}
	if strings.TrimSpace(v.GetString("keystore.dir")) == "" {
		return fmt.Errorf("PROD-SAFETY: KMS keystore.dir must be explicitly configured in env=prod (don't rely on default path)")
	}
	for _, k := range []string{"tls.server_cert", "tls.server_key", "tls.client_ca"} {
		if strings.TrimSpace(v.GetString(k)) == "" {
			return fmt.Errorf("PROD-SAFETY: KMS %s required in env=prod (mTLS mandatory; P0-4)", k)
		}
	}
	// auth.allowed_client_ids (SAN whitelist) + rate_limit 已 100% 迁到 config-center。
	// SAN 在 namespace=kms-manage 下 key=auth.allowed_client_ids（JSON list）；
	// 强制 prod 必须配 config-center，admin web 必须有非空 allowlist 才接调用。
	return configcenter.AssertProdMandatory(v)
}

func newLogger() (*zap.Logger, error) {
	if os.Getenv("KMS_LOG_DEV") == "true" {
		return zap.NewDevelopment()
	}
	return zap.NewProduction()
}

func newKeystore(v *viper.Viper, logger *zap.Logger) (*keystore.Store, error) {
	dir := v.GetString("keystore.dir")
	if dir == "" {
		dir = "/var/lib/kms-manage/keys"
	}
	s, err := keystore.Load(dir)
	if err != nil {
		return nil, err
	}
	// 暴露给 Prometheus：active key 年龄 + loaded key 总数。
	// 监控告警阈值参考 PCI-DSS 3.6.4：超过 90 天未轮换报 P1。
	metrics.LoadedKeyCount.Set(float64(len(s.List())))
	if meta, ok := s.Meta(s.ActiveKeyID()); ok {
		age := time.Since(meta.CreatedAt)
		metrics.ActiveKeyAgeSeconds.Set(age.Seconds())
		logger.Info("keystore loaded",
			zap.String("dir", dir),
			zap.String("active_key", s.ActiveKeyID()),
			zap.Int("total_keys", len(s.List())),
			zap.Duration("active_key_age", age),
			zap.Time("active_key_created_at", meta.CreatedAt),
		)
		// PCI-DSS 90 天: 7,776,000 秒。超过给 WARN 提醒运维。
		if age > 90*24*time.Hour {
			logger.Warn("active master key is older than 90 days; consider rotating (PCI-DSS 3.6.4)",
				zap.Duration("age", age))
		}
	} else {
		logger.Info("keystore loaded",
			zap.String("dir", dir),
			zap.String("active_key", s.ActiveKeyID()),
			zap.Int("total_keys", len(s.List())),
		)
	}
	return s, nil
}

// newRedisClient creates optional Redis client for cache.
// Redis configuration is optional; if unavailable, service degrades gracefully.
func newRedisClient(v *viper.Viper, logger *zap.Logger) *redis.Client {
	addr := v.GetString("redis.addr")
	if addr == "" {
		addr = "redis:6379" // default for docker-compose
	}
	enabled := v.GetBool("redis.enabled")
	if !enabled {
		logger.Info("redis cache disabled")
		return nil
	}
	client := redis.NewClient(&redis.Options{
		Addr:         addr,
		Password:     v.GetString("redis.password"),
		DB:           v.GetInt("redis.db"),
		ReadTimeout:  v.GetDuration("redis.read_timeout"),
		WriteTimeout: v.GetDuration("redis.write_timeout"),
	})
	// Test connection
	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		logger.Warn("redis connection failed; cache will be disabled",
			zap.String("addr", addr), zap.Error(err))
		return nil
	}
	logger.Info("redis client connected", zap.String("addr", addr))
	return client
}

func newKMSSvc(s *keystore.Store, logger *zap.Logger, rc *redis.Client, v *viper.Viper) *service.KMSService {
	svc := service.NewKMSService(s, logger)

	// Attach Redis cache if available
	if rc != nil {
		ttl := v.GetDuration("redis.cache_ttl")
		if ttl == 0 {
			ttl = 5 * time.Minute
		}
		// Generate ephemeral key for at-rest encryption
		ephemeralKey := make([]byte, 32)
		if _, err := rand.Read(ephemeralKey); err != nil {
			logger.Error("failed to generate ephemeral key", zap.Error(err))
		} else {
			redisCache, err := service.NewRedisDecryptCache(rc, ttl, ephemeralKey, logger)
			if err != nil {
				logger.Error("failed to create redis cache", zap.Error(err))
			} else {
				svc.SetRedisCache(redisCache)
				logger.Info("redis decrypt cache enabled", zap.Duration("ttl", ttl))
			}
		}
	}

	return svc
}

// kitexImpl 把 internal/server.Server 适配成 Kitex kmsv1.KMSServiceServer.
// 老 internal/server.Server 已实现 5 个 RPC 方法 (Encrypt/Decrypt/...), 签名跟 Kitex
// 生成的接口形态一致 (ctx + *pbReq → *pbResp + error); 直接复用业务逻辑.
type kitexImpl struct {
	inner *kmsimpl.Server
}

func (k *kitexImpl) Encrypt(ctx context.Context, req *kmsv1.EncryptRequest) (*kmsv1.EncryptResponse, error) {
	return k.inner.Encrypt(ctx, req)
}
func (k *kitexImpl) Decrypt(ctx context.Context, req *kmsv1.DecryptRequest) (*kmsv1.DecryptResponse, error) {
	return k.inner.Decrypt(ctx, req)
}
func (k *kitexImpl) GenerateDataKey(ctx context.Context, req *kmsv1.GenerateDataKeyRequest) (*kmsv1.GenerateDataKeyResponse, error) {
	return k.inner.GenerateDataKey(ctx, req)
}
func (k *kitexImpl) DescribeKey(ctx context.Context, req *kmsv1.DescribeKeyRequest) (*kmsv1.DescribeKeyResponse, error) {
	return k.inner.DescribeKey(ctx, req)
}
func (k *kitexImpl) ListKeys(ctx context.Context, req *kmsv1.ListKeysRequest) (*kmsv1.ListKeysResponse, error) {
	return k.inner.ListKeys(ctx, req)
}

func newServer(svc *service.KMSService, v *viper.Viper, cli *configcenter.Client, logger *zap.Logger) (server.Server, error) {
	tokens := map[string]string{}
	for _, t := range v.GetStringSlice("auth.tokens") {
		tokens[t] = "ok"
	}
	// SAN whitelist + rate_limit 100% 走 config-center; 不可达 → hardcoded safe default.
	var allowed []string
	rps := 500.0
	burst := 1000
	if cli != nil {
		ctx := context.Background()
		allowed = cli.GetStringList(ctx, "auth.allowed_client_ids", nil)
		rps = cli.GetFloat64(ctx, "rate_limit.rps", rps)
		burst = cli.GetInt(ctx, "rate_limit.burst", burst)
	}
	// 把老 grpc-based impl 包到 kitexImpl, 复用业务逻辑 + middleware 走 kitexutil.
	innerSrv, err := kmsimpl.NewServer(kmsimpl.Deps{
		KMSSvc:     svc,
		AuthTokens: tokens,
		AllowedIDs: allowed,
		TLS: kmsimpl.TLSPaths{
			ServerCert: v.GetString("tls.server_cert"),
			ServerKey:  v.GetString("tls.server_key"),
			ClientCA:   v.GetString("tls.client_ca"),
		},
		RateLimitRPS: rps,
		RateBurst:    burst,
		Logger:       logger,
	})
	if err != nil {
		return nil, err
	}

	port := v.GetInt("server.grpc_port")
	if port == 0 {
		port = 9290
	}
	addr, _ := net.ResolveTCPAddr("tcp", fmt.Sprintf(":%d", port))

	// Kitex middleware 链 — 待 kitexutil.MultiAuthMW 接通后, 把 tokens map[string]string
	// 传进去做轮询验. 目前 AuthMW 还是 stub, 不读 tokens 内容.
	_ = tokens
	srv := kmsservice.NewServer(
		&kitexImpl{inner: innerSrv},
		server.WithServiceAddr(addr),
		// 接 kitexutil MW; 等 Kitex middleware 形态对齐后 server.WithMiddleware(...) 直接装.
		// TODO: kitexutil.RateLimitMW(rps, burst) — 跟老 RateLimitInterceptor 等价
		// TODO: kitexutil.SANAllowMW(allowed) — 跟老 ClientIdentityInterceptor 等价
		// 当前 stub 占位, 等 Kitex impl 验证后接上.
	)

	// rate_limit 热更 — 现阶段 kitexutil.RateLimitMW 没接, 仅 log.
	// 真实接好后改成 mw.SetLimit(r, b) 替换占位.
	if cli != nil {
		apply := func(_ *configcenter.ConfigValue) {
			ctx := context.Background()
			r := cli.GetFloat64(ctx, "rate_limit.rps", 500.0)
			b := cli.GetInt(ctx, "rate_limit.burst", 1000)
			logger.Info("kms rate_limit hot-reload (TODO wire to kitexutil.RateLimitMW)",
				zap.Float64("rps", r), zap.Int("burst", b))
		}
		cli.OnChange("rate_limit.rps", apply)
		cli.OnChange("rate_limit.burst", apply)
	}
	_ = kitexutil.LogMW // 防 import 未用; newServer 实际中间件接好后去掉
	return srv, nil
}

func startGRPC(lc fx.Lifecycle, s server.Server, v *viper.Viper, logger *zap.Logger) {
	port := v.GetInt("server.grpc_port")
	if port == 0 {
		port = 9290
	}
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			logger.Info("kms-manage Kitex listening", zap.Int("port", port))
			go func() {
				if err := s.Run(); err != nil {
					logger.Error("kitex serve exited", zap.Error(err))
				}
			}()
			return nil
		},
		OnStop: func(_ context.Context) error {
			if err := s.Stop(); err != nil {
				logger.Warn("kitex stop error", zap.Error(err))
			}
			return nil
		},
	})
}

func startMetricsHTTP(lc fx.Lifecycle, v *viper.Viper, ks *keystore.Store, logger *zap.Logger) {
	addr := v.GetString("metrics.addr")
	if addr == "" {
		addr = ":9390"
	}
	// readiness probe: 报告 active key + 总加载 key 数；keystore 不可用时 fail。
	probe := func() (string, int, error) {
		active := ks.ActiveKeyID()
		list := ks.List()
		return active, len(list), nil
	}
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			metrics.StartServer(addr, logger, probe)
			return nil
		},
	})
}
