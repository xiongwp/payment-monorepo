// Command server 启动 card-center gRPC over mTLS。
//
// 部署在隔离 DC（SAQ-D scope）：
//   - 仅 mTLS 入站
//   - 出站只到 KMS / Kafka audit / 自有 MySQL（card_center_db_*）
//   - 启动期校验 env=prod 时 TLS / KMS / Kafka 全部必填
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"
	"go.uber.org/fx"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/xiongwp/card-center/internal/audit"
	"github.com/xiongwp/card-center/internal/kmsclient"
	"github.com/xiongwp/card-center/internal/repo"
	"github.com/xiongwp/card-center/internal/server"
	"github.com/xiongwp/card-center/internal/service"
	"github.com/xiongwp/card-center/internal/sharding"
	"github.com/xiongwp/card-center/internal/vault"
	"github.com/xiongwp/payment-util/shadow"
	"github.com/xiongwp/payment-util/trace"
)

func main() {
	app := fx.New(
		fx.StartTimeout(30*time.Second),
		fx.StopTimeout(30*time.Second),
		fx.Provide(
			loadConfig,
			newLogger,
			newRouter,
			newDBManager,
			newKMSClient,
			newVault,
			newStoredCardRepo,
			newPaymentTokenRepo,
			newAuditEmitter,
			newService,
			newGRPCServer,
		),
		fx.Invoke(startGRPC),
	)
	app.Run()
}

func loadConfig() (*viper.Viper, error) {
	v := viper.New()
	v.SetEnvPrefix("CARDCENTER")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	for _, k := range []string{"env", "kms.endpoint", "tls.cert", "tls.key", "tls.client_ca",
		"audit.kafka_brokers", "database.meta.dsn"} {
		_ = v.BindEnv(k)
	}
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath("./config")
	v.AddConfigPath(".")
	v.AddConfigPath("/etc/card-center")
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

func assertProdSafety(v *viper.Viper) error {
	env := strings.ToLower(strings.TrimSpace(v.GetString("env")))
	if env != "prod" && env != "production" {
		return nil
	}
	for _, k := range []string{"tls.cert", "tls.key", "tls.client_ca"} {
		if strings.TrimSpace(v.GetString(k)) == "" {
			return fmt.Errorf("PROD-SAFETY: %s must be configured (mTLS-only)", k)
		}
	}
	if strings.TrimSpace(v.GetString("kms.endpoint")) == "" {
		return fmt.Errorf("PROD-SAFETY: kms.endpoint must be configured")
	}
	if len(v.GetStringSlice("audit.kafka_brokers")) == 0 {
		return fmt.Errorf("PROD-SAFETY: audit.kafka_brokers must be configured (audit cannot be lost)")
	}
	if strings.TrimSpace(v.GetString("database.meta.dsn")) == "" {
		return fmt.Errorf("PROD-SAFETY: database.meta.dsn required")
	}
	for i := 0; i < sharding.ShardDBCount; i++ {
		if strings.TrimSpace(v.GetString(fmt.Sprintf("database.shard_%d.dsn", i))) == "" {
			return fmt.Errorf("PROD-SAFETY: database.shard_%d.dsn required", i)
		}
	}
	return nil
}

func newLogger() (*zap.Logger, error) {
	if os.Getenv("CARDCENTER_LOG_DEV") == "true" {
		return zap.NewDevelopment()
	}
	return zap.NewProduction()
}

func newRouter() *sharding.Router { return sharding.NewRouter() }

func newDBManager(v *viper.Viper, router *sharding.Router, logger *zap.Logger) (*repo.Manager, error) {
	var meta repo.DBConfig
	_ = v.UnmarshalKey("database.meta", &meta)
	shards := make([]repo.DBConfig, sharding.ShardDBCount)
	for i := 0; i < sharding.ShardDBCount; i++ {
		key := fmt.Sprintf("database.shard_%d", i)
		var s repo.DBConfig
		_ = v.UnmarshalKey(key, &s)
		shards[i] = s
	}
	return repo.NewManager(meta, shards, router, logger)
}

func newKMSClient(v *viper.Viper) (vault.KMS, error) {
	cfg := kmsclient.Config{
		Endpoint:    v.GetString("kms.endpoint"),
		BearerToken: v.GetString("kms.bearer_token"),
		RPCTimeout:  v.GetDuration("kms.rpc_timeout"),
		ClientCert:  v.GetString("kms.client_cert"),
		ClientKey:   v.GetString("kms.client_key"),
		ServerCA:    v.GetString("kms.server_ca"),
		Insecure:    v.GetBool("kms.insecure"),
	}
	if cfg.Endpoint == "" {
		return nil, errors.New("kms.endpoint required")
	}
	return kmsclient.New(cfg)
}

func newVault(kms vault.KMS) *vault.Vault { return vault.NewVault(kms, time.Now) }

func newStoredCardRepo(mgr *repo.Manager) repo.StoredCardRepo {
	return repo.NewStoredCardRepo(mgr)
}

func newPaymentTokenRepo(mgr *repo.Manager) repo.PaymentTokenRepo {
	return repo.NewPaymentTokenRepo(mgr)
}

// newAuditEmitter 构造 audit emitter；Kafka producer 这里先注入 nil，
// service 层只走 DB 落盘。生产引入 sarama / kafka-go 后这里替换。
func newAuditEmitter(mgr *repo.Manager, v *viper.Viper, logger *zap.Logger) service.AuditEmitter {
	topic := v.GetString("audit.topic")
	if topic == "" {
		topic = "card-center.audit"
	}
	// TODO: 接 sarama 的 SyncProducer，实现 audit.KafkaProducer
	return audit.New(nil, topic, mgr.Meta(), logger)
}

func newService(v *vault.Vault, sr repo.StoredCardRepo, pr repo.PaymentTokenRepo, ae service.AuditEmitter, logger *zap.Logger) *service.Service {
	return service.NewService(v, sr, pr, ae, logger)
}

// newGRPCServer 构造带 mTLS + 业务 interceptor 链的 grpc.Server
func newGRPCServer(v *viper.Viper, svc *service.Service, logger *zap.Logger) (*grpc.Server, *server.Server, error) {
	tlsCfg, err := buildTLSConfig(v)
	if err != nil {
		return nil, nil, fmt.Errorf("tls config: %w", err)
	}
	allowMap := v.GetStringMapStringSlice("auth.client_cn")
	allow := server.NewClientCNAllowList(allowMap)

	srv := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsCfg)),
		grpc.ChainUnaryInterceptor(
			trace.UnaryServerInterceptor(logger),
			shadow.UnaryServerInterceptor(),
			server.UnaryClientCNInterceptor(allow),
		),
	)
	bs := server.NewServer(svc, logger)
	bs.Register(srv)
	return srv, bs, nil
}

