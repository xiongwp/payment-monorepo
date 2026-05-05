// Command server 启动 card-payment gRPC over mTLS.
//
// 部署在隔离 DC（SAQ-D scope）。仅 mTLS 入站。出站只允许：
//   - mTLS gRPC 到 card-center (Detokenize)
//   - HTTPS 到卡组织（Visa Net / Mastercard MIP / ...）
//   - 自有 MySQL（card_payment_db_*）
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

	"github.com/xiongwp/card-payment/internal/adapter/amex"
	"github.com/xiongwp/card-payment/internal/adapter/jcb"
	"github.com/xiongwp/card-payment/internal/adapter/mastercard"
	"github.com/xiongwp/card-payment/internal/adapter/unionpay"
	"github.com/xiongwp/card-payment/internal/adapter/visa"
	"github.com/xiongwp/card-payment/internal/cardcenterclient"
	"github.com/xiongwp/card-payment/internal/processor"
	"github.com/xiongwp/card-payment/internal/repo"
	"github.com/xiongwp/card-payment/internal/server"
	"github.com/xiongwp/card-payment/internal/sharding"
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
			newCardTransactionRepo,
			newCardCenterClient,
			newNetworks,
			newProcessor,
			newGRPCServer,
		),
		fx.Invoke(startGRPC),
	)
	app.Run()
}

func loadConfig() (*viper.Viper, error) {
	v := viper.New()
	v.SetEnvPrefix("CARDPAYMENT")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	for _, k := range []string{"env", "card_center.endpoint", "tls.cert", "tls.key", "tls.client_ca",
		"network.visa.endpoint", "network.mastercard.endpoint", "network.jcb.endpoint",
		"network.amex.endpoint", "network.unionpay.endpoint",
		"database.meta.dsn"} {
		_ = v.BindEnv(k)
	}
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath("./config")
	v.AddConfigPath(".")
	v.AddConfigPath("/etc/card-payment")
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
			return fmt.Errorf("PROD-SAFETY: %s required (mTLS-only)", k)
		}
	}
	if strings.TrimSpace(v.GetString("card_center.endpoint")) == "" {
		return fmt.Errorf("PROD-SAFETY: card_center.endpoint required")
	}
	atLeastOne := false
	for _, n := range []string{"visa", "mastercard", "jcb", "amex", "unionpay"} {
		ep := v.GetString("network." + n + ".endpoint")
		if ep == "" {
			continue
		}
		if !strings.HasPrefix(ep, "https://") {
			return fmt.Errorf("PROD-SAFETY: network.%s.endpoint must use https://", n)
		}
		atLeastOne = true
	}
	if !atLeastOne {
		return fmt.Errorf("PROD-SAFETY: at least one network adapter required")
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
	if os.Getenv("CARDPAYMENT_LOG_DEV") == "true" {
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
		var s repo.DBConfig
		_ = v.UnmarshalKey(fmt.Sprintf("database.shard_%d", i), &s)
		shards[i] = s
	}
	return repo.NewManager(meta, shards, router, logger)
}

func newCardTransactionRepo(mgr *repo.Manager) processor.CardTransactionRepo {
	return repo.NewCardTransactionRepo(mgr)
}

func newCardCenterClient(v *viper.Viper) (processor.CardCenter, error) {
	cfg := cardcenterclient.Config{
		Endpoint:   v.GetString("card_center.endpoint"),
		RPCTimeout: v.GetDuration("card_center.rpc_timeout"),
		ClientCert: v.GetString("card_center.client_cert"),
		ClientKey:  v.GetString("card_center.client_key"),
		ServerCA:   v.GetString("card_center.server_ca"),
		Insecure:   v.GetBool("card_center.insecure"),
	}
	if cfg.Endpoint == "" {
		return nil, errors.New("card_center.endpoint required")
	}
	return cardcenterclient.New(cfg)
}

