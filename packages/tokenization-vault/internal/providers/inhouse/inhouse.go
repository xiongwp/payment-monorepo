// Package inhouse — 自建 vault provider (无网络代币 fallback).
//
// 用于:
//   - 卡组不支持 network token 的 BIN (个别国际卡 / 小众 brand)
//   - VTS/MDES API 故障期间 (provider 转 inhouse 兜底)
//
// "network_token" 在 inhouse 模式下其实就是 internal_token 本身 (一对一 PAN);
// "cryptogram" 不存在, ECI = "07" (non-3DS).
//
// 这条路径走真 PAN 推 PSP, 需要 PCI Level 1 合规, 慎用.

package inhouse

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"reconcile-system/packages/tokenization-vault/internal/domain"
	"reconcile-system/packages/tokenization-vault/internal/providers"
)

type Provider struct{}

func New() *Provider { return &Provider{} }

func (p *Provider) Name() domain.TokenProvider { return domain.ProviderInhouse }

func (p *Provider) Provision(_ context.Context, in providers.ProvisionIn) (providers.ProvisionOut, error) {
	h := sha256.Sum256([]byte("inhouse|" + in.PAN))
	ref := "in_" + hex.EncodeToString(h[:8])
	exp := time.Now().AddDate(2, 0, 0)
	expiry := fmt.Sprintf("%02d%02d", exp.Month(), exp.Year()%100)
	return providers.ProvisionOut{
		NetworkToken: ref, // inhouse 没有真 NT, 用 ref 代替
		TokenRefID:   ref,
		TokenExpiry:  expiry,
		PAR:          "",
	}, nil
}

// Cryptogram inhouse 不出 cryptogram, 直接返空字符串 + ECI=07.
// 实际推卡组用真 PAN; 由调用方从 vault 走 ChargeIntent 把 NetworkToken 换 PAN.
func (p *Provider) Cryptogram(_ context.Context, _ providers.CryptogramIn) (providers.CryptogramOut, error) {
	return providers.CryptogramOut{Cryptogram: "", ECI: "07", ATC: 0}, nil
}

func (p *Provider) Suspend(_ context.Context, _ string) error { return nil }
func (p *Provider) Resume(_ context.Context, _ string) error  { return nil }
func (p *Provider) Delete(_ context.Context, _ string) error  { return nil }