func startGRPC(lc fx.Lifecycle, srv *grpc.Server, _ *server.Server, v *viper.Viper, logger *zap.Logger) error {
	port := v.GetInt("server.grpc_port")
	if port == 0 {
		port = 9443
	}
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}
	logger.Info("card-center mTLS gRPC listening", zap.Int("port", port))
	go func() {
		if err := srv.Serve(lis); err != nil {
			logger.Error("grpc serve", zap.Error(err))
		}
	}()
	lc.Append(fx.Hook{
		OnStop: func(ctx context.Context) error {
			done := make(chan struct{})
			go func() { srv.GracefulStop(); close(done) }()
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				srv.Stop()
			}
			return nil
		},
	})
	return nil
}

func buildTLSConfig(v *viper.Viper) (*tls.Config, error) {
	certPath := v.GetString("tls.cert")
	keyPath := v.GetString("tls.key")
	caPath := v.GetString("tls.client_ca")
	if certPath == "" || keyPath == "" {
		return nil, errors.New("tls.cert / tls.key required")
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("server keypair: %w", err)
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	if caPath != "" {
		pool := x509.NewCertPool()
		caBytes, err := os.ReadFile(caPath)
		if err != nil {
			return nil, fmt.Errorf("client_ca: %w", err)
		}
		if !pool.AppendCertsFromPEM(caBytes) {
			return nil, fmt.Errorf("client_ca PEM parse failed")
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}
