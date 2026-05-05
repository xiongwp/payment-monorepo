// Package cardcenterclient mTLS gRPC 调 card-center 服务（专用于派生支付 token）。
//
// order-core 在 PI Confirm 路径调 CreatePaymentToken：把 user_card 的 stored_token
// 转成绑定 pi_id 的一次性支付 token（TTL 30min），传给 payment-channel.adapter[card]
// → card-payment → card-center.Detokenize → PAN → 卡组织。
package cardcenterclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// Client 调 card-center 的 gRPC 客户端
type Client struct {
	conn    *grpc.ClientConn
	timeout time.Duration
}

// Config
type Config struct {
	Endpoint   string
	RPCTimeout time.Duration
	ClientCert string
	ClientKey  string
	ServerCA   string
	Insecure   bool
}

// CreatePaymentTokenRequest 业务层请求
type CreatePaymentTokenRequest struct {
	StoredToken string
	UserID      int64
	PIID        string
	Amount      int64
	Currency    string
	TTL         time.Duration
	TraceID     string
}

// CreatePaymentTokenResponse
type CreatePaymentTokenResponse struct {
	PaymentToken string
	ExpiresAt    time.Time
	MaskedPAN    string
	Network      string
}

// New
func New(cfg Config) (*Client, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("cardcenterclient: endpoint required")
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
		t = 5 * time.Second
	}
	return &Client{conn: conn, timeout: t}, nil
}

// Close
func (c *Client) Close() error { return c.conn.Close() }

// CreatePaymentToken 调 card-center.CreatePaymentToken
//
// TODO: 接通 cardcenterv1 generated stubs：
//
//	cli := cardcenterv1.NewCardCenterClient(c.conn)
//	resp, err := cli.CreatePaymentToken(cctx, &cardcenterv1.CreatePaymentTokenRequest{
//	    StoredToken: req.StoredToken,
//	    UserId:      strconv.FormatInt(req.UserID, 10),
//	    PiId:        req.PIID,
//	    Amount:      req.Amount,
//	    Currency:    req.Currency,
//	    TtlSeconds:  int32(req.TTL.Seconds()),
//	    TraceId:     req.TraceID,
//	})
//	if err != nil { return nil, err }
//	return &CreatePaymentTokenResponse{
//	    PaymentToken: resp.PaymentToken,
//	    ExpiresAt:    time.Unix(resp.ExpiresAt, 0),
//	    MaskedPAN:    resp.MaskedPan,
//	    Network:      resp.Network,
//	}, nil
func (c *Client) CreatePaymentToken(ctx context.Context, req *CreatePaymentTokenRequest) (*CreatePaymentTokenResponse, error) {
	cctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	_ = cctx
	return nil, errors.New("cardcenterclient: TODO wire cardcenterv1 stubs")
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
