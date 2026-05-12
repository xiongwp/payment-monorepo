// Package vault — vault 主编排逻辑 (Exchange / Provision / ChargeIntent).
//
// 业务流程:
//
//  1) Exchange(PAN) → internal_token:
//     - Luhn 校验
//     - 算 pan_hash (sha256[:32])
//     - 查 dedup: merchant_id + pan_hash 已有 token → 直接返
//     - 否则: generate internal_token = "tk_<env>_<32hex>"
//     - DEK 加密 PAN, 落 encrypted_pan 表
//     - 异步 Provision (VTS/MDES)
//
//  2) Provision(internal_token) → network_token:
//     - Get internal_token → decrypt PAN
//     - 按 brand 选 provider (visa→vts, mc→mdes, else→inhouse)
//     - provider.Provision(PAN, exp, cardholder) → NetworkRef
//     - store.UpdateNetworkRef
//
//  3) ChargeIntent(internal_token, amount, recurring) → (network_token, cryptogram):
//     - 取 internal_token; 校验 status=active, NetworkRef 已 provision
//     - provider.Cryptogram(token_ref_id, amount, recurring) → cryptogram
//     - 返回 (NetworkToken, Cryptogram, ECI)

package vault

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"reconcile-system/packages/tokenization-vault/internal/crypto"
	"reconcile-system/packages/tokenization-vault/internal/domain"
	"reconcile-system/packages/tokenization-vault/internal/providers"
	"reconcile-system/packages/tokenization-vault/internal/store"
)

var (
	ErrInvalidPAN    = errors.New("vault: invalid PAN (Luhn failed)")
	ErrNotProvisioned = errors.New("vault: token not provisioned to network")
	ErrInactive       = errors.New("vault: token not active")
	ErrNoProvider     = errors.New("vault: no provider for brand")
)

type Vault struct {
	Store     store.Store
	Providers map[domain.TokenProvider]providers.Provider
	DEK       []byte // 32 bytes; 真生产由 KMS 解 envelope
	TokenEnv  string // live / test, 嵌入 token 前缀
}

// Exchange PAN → internal_token. PCI 边界内只此函数能见 PAN 明文.
func (v *Vault) Exchange(ctx context.Context, in domain.ExchangeRequest) (domain.ExchangeResponse, error) {
	pan := strings.TrimSpace(in.PAN)
	if !crypto.ValidLuhn(pan) {
		return domain.ExchangeResponse{}, ErrInvalidPAN
	}
	panHash := hashPAN(pan)

	// dedup
	if existing, err := v.Store.FindByMerchantPANHash(in.MerchantID, panHash); err == nil {
		return responseFromToken(existing), nil
	}

	// 新 token
	internalToken := v.generateInternalToken()
	brand := domain.CardBrand(crypto.DetectBrand(pan))
	bin := pan[:6]
	last4 := pan[len(pan)-4:]

	t := domain.InternalToken{
		Token:       internalToken,
		MerchantID:  in.MerchantID,
		PANHash:     panHash,
		PANLast4:    last4,
		BIN:         bin,
		Brand:       brand,
		ExpMonth:    in.ExpMonth,
		ExpYear:     in.ExpYear,
		CardholderH: hashName(in.Cardholder),
		Status:      domain.TokenActive,
	}
	if err := v.Store.Create(t); err != nil {
		return domain.ExchangeResponse{}, fmt.Errorf("vault store create: %w", err)
	}

	// 加密 PAN
	enc, err := crypto.EncryptPAN(pan, v.DEK)
	if err != nil {
		return domain.ExchangeResponse{}, fmt.Errorf("vault encrypt pan: %w", err)
	}
	if err := v.Store.StoreEncryptedPAN(internalToken, enc); err != nil {
		return domain.ExchangeResponse{}, fmt.Errorf("vault store enc pan: %w", err)
	}

	// 异步 Provision — 错过的话 charge intent 时再补
	go func() {
		_, _ = v.Provision(context.Background(), internalToken)
	}()

	return responseFromToken(t), nil
}

// Provision 把 internal_token 上 network token (VTS/MDES).
// 多次调用幂等 — 已经 provisioned 就直接返已有 ref.
func (v *Vault) Provision(ctx context.Context, internalToken string) (domain.ProvisionResponse, error) {
	t, err := v.Store.Get(internalToken)
	if err != nil {
		return domain.ProvisionResponse{}, err
	}
	if t.NetworkRef != nil {
		return domain.ProvisionResponse{
			Token:      internalToken,
			Provider:   t.NetworkRef.Provider,
			NetworkRef: *t.NetworkRef,
		}, nil
	}
	prov := v.providerForBrand(t.Brand)
	if prov == nil {
		return domain.ProvisionResponse{}, ErrNoProvider
	}
	enc, err := v.Store.GetEncryptedPAN(internalToken)
	if err != nil {
		return domain.ProvisionResponse{}, fmt.Errorf("get enc pan: %w", err)
	}
	pan, err := crypto.DecryptPAN(enc, v.DEK)
	if err != nil {
		return domain.ProvisionResponse{}, fmt.Errorf("decrypt pan: %w", err)
	}
	out, err := prov.Provision(ctx, providers.ProvisionIn{
		PAN:        pan,
		ExpMonth:   t.ExpMonth,
		ExpYear:    t.ExpYear,
		MerchantID: t.MerchantID,
	})
	if err != nil {
		return domain.ProvisionResponse{}, fmt.Errorf("provider provision: %w", err)
	}
	ref := domain.NetworkRef{
		Provider:      prov.Name(),
		NetworkToken:  out.NetworkToken,
		TokenRefID:    out.TokenRefID,
		TokenExpiry:   out.TokenExpiry,
		ProvisionedAt: time.Now().UTC(),
	}
	if err := v.Store.UpdateNetworkRef(internalToken, ref); err != nil {
		return domain.ProvisionResponse{}, err
	}
	return domain.ProvisionResponse{
		Token:      internalToken,
		Provider:   prov.Name(),
		NetworkRef: ref,
	}, nil
}

