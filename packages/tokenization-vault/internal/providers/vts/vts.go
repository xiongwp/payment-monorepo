// Package vts — Visa Token Service adapter.
//
// 生产:
//   - mTLS 到 sandbox.api.visa.com / api.visa.com
//   - VTS Provisioning API: POST /vts/payment-instruments
//     输入: PAN + cardholder + device_data
//     输出: provisionedTokenInfo { token, expirationDate, lastFour, ... }
//   - VTS Cryptogram API: POST /vts/payment-instruments/{tokenId}/cryptogram
//     输入: amount, currency, recurring flag
//     输出: tavv (cryptogram), eci, atc
//
// 这里实现一个 stub — 同接口契约, 但生成本地确定性的 token/cryptogram 给 dev / smoke 用.
// 接生产时只换 transport (http.Client → VTS endpoint).

package vts

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"reconcile-system/packages/tokenization-vault/internal/domain"
	"reconcile-system/packages/tokenization-vault/internal/providers"
)

// Provider VTS provider (stub for dev; 真 prod 换 HTTP transport).
type Provider struct {
	// 生产: mTLS client + endpoint + apiKey + apiSecret
	signingKey []byte // 给 cryptogram HMAC 签名用 (dev stub)
	atc        atomic.Int64
}

func New(signingKey []byte) *Provider {
	if len(signingKey) == 0 {
		signingKey = []byte("vts-dev-stub-key-CHANGE-IN-PROD")
	}
	return &Provider{signingKey: signingKey}
}

func (p *Provider) Name() domain.TokenProvider { return domain.ProviderVTS }

// Provision: dev stub — network_token 用 PAN sha256 前 16 字节生成稳定 DPAN.
// 真 prod: HTTPS POST 到 VTS, 解析 provisionedTokenInfo.
func (p *Provider) Provision(_ context.Context, in providers.ProvisionIn) (providers.ProvisionOut, error) {
	if len(in.PAN) < 13 || len(in.PAN) > 19 {
		return providers.ProvisionOut{}, fmt.Errorf("vts: bad pan length %d", len(in.PAN))
	}
	// stable token derivation
	h := sha256.Sum256([]byte("vts|" + in.PAN))
	dpanRaw := hex.EncodeToString(h[:])[:16] // 16 hex → 16 chars 不是真卡号
	// 真 VTS DPAN 是真 PAN-like (Luhn 合法 16-19 位); 这里用 hex 16 字符占位.
	networkToken := "DPAN-" + dpanRaw
	tokenRefID := "tref_" + hex.EncodeToString(h[16:24])
	par := "V0000" + hex.EncodeToString(h[24:32]) // 29 char Visa PAR format (loose)

	// 过期: 2 年后
	exp := time.Now().AddDate(2, 0, 0)
	expiry := fmt.Sprintf("%02d%02d", exp.Month(), exp.Year()%100)
	return providers.ProvisionOut{
		NetworkToken: networkToken,
		TokenRefID:   tokenRefID,
		TokenExpiry:  expiry,
		PAR:          strings.ToUpper(par)[:29],
	}, nil
}

// Cryptogram: dev stub — HMAC(signing_key, tokenRefID|amount|currency|intent|atc).
// 真 prod: HTTPS POST VTS, 拿 TAVV.
func (p *Provider) Cryptogram(_ context.Context, in providers.CryptogramIn) (providers.CryptogramOut, error) {
	atc := int(p.atc.Add(1))
	msg := fmt.Sprintf("%s|%d|%s|%s|%d|%t", in.TokenRefID, in.Amount, in.Currency, in.IntentID, atc, in.Recurring)
	mac := hmac.New(sha256.New, p.signingKey)
	mac.Write([]byte(msg))
	tavv := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	// ECI: visa 05 = SCA enforced, 06 = attempted, 07 = non-3DS.
	eci := "05"
	if in.Recurring {
		eci = "07" // recurring 通常 ECI 07 (无 SCA)
	}
	return providers.CryptogramOut{
		Cryptogram: tavv,
		ECI:        eci,
		ATC:        atc,
	}, nil
}

func (p *Provider) Suspend(_ context.Context, _ string) error { return nil }
func (p *Provider) Resume(_ context.Context, _ string) error  { return nil }
func (p *Provider) Delete(_ context.Context, _ string) error  { return nil }
