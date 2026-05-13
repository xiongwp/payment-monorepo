// Package usermerchantclient 给 order-core 用的 user-merchant-core 客户端补充：
// 在 Confirm 路径根据 (user_id, user_card_id) 查 stored_token + masked_pan + network。
//
// 注意：返回的 stored_token 仅在 order-core 内 Confirm 函数 stack 内出现，
// 拿到后立即用来调 card-center.CreatePaymentToken,绝不外发 / 不存。
package usermerchantclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	usermerchantv1 "github.com/xiongwp/user-merchant-core/api/proto/usermerchant/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// CardLookup 接口：仅为 order-core 内 Confirm 路径定义。生产实现走 mTLS gRPC
// 调 user-merchant-core 的内部 RPC（user-merchant-core 暴露专门的 internal-only
// 服务,CN 白名单仅含 order-core）。
type CardLookup interface {
	GetStoredTokenForPayment(ctx context.Context, userID, userCardID int64) (storedToken, maskedPAN, network string, err error)
}

// Config gRPC 连接配置
type Config struct {
	Endpoint   string        // user-merchant-core internal port (mTLS)
	RPCTimeout time.Duration // 默认 3s
	ClientCert string
	ClientKey  string
	ServerCA   string
	Insecure   bool // 仅 dev/test
}

// grpcLookup 真实实现
type grpcLookup struct {
	conn    *grpc.ClientConn
	cli     usermerchantv1.UserCardInternalServiceClient
	timeout time.Duration
}

// New 构造真实 client.
func New(cfg Config) (CardLookup, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("usermerchantclient: endpoint required")
	}
	var creds credentials.TransportCredentials
	if cfg.Insecure {
		creds = insecure.NewCredentials()
	} else {
		tc, err := buildTLS(cfg)
		if err != nil {
			return nil, err
		}
		creds = credentials.NewTLS(tc)
	}
	conn, err := grpc.NewClient(cfg.Endpoint, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	t := cfg.RPCTimeout
	if t <= 0 {
		t = 3 * time.Second
	}
	return &grpcLookup{
		conn:    conn,
		cli:     usermerchantv1.NewUserCardInternalServiceClient(conn),
		timeout: t,
	}, nil
}

func (g *grpcLookup) GetStoredTokenForPayment(
	ctx context.Context, userID, userCardID int64,
) (string, string, string, error) {
	if userID == 0 || userCardID == 0 {
		return "", "", "", errors.New("user_id / user_card_id required")
	}
	cctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	resp, err := g.cli.GetStoredTokenForPayment(cctx, &usermerchantv1.GetStoredTokenRequest{
		UserId:     strconv.FormatInt(userID, 10),
		UserCardId: strconv.FormatInt(userCardID, 10),
	})
	if err != nil {
		return "", "", "", fmt.Errorf("GetStoredTokenForPayment: %w", err)
	}
	return resp.GetStoredToken(), resp.GetMaskedPan(), resp.GetNetwork(), nil
}

func buildTLS(cfg Config) (*tls.Config, error) {
	if cfg.ClientCert == "" || cfg.ClientKey == "" {
		return nil, errors.New("client_cert / client_key required")
	}
	cert, err := tls.LoadX509KeyPair(cfg.ClientCert, cfg.ClientKey)
	if err != nil {
		return nil, fmt.Errorf("client keypair: %w", err)
	}
	out := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	if cfg.ServerCA != "" {
		pool := x509.NewCertPool()
		caBytes, err := os.ReadFile(cfg.ServerCA)
		if err != nil {
			return nil, fmt.Errorf("server CA: %w", err)
		}
		if !pool.AppendCertsFromPEM(caBytes) {
			return nil, fmt.Errorf("server CA PEM parse failed")
		}
		out.RootCAs = pool
	}
	return out, nil
}

// stubLookup 仅 dev / unit-test 用 — 返回固定假值,生产代码绝不该走这条路径.
type stubLookup struct {
	storedToken string
	maskedPAN   string
	network     string
}

// NewStub 给单测用。可选传入 (storedToken, maskedPAN, network),空则报错。
func NewStub(storedToken, maskedPAN, network string) CardLookup {
	return &stubLookup{
		storedToken: storedToken,
		maskedPAN:   maskedPAN,
		network:     network,
	}
}

func (s *stubLookup) GetStoredTokenForPayment(ctx context.Context, userID, userCardID int64) (string, string, string, error) {
	_ = ctx
	if userID == 0 || userCardID == 0 {
		return "", "", "", errors.New("user_id / user_card_id required")
	}
	if s.storedToken == "" {
		return "", "", "", errors.New("usermerchantclient: stub not configured (test only)")
	}
	return s.storedToken, s.maskedPAN, s.network, nil
}