// ChargeIntent 给 payment-core / card-payment 出单时调用.
// 返回 (network_token, cryptogram), PSP 上送卡组.
func (v *Vault) ChargeIntent(ctx context.Context, in domain.ChargeIntentRequest) (domain.ChargeIntentResponse, error) {
	t, err := v.Store.Get(in.Token)
	if err != nil {
		return domain.ChargeIntentResponse{}, err
	}
	if t.Status != domain.TokenActive {
		return domain.ChargeIntentResponse{}, ErrInactive
	}
	if t.NetworkRef == nil {
		// 兜底: 立即 provision
		if _, err := v.Provision(ctx, in.Token); err != nil {
			return domain.ChargeIntentResponse{}, fmt.Errorf("just-in-time provision: %w", err)
		}
		t, _ = v.Store.Get(in.Token)
		if t.NetworkRef == nil {
			return domain.ChargeIntentResponse{}, ErrNotProvisioned
		}
	}
	prov := v.Providers[t.NetworkRef.Provider]
	if prov == nil {
		return domain.ChargeIntentResponse{}, ErrNoProvider
	}
	mit := providers.MITNone
	if in.Recurring {
		mit = providers.MITRecurring
	}
	out, err := prov.Cryptogram(ctx, providers.CryptogramIn{
		TokenRefID: t.NetworkRef.TokenRefID,
		Amount:     in.Amount,
		Currency:   in.Currency,
		Recurring:  in.Recurring,
		MIT:        mit,
		IntentID:   in.IntentID,
	})
	if err != nil {
		return domain.ChargeIntentResponse{}, fmt.Errorf("cryptogram: %w", err)
	}
	return domain.ChargeIntentResponse{
		NetworkToken: t.NetworkRef.NetworkToken,
		Cryptogram:   out.Cryptogram,
		ECI:          out.ECI,
		TokenExpiry:  t.NetworkRef.TokenExpiry,
		Provider:     t.NetworkRef.Provider,
		IntentID:     in.IntentID,
	}, nil
}

// SuspendToken 商户冻结 / 风控冻结. 调用 provider 同步状态.
func (v *Vault) SuspendToken(ctx context.Context, token string, reason string) error {
	t, err := v.Store.Get(token)
	if err != nil {
		return err
	}
	if t.NetworkRef != nil {
		prov := v.Providers[t.NetworkRef.Provider]
		if prov != nil {
			_ = prov.Suspend(ctx, t.NetworkRef.TokenRefID)
		}
	}
	return v.Store.UpdateStatus(token, domain.TokenSuspended)
}

func (v *Vault) DeleteToken(ctx context.Context, token string) error {
	t, err := v.Store.Get(token)
	if err != nil {
		return err
	}
	if t.NetworkRef != nil {
		prov := v.Providers[t.NetworkRef.Provider]
		if prov != nil {
			_ = prov.Delete(ctx, t.NetworkRef.TokenRefID)
		}
	}
	_ = v.Store.DeleteEncryptedPAN(token)
	return v.Store.UpdateStatus(token, domain.TokenDeleted)
}

// ── helpers ──

func (v *Vault) providerForBrand(b domain.CardBrand) providers.Provider {
	// 默认路由
	switch b {
	case domain.BrandVisa:
		if p, ok := v.Providers[domain.ProviderVTS]; ok {
			return p
		}
	case domain.BrandMastercard:
		if p, ok := v.Providers[domain.ProviderMDES]; ok {
			return p
		}
	}
	// fallback to inhouse
	return v.Providers[domain.ProviderInhouse]
}

func (v *Vault) generateInternalToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	env := v.TokenEnv
	if env == "" {
		env = "live"
	}
	return "tk_" + env + "_" + hex.EncodeToString(b)
}

func hashPAN(pan string) string {
	h := sha256.Sum256([]byte("pan|" + pan))
	return hex.EncodeToString(h[:])
}

func hashName(name string) string {
	if name == "" {
		return ""
	}
	h := sha256.Sum256([]byte("name|" + strings.ToLower(strings.TrimSpace(name))))
	return hex.EncodeToString(h[:])[:16]
}

func responseFromToken(t domain.InternalToken) domain.ExchangeResponse {
	return domain.ExchangeResponse{
		Token:       t.Token,
		PANLast4:    t.PANLast4,
		BIN:         t.BIN,
		Brand:       t.Brand,
		ExpMonth:    t.ExpMonth,
		ExpYear:     t.ExpYear,
		Status:      t.Status,
		Provisioned: t.NetworkRef != nil,
	}
}
