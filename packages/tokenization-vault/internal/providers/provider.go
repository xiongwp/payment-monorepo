// Package providers — 网络代币 provider 接口.
//
// 一个 Provider 实现要做的事:
//   1. Provision(pan) → network_token, ref_id, expiry — 注册 PAN 到 VTS/MDES
//   2. GenerateCryptogram(network_token, amount, currency, MIT/CIT) → cryptogram + ECI
//   3. Suspend(ref_id) / Resume(ref_id) — 生命周期
//   4. RetrievePAR(ref_id) → PAR (Payment Account Reference, 跨 token 关联)
//
// 真生产实现:
//   - vts/    Visa Token Service (mTLS + JWT, ASN.1 ECC cryptogram)
//   - mdes/   Mastercard Digital Enablement Service (mTLS + signed JWS)
//   - inhouse/ 自建 PCI vault (没有真 network token, 直接保 PAN + 用 issuer 网关 cryptogram)

package providers

import (
	"context"
	"errors"

	"reconcile-system/packages/tokenization-vault/internal/domain"
)

var (
	ErrCardDeclined = errors.New("provider: card declined")
	ErrNoProvider   = errors.New("provider: no provider for brand")
	ErrTransient    = errors.New("provider: transient error, retry")
)

// Provider 网络代币 provider 抽象.
type Provider interface {
	Name() domain.TokenProvider

	// Provision 把 PAN 注册到 VTS/MDES 拿 network token.
	// 同一 PAN 多次 provision 应 idempotent (返回同 network_token).
	Provision(ctx context.Context, in ProvisionIn) (ProvisionOut, error)

	// Cryptogram 给一次扣款生成 (cryptogram, ECI).
	// CIT (recurring=false) 用 Card-on-File initial; MIT 用 stored credential.
	Cryptogram(ctx context.Context, in CryptogramIn) (CryptogramOut, error)

	// Lifecycle
	Suspend(ctx context.Context, tokenRefID string) error
	Resume(ctx context.Context, tokenRefID string) error
	Delete(ctx context.Context, tokenRefID string) error
}

type ProvisionIn struct {
	PAN         string
	ExpMonth    int
	ExpYear     int
	Cardholder  string
	MerchantID  string
	// 卡组要的额外 attestation (visa requires device_data for in-app, etc)
	DeviceData  map[string]string
}

type ProvisionOut struct {
	NetworkToken string
	TokenRefID   string
	TokenExpiry  string // MMYY
	PAR          string // Payment Account Reference (visa/mc 都给)
}

type CryptogramIn struct {
	TokenRefID  string
	Amount      int64  // 分
	Currency    string // ISO 4217
	Recurring   bool
	MIT         MITKind // 如果 Recurring=true
	IntentID    string  // payment-core 的幂等 ID, 给卡组 trace
}

type MITKind string

const (
	MITNone           MITKind = ""
	MITRecurring      MITKind = "recurring"      // 周期订阅
	MITUnscheduled    MITKind = "unscheduled"    // top-up 不定期
	MITInstallment    MITKind = "installment"    // 分期
	MITNoShow         MITKind = "no_show"        // 酒店 no-show
	MITDelayedCharge  MITKind = "delayed_charge" // 后置消费
	MITReauth         MITKind = "reauth"         // 重新授权
)

type CryptogramOut struct {
	Cryptogram string // base64 (visa TAVV / mc UCAF)
	ECI        string // visa 05/06, mc 02
	ATC        int    // ATC counter (some providers expose)
}
