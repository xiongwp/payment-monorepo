// Package mdes — Mastercard Digital Enablement Service adapter.
//
// 生产:
//   - mTLS + Signed JWS request body (RFC 7515) to api.mastercard.com/mdes
//   - Tokenize API:    POST /mdes/digitization/static/1/0/tokenize
//   - Cryptogram API:  POST /mdes/digitization/static/1/0/transact
//
// 同 vts 模式 — 这里实现 stub, 真 prod 接 HTTPS.

package mdes

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"reconcile-system/packages/tokenization-vault/internal/domain"
	"reconcile-system/packages/tokenization-vault/internal/providers"
)

type Provider struct {
	signingKey []byte
	atc        atomic.Int64
}

func New(signingKey []byte) *Provider {
	// P0-VAULT-1: prod 必须显式给 signingKey, 用 dev stub key panic.
	// 完整 prod 实现: mTLS client + Mastercard MDES endpoint + apiKey 注入,
	// signingKey 仅 dev/staging 走本地 HMAC.
	if len(signingKey) == 0 {
		env := strings.ToLower(strings.TrimSpace(os.Getenv("APP_ENV")))
		if env == "prod" || env == "production" {
			panic("tokenization-vault MDES: signingKey 不能为空 (APP_ENV=" + env + "). " +
				"prod 必须真接 Mastercard MDES endpoint + 注入真签名密钥; dev stub HMAC 会让 cryptogram 被卡组织拒.")
		}
		signingKey = []byte("mdes-dev-stub-key-CHANGE-IN-PROD")
	}
	return &Provider{signingKey: signingKey}
}

func (p *Provider) Name() domain.TokenProvider { return domain.ProviderMDES }

func (p *Provider) Provision(_ context.Context, in providers.ProvisionIn) (providers.ProvisionOut, error) {
	if len(in.PAN) < 13 || len(in.PAN) > 19 {
		return providers.ProvisionOut{}, fmt.Errorf("mdes: bad pan length %d", len(in.PAN))
	}
	h := sha256.Sum256([]byte("mdes|" + in.PAN))
	dpan := "DPAN-MC-" + hex.EncodeToString(h[:])[:14]
	ref := "mref_" + hex.EncodeToString(h[14:22])
	par := "M0000" + hex.EncodeToString(h[22:30])

	exp := time.Now().AddDate(2, 0, 0)
	expiry := fmt.Sprintf("%02d%02d", exp.Month(), exp.Year()%100)
	return providers.ProvisionOut{
		NetworkToken: dpan,
		TokenRefID:   ref,
		TokenExpiry:  expiry,
		PAR:          par,
	}, nil
}

func (p *Provider) Cryptogram(_ context.Context, in providers.CryptogramIn) (providers.CryptogramOut, error) {
	atc := int(p.atc.Add(1))
	msg := fmt.Sprintf("MDES|%s|%d|%s|%s|%d|%t", in.TokenRefID, in.Amount, in.Currency, in.IntentID, atc, in.Recurring)
	mac := hmac.New(sha256.New, p.signingKey)
	mac.Write([]byte(msg))
	ucaf := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	// MC ECI: 02 = SCA, 01 = attempted
	eci := "02"
	if in.Recurring {
		eci = "07"
	}
	return providers.CryptogramOut{
		Cryptogram: ucaf,
		ECI:        eci,
		ATC:        atc,
	}, nil
}

func (p *Provider) Suspend(_ context.Context, _ string) error { return nil }
func (p *Provider) Resume(_ context.Context, _ string) error  { return nil }
func (p *Provider) Delete(_ context.Context, _ string) error  { return nil }
