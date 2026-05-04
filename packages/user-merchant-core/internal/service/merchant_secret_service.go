// Package service — merchant channel secret storage.
//
// Multi-tenant payment-channel credential isolation. Each merchant gets their
// own GCash partner id, Maya keys, webhook secrets etc.; those plaintexts are
// encrypted at Put time via kms-manage and stored keyed by (merchant, channel,
// field). Plaintext is only retrieved server-side by payment-channel at
// adapter init time via GetPlaintext — admin UIs should only ever see masked
// hints.
package service

import (
	"context"
	"fmt"

	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"

	"github.com/xiongwp/user-merchant-core/internal/cache"
	"github.com/xiongwp/user-merchant-core/internal/domain"
	"github.com/xiongwp/user-merchant-core/internal/repo"
)

// KMSClient is the minimal surface merchant-secret service needs from kms-
// manage. Decoupling keeps the repo import clean + eases unit testing with
// a fake. In production this is satisfied by a gRPC client; in tests with a
// simple passthrough stub.
type KMSClient interface {
	Encrypt(ctx context.Context, plaintext []byte, context string) (ciphertext []byte, err error)
	Decrypt(ctx context.Context, ciphertext []byte, context string) (plaintext []byte, err error)
}

// MerchantSecretService put/get/list merchant channel credentials.
type MerchantSecretService interface {
	// Put encrypts plaintext via KMS and upserts the row. Returns the row
	// (no plaintext).
	Put(ctx context.Context, merchantID, channel, fieldName, plaintext, actor string) (*domain.MerchantChannelSecret, error)

	// GetPlaintext returns the decrypted plaintext. Restricted endpoint —
	// wire separately from admin-visible List.
	GetPlaintext(ctx context.Context, merchantID, channel, fieldName string) (string, error)

	// ListForChannel returns every stored field for the (merchant, channel)
	// — masked only.
	ListForChannel(ctx context.Context, merchantID, channel string) ([]*domain.MerchantChannelSecret, error)

	// ListForMerchant returns every stored field across all channels — masked
	// only.
	ListForMerchant(ctx context.Context, merchantID string) ([]*domain.MerchantChannelSecret, error)

	// Delete removes a single (merchant, channel, field) row.
	Delete(ctx context.Context, merchantID, channel, fieldName string) error

	// BulkGetPlaintext returns a field→plaintext map for all fields stored
	// for (merchant, channel). Used by payment-channel adapter init.
	BulkGetPlaintext(ctx context.Context, merchantID, channel string) (map[string]string, error)
}

type merchantSecretService struct {
	repo   repo.MerchantSecretRepository
	kms    KMSClient
	cache  *cache.SecretCache
	sf     singleflight.Group
	logger *zap.Logger
}

// NewMerchantSecretService 构造。secretCache 为 nil 时禁用缓存（全部走 KMS）。
func NewMerchantSecretService(r repo.MerchantSecretRepository, kms KMSClient, c *cache.SecretCache, logger *zap.Logger) MerchantSecretService {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &merchantSecretService{repo: r, kms: kms, cache: c, logger: logger}
}

// ─── AAD context ────────────────────────────────────────────────────────────
//
// KMS AAD ties each ciphertext to its (merchant, channel, field) triple;
// swapping the row under the DB or across fields fails decrypt.

func secretContext(merchantID, channel, fieldName string) string {
	return fmt.Sprintf("merchant:%s:channel:%s:%s", merchantID, channel, fieldName)
}

// ─── Put / Get ───────────────────────────────────────────────────────────────

func (s *merchantSecretService) Put(ctx context.Context, merchantID, channel, fieldName, plaintext, actor string) (*domain.MerchantChannelSecret, error) {
	if merchantID == "" || channel == "" || fieldName == "" {
		return nil, fmt.Errorf("%w: merchant_id/channel/field_name required", domain.ErrValidation)
	}
	if plaintext == "" {
		return nil, fmt.Errorf("%w: plaintext empty", domain.ErrValidation)
	}
	if s.kms == nil {
		return nil, fmt.Errorf("merchant-secret: KMS client not configured")
	}
	aad := secretContext(merchantID, channel, fieldName)
	ciphertext, err := s.kms.Encrypt(ctx, []byte(plaintext), aad)
	if err != nil {
		return nil, fmt.Errorf("kms encrypt: %w", err)
	}
	row := &domain.MerchantChannelSecret{
		MerchantID: merchantID,
		Channel:    channel,
		FieldName:  fieldName,
		Ciphertext: ciphertext,
		Context:    aad,
		MaskedHint: domain.MaskSecret(plaintext),
		Version:    1, // upsert bumps it
		CreatedBy:  actor,
	}
	out, err := s.repo.Upsert(ctx, row)
	if err != nil {
		return nil, err
	}
	// 覆盖了某个 field 之后旧桶 stale → 失效，让下次 BulkGetPlaintext 回源。
	if s.cache != nil {
		s.cache.Invalidate(merchantID, channel)
	}
	s.logger.Info("merchant secret stored",
		zap.String("merchant_id", merchantID),
		zap.String("channel", channel),
		zap.String("field_name", fieldName),
		zap.String("actor", actor),
		zap.Int("version", out.Version))
	return out, nil
}

