// Package usermerchantclient — 临时 STUB.
//
// 原版通过 gRPC + mTLS 调 user-merchant-core.UserCardInternalService.
// Kitex 切换 + cross-service kitex_gen 接入 docker build 完成后, 改成 Kitex client.
// 当前 stub: New 返 fail-closed implementation, NewStub 给单测用.
package usermerchantclient

import (
	"context"
	"errors"
	"time"
)

// CardLookup 接口 — 让现有调用方编译过.
type CardLookup interface {
	GetStoredTokenForPayment(ctx context.Context, userID, userCardID int64) (storedToken, maskedPAN, network string, err error)
}

// Config 保持原 signature 让 main.go 编过.
type Config struct {
	Endpoint   string
	RPCTimeout time.Duration
	ClientCert string
	ClientKey  string
	ServerCA   string
	Insecure   bool
}

// ErrNotWired 跨服务 Kitex client 未接通; 上游 caller 走降级 (拒卡支付).
var ErrNotWired = errors.New("usermerchantclient STUB: cross-service Kitex client not wired in build")

type stubFailLookup struct{ timeout time.Duration }

// New 暂返 fail-closed stub (任何 GetStoredTokenForPayment 调用直接报 ErrNotWired).
func New(cfg Config) (CardLookup, error) {
	t := cfg.RPCTimeout
	if t <= 0 {
		t = 3 * time.Second
	}
	return &stubFailLookup{timeout: t}, nil
}

func (s *stubFailLookup) GetStoredTokenForPayment(_ context.Context, userID, userCardID int64) (string, string, string, error) {
	if userID == 0 || userCardID == 0 {
		return "", "", "", errors.New("user_id / user_card_id required")
	}
	return "", "", "", ErrNotWired
}

// stubLookup 给单测用 — 返回固定假值.
type stubLookup struct{ storedToken, maskedPAN, network string }

// NewStub 给单测用.
func NewStub(storedToken, maskedPAN, network string) CardLookup {
	return &stubLookup{storedToken: storedToken, maskedPAN: maskedPAN, network: network}
}

func (s *stubLookup) GetStoredTokenForPayment(_ context.Context, userID, userCardID int64) (string, string, string, error) {
	if userID == 0 || userCardID == 0 {
		return "", "", "", errors.New("user_id / user_card_id required")
	}
	if s.storedToken == "" {
		return "", "", "", errors.New("usermerchantclient: stub not configured (test only)")
	}
	return s.storedToken, s.maskedPAN, s.network, nil
}