// newNetworks 构造 5 个 network adapter map（visa / mastercard / jcb / amex / unionpay）。
//
// 每个 adapter：endpoint 为空 或 env != prod 时走 mock。生产 endpoint 必须 https://，
// 由 assertProdSafety 校验。
func newNetworks(v *viper.Viper, logger *zap.Logger) map[string]processor.Network {
	mockMode := strings.ToLower(v.GetString("env")) != "prod" && strings.ToLower(v.GetString("env")) != "production"
	out := make(map[string]processor.Network, 5)

	visaEP := v.GetString("network.visa.endpoint")
	out["visa"] = visa.New(visa.Config{
		Endpoint: visaEP, APIKey: v.GetString("network.visa.api_key"),
		Cert: v.GetString("network.visa.cert"), Timeout: v.GetDuration("network.visa.timeout"),
		Mock: mockMode || visaEP == "",
	}, logger)

	mcEP := v.GetString("network.mastercard.endpoint")
	out["mastercard"] = mastercard.New(mastercard.Config{
		Endpoint: mcEP, APIKey: v.GetString("network.mastercard.api_key"),
		Cert: v.GetString("network.mastercard.cert"), Timeout: v.GetDuration("network.mastercard.timeout"),
		Mock: mockMode || mcEP == "",
	}, logger)

	jcbEP := v.GetString("network.jcb.endpoint")
	out["jcb"] = jcb.New(jcb.Config{
		Endpoint: jcbEP, APIKey: v.GetString("network.jcb.api_key"),
		Cert: v.GetString("network.jcb.cert"), Timeout: v.GetDuration("network.jcb.timeout"),
		Mock: mockMode || jcbEP == "",
	}, logger)

	amexEP := v.GetString("network.amex.endpoint")
	out["amex"] = amex.New(amex.Config{
		Endpoint: amexEP, APIKey: v.GetString("network.amex.api_key"),
		Cert: v.GetString("network.amex.cert"), Timeout: v.GetDuration("network.amex.timeout"),
		Mock: mockMode || amexEP == "",
	}, logger)

	upEP := v.GetString("network.unionpay.endpoint")
	out["unionpay"] = unionpay.New(unionpay.Config{
		Endpoint: upEP, APIKey: v.GetString("network.unionpay.api_key"),
		Cert: v.GetString("network.unionpay.cert"), Timeout: v.GetDuration("network.unionpay.timeout"),
		Mock: mockMode || upEP == "",
	}, logger)

	return out
}

func newProcessor(cc processor.CardCenter, networks map[string]processor.Network, repo processor.CardTransactionRepo, logger *zap.Logger) *processor.Processor {
	return processor.NewProcessor(cc, networks, repo, logger)
}

func newGRPCServer(v *viper.Viper, p *processor.Processor, logger *zap.Logger) (*grpc.Server, error) {
	tlsCfg, err := buildTLSConfig(v)
	if err != nil {
		return nil, fmt.Errorf("tls: %w", err)
	}
	allow := server.NewClientCNAllowList(v.GetStringSlice("auth.client_cn.allowed"))
	srv := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsCfg)),
		grpc.ChainUnaryInterceptor(
			trace.UnaryServerInterceptor(logger),
			shadow.UnaryServerInterceptor(),
			server.UnaryClientCNInterceptor(allow),
		),
	)
	bs := server.NewServer(p, logger)
	bs.Register(srv)
	return srv, nil
}

func startGRPC(lc fx.Lifecycle, srv *grpc.Server, v *viper.Viper, logger *zap.Logger) error {
	port := v.GetInt("server.grpc_port")
	if port == 0 {
		port = 9443
	}
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}
	logger.Info("card-payment mTLS gRPC listening", zap.Int("port", port))
	go func() {
		if err := srv.Serve(lis); err != nil {
			logger.Error("grpc serve", zap.Error(err))
		}
	}()
	lc.Append(fx.Hook{OnStop: func(ctx context.Context) error {
		done := make(chan struct{})
		go func() { srv.GracefulStop(); close(done) }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			srv.Stop()
		}
		return nil
	}})
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
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
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