func (s *merchantSecretService) GetPlaintext(ctx context.Context, merchantID, channel, fieldName string) (string, error) {
	if s.kms == nil {
		return "", fmt.Errorf("merchant-secret: KMS client not configured")
	}
	row, err := s.repo.Get(ctx, merchantID, channel, fieldName)
	if err != nil {
		return "", err
	}
	plaintext, err := s.kms.Decrypt(ctx, row.Ciphertext, row.Context)
	if err != nil {
		return "", fmt.Errorf("kms decrypt: %w", err)
	}
	return string(plaintext), nil
}

func (s *merchantSecretService) ListForChannel(ctx context.Context, merchantID, channel string) ([]*domain.MerchantChannelSecret, error) {
	rows, err := s.repo.ListByMerchantChannel(ctx, merchantID, channel)
	if err != nil {
		return nil, err
	}
	// defensive: zero out ciphertext + context so response JSON never carries them
	for _, r := range rows {
		r.Ciphertext = nil
		r.Context = ""
	}
	return rows, nil
}

func (s *merchantSecretService) ListForMerchant(ctx context.Context, merchantID string) ([]*domain.MerchantChannelSecret, error) {
	rows, err := s.repo.ListByMerchant(ctx, merchantID)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		r.Ciphertext = nil
		r.Context = ""
	}
	return rows, nil
}

func (s *merchantSecretService) Delete(ctx context.Context, merchantID, channel, fieldName string) error {
	if err := s.repo.Delete(ctx, merchantID, channel, fieldName); err != nil {
		return err
	}
	if s.cache != nil {
		s.cache.Invalidate(merchantID, channel)
	}
	return nil
}

// BulkGetPlaintext — 热路径（payment-channel adapter-init）。
// cache-aside + singleflight：并发 miss 时只跑一次 KMS 解密，后续共享结果。
// 失效由 Put / Delete 主动触发；TTL（默认 5m）兜底以应对外部绕过服务的变更。
func (s *merchantSecretService) BulkGetPlaintext(ctx context.Context, merchantID, channel string) (map[string]string, error) {
	if s.kms == nil {
		return nil, fmt.Errorf("merchant-secret: KMS client not configured")
	}
	if s.cache != nil {
		if cached, ok := s.cache.Get(merchantID, channel); ok {
			return cached, nil
		}
	}
	v, err, _ := s.sf.Do("bulk:"+merchantID+"|"+channel, func() (any, error) {
		return s.loadAndDecrypt(ctx, merchantID, channel)
	})
	if err != nil {
		return nil, err
	}
	m := v.(map[string]string)
	// 写缓存时复制一份，防被调用方改完污染缓存（SecretCache.Get 也会复制一份给返回）。
	if s.cache != nil && len(m) > 0 {
		store := make(map[string]string, len(m))
		for k, val := range m {
			store[k] = val
		}
		s.cache.Put(merchantID, channel, store)
	}
	return m, nil
}

func (s *merchantSecretService) loadAndDecrypt(ctx context.Context, merchantID, channel string) (map[string]string, error) {
	rows, err := s.repo.ListByMerchantChannel(ctx, merchantID, channel)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		pt, err := s.kms.Decrypt(ctx, r.Ciphertext, r.Context)
		if err != nil {
			s.logger.Warn("decrypt skipped",
				zap.String("merchant_id", merchantID),
				zap.String("channel", channel),
				zap.String("field_name", r.FieldName),
				zap.Error(err))
			continue
		}
		out[r.FieldName] = string(pt)
	}
	return out, nil
}
