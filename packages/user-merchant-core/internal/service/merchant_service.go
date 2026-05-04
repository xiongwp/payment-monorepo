// Package service — merchant onboarding + KYC orchestration.
package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"strings"

	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"

	"github.com/xiongwp/payment-util/shadow"
	"github.com/xiongwp/user-merchant-core/internal/cache"
	"github.com/xiongwp/user-merchant-core/internal/domain"
	"github.com/xiongwp/user-merchant-core/internal/idgen"
	"github.com/xiongwp/user-merchant-core/internal/repo"
	"github.com/xiongwp/user-merchant-core/pkg/pii"
)

// MerchantService 商户生命周期服务。
//
// 职责：
//   - Create: 注册新商户（幂等/去重 email），生成 API keys 的一次性明文 + 落 hash
//   - SubmitKYC: 商户上传材料完成后推进到 submitted
//   - StartReview / Approve / Reject / RequestMoreInfo: 审核方状态推进
//   - Suspend / Unsuspend / Terminate: approved 态的后续调整
//   - Docs: 上传、审核 KYC 文档
type MerchantService interface {
	Create(ctx context.Context, in *CreateMerchantInput) (*CreateMerchantOutput, error)
	Get(ctx context.Context, id string) (*domain.Merchant, error)
	BatchGet(ctx context.Context, ids []string) (map[string]*domain.Merchant, error)
	List(ctx context.Context, status domain.MerchantStatus, kyc domain.KYCStatus, limit, offset int) ([]*domain.Merchant, int64, error)
	Update(ctx context.Context, id string, fields map[string]any) (*domain.Merchant, error)
	RotateAPIKeys(ctx context.Context, id string, liveOrTest string) (plaintext string, _ error)

	// KYC state machine (高层语义封装 TransitionKYC)
	SubmitKYC(ctx context.Context, id, actor string) (*domain.Merchant, error)
	StartReview(ctx context.Context, id, actor string) (*domain.Merchant, error)
	Approve(ctx context.Context, id, actor string) (*domain.Merchant, error)
	Reject(ctx context.Context, id, actor, reason string) (*domain.Merchant, error)
	RequestMoreInfo(ctx context.Context, id, actor, reason string) (*domain.Merchant, error)
	Suspend(ctx context.Context, id, actor, reason string) (*domain.Merchant, error)
	Unsuspend(ctx context.Context, id, actor string) (*domain.Merchant, error)
	Terminate(ctx context.Context, id, actor, reason string) (*domain.Merchant, error)

	// Docs
	AddDocument(ctx context.Context, d *domain.MerchantKYCDocument) (*domain.MerchantKYCDocument, error)
	ListDocuments(ctx context.Context, merchantID string) ([]*domain.MerchantKYCDocument, error)
	ReviewDocument(ctx context.Context, id, status, note string) error

	// Audit
	ListKYCAudits(ctx context.Context, merchantID string, limit int) ([]*domain.MerchantKYCAudit, error)

	// Authenticate by API key plaintext (returns matched merchant; constant-time comparison on hash)
	AuthenticateByAPIKey(ctx context.Context, plaintext string) (*domain.Merchant, error)

	// WarmupCache 启动期预热，从 DB 拉活跃商户塞进 cache。limit<=0 走默认 1000。
	WarmupCache(ctx context.Context, limit int) error
}

// CreateMerchantInput 注册新商户入参
type CreateMerchantInput struct {
	Name           string
	LegalName      string
	Country        string
	BusinessType   string
	TaxID          string
	ContactEmail   string
	ContactPhone   string
	Website        string
	MCC            string
	SettleCurrency string
	SettleMethod   string
	SettleAccount  string
	SettleBank     string
	SettleHolder   string
	WebhookURL     string
	Metadata       domain.Metadata
}

// CreateMerchantOutput 注册返回（API keys 只在注册时返回一次明文）
type CreateMerchantOutput struct {
	Merchant      *domain.Merchant
	LiveSecretKey string // plaintext, shown once
	TestSecretKey string // plaintext, shown once
	WebhookSecret string // plaintext, shown once
}

type merchantService struct {
	repo   repo.MerchantRepository
	idg    idgen.IDGenerator
	cache  *cache.MerchantCache
	sf     singleflight.Group // 防缓存击穿：同 id 并发只回源一次
	logger *zap.Logger
}

// NewMerchantService 构造。cache 传 nil 时禁用缓存（单元测试 / ops reload 用）。
func NewMerchantService(r repo.MerchantRepository, g idgen.IDGenerator, c *cache.MerchantCache, logger *zap.Logger) MerchantService {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &merchantService{repo: r, idg: g, cache: c, logger: logger}
}

// WarmupCache 启动期预热：从 DB 拉最多 limit 条 active 商户塞进 cache，
// 避开冷启动时的 thundering-herd。limit <=0 走默认 1000。
func (s *merchantService) WarmupCache(ctx context.Context, limit int) error {
	if s.cache == nil {
		return nil
	}
	rows, err := s.repo.ListActive(ctx, limit)
	if err != nil {
		return err
	}
	for _, m := range rows {
		s.cache.Put(m)
	}
	s.logger.Info("merchant cache warmed up", zap.Int("count", len(rows)))
	return nil
}

// ─── create / crud ───────────────────────────────────────────────────────────

func (s *merchantService) Create(ctx context.Context, in *CreateMerchantInput) (*CreateMerchantOutput, error) {
	if in == nil {
		return nil, fmt.Errorf("%w: input nil", domain.ErrValidation)
	}
	if in.Name == "" || in.ContactEmail == "" {
		return nil, fmt.Errorf("%w: name and contact_email required", domain.ErrValidation)
	}
	seq, err := s.idg.NextID(ctx, idgen.BizTagMerchant)
	if err != nil {
		return nil, fmt.Errorf("idgen: %w", err)
	}
	id, err := shadow.EncodeIDStr(ctx, shadow.IDTypeMerchantID, 0, seq)
	if err != nil {
		return nil, fmt.Errorf("encode merchant id: %w", err)
	}

	liveKey := newSecretKey("sk_live_")
	testKey := newSecretKey("sk_test_")
	whSecret := randomHex(32)

	m := &domain.Merchant{
		ID:             id,
		Name:           in.Name,
		LegalName:      in.LegalName,
		Country:        nonEmpty(in.Country, "PH"),
		BusinessType:   nonEmpty(in.BusinessType, "individual"),
		TaxID:          in.TaxID,
		ContactEmail:   in.ContactEmail,
		ContactPhone:   in.ContactPhone,
		Website:        in.Website,
		MCC:            in.MCC,
		LiveKeyHash:    hashKey(liveKey),
		TestKeyHash:    hashKey(testKey),
		WebhookURL:     in.WebhookURL,
		WebhookSecret:  whSecret,
		KYCStatus:      domain.KYCStatusPending,
		Status:         domain.MerchantStatusPending,
		RiskTier:       "standard",
		RateLimitRPS:   100,
		SettleCurrency: nonEmpty(in.SettleCurrency, "PHP"),
		SettleMethod:   in.SettleMethod,
		SettleAccount:  in.SettleAccount,
		SettleBank:     in.SettleBank,
		SettleHolder:   in.SettleHolder,
		Metadata:       in.Metadata,
	}
	if err := s.repo.Create(ctx, m); err != nil {
		return nil, err
	}
	s.cachePut(m) // 新商户立刻入缓存，后续 GetByID/Auth 都走内存
	s.logger.Info("merchant created",
		zap.String("id", id),
		zap.String("email_masked", pii.MaskEmail(in.ContactEmail)))
	return &CreateMerchantOutput{
		Merchant: m, LiveSecretKey: liveKey, TestSecretKey: testKey, WebhookSecret: whSecret,
	}, nil
}

func (s *merchantService) Get(ctx context.Context, id string) (*domain.Merchant, error) {
	if s.cache != nil {
		if m, ok := s.cache.GetByID(id); ok {
			return m, nil
		}
	}
	// singleflight 合并同 id 的并发回源（比如热点商户 100 QPS 同时 miss 只打一次 DB）
	v, err, _ := s.sf.Do("get:"+id, func() (any, error) {
		m, err := s.repo.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		s.cachePut(m)
		return m, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*domain.Merchant), nil
}

// BatchGet 先查缓存逐个命中，miss 的一批再一次性走 DB。
// 缓存粒度是单条 merchant，所以批量语义天然可以"部分 hit / 部分 miss"。
func (s *merchantService) BatchGet(ctx context.Context, ids []string) (map[string]*domain.Merchant, error) {
	if len(ids) == 0 {
		return map[string]*domain.Merchant{}, nil
	}
	out := make(map[string]*domain.Merchant, len(ids))
	miss := make([]string, 0, len(ids))
	if s.cache != nil {
		for _, id := range ids {
			if m, ok := s.cache.GetByID(id); ok {
				out[id] = m
				continue
			}
			miss = append(miss, id)
		}
	} else {
		miss = ids
	}
	if len(miss) > 0 {
		rows, err := s.repo.BatchGet(ctx, miss)
		if err != nil {
			return nil, err
		}
		for _, m := range rows {
			out[m.ID] = m
			s.cachePut(m)
		}
	}
	return out, nil
}

func (s *merchantService) List(ctx context.Context, status domain.MerchantStatus, kyc domain.KYCStatus, limit, offset int) ([]*domain.Merchant, int64, error) {
	return s.repo.List(ctx, status, kyc, limit, offset)
}

func (s *merchantService) Update(ctx context.Context, id string, fields map[string]any) (*domain.Merchant, error) {
	// Protect sensitive fields from direct PATCH
	for _, k := range []string{"live_key_hash", "test_key_hash", "webhook_secret", "kyc_status", "status"} {
		delete(fields, k)
	}
	m, err := s.repo.UpdateFields(ctx, id, fields)
	if err != nil {
		return nil, err
	}
	s.cachePut(m) // 覆写，不是 Invalidate；热点商户无须让下一次请求回源
	return m, nil
}

func (s *merchantService) RotateAPIKeys(ctx context.Context, id, liveOrTest string) (string, error) {
	plain := newSecretKey("sk_" + liveOrTest + "_")
	field := liveOrTest + "_key_hash"
	if field != "live_key_hash" && field != "test_key_hash" {
		return "", fmt.Errorf("%w: liveOrTest must be 'live' or 'test'", domain.ErrValidation)
	}
	if _, err := s.repo.UpdateFields(ctx, id, map[string]any{field: hashKey(plain)}); err != nil {
		return "", err
	}
	// 老 key 的 hash→id 反查下一次 LookupByKeyHash 仍会命中 hashToID 但 byID miss
	// → 走 DB 再由 Put 覆盖。不需要逐个删 hashToID。
	s.cacheInvalidate(id)
	s.logger.Info("api key rotated",
		zap.String("merchant_id", id),
		zap.String("kind", liveOrTest))
	return plain, nil
}

// ─── KYC transitions ─────────────────────────────────────────────────────────

func (s *merchantService) transition(ctx context.Context, id, actor, reason string, to domain.KYCStatus, validFrom ...domain.KYCStatus) (*domain.Merchant, error) {
	cur, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	// Check the current state is one of the accepted sources (caller convenience).
	if len(validFrom) > 0 {
		ok := false
		for _, v := range validFrom {
			if cur.KYCStatus == v {
				ok = true
				break
			}
		}
		if !ok {
			return nil, fmt.Errorf("%w: current=%s, expected one of %v", domain.ErrMerchantKYCInvalidTransition, cur.KYCStatus, validFrom)
		}
	}
	m, err := s.repo.TransitionKYC(ctx, id, cur.KYCStatus, to, reason, actor)
	if err != nil {
		return nil, err
	}
	s.cachePut(m) // status 变化的下游（比如"suspended 后拒绝新收单"）必须立即可见
	return m, nil
}

func (s *merchantService) SubmitKYC(ctx context.Context, id, actor string) (*domain.Merchant, error) {
	return s.transition(ctx, id, actor, "", domain.KYCStatusSubmitted,
		domain.KYCStatusPending, domain.KYCStatusNeedsMoreInfo)
}

func (s *merchantService) StartReview(ctx context.Context, id, actor string) (*domain.Merchant, error) {
	return s.transition(ctx, id, actor, "", domain.KYCStatusReviewing, domain.KYCStatusSubmitted)
}

func (s *merchantService) Approve(ctx context.Context, id, actor string) (*domain.Merchant, error) {
	return s.transition(ctx, id, actor, "", domain.KYCStatusApproved,
		domain.KYCStatusReviewing, domain.KYCStatusSuspended)
}

func (s *merchantService) Reject(ctx context.Context, id, actor, reason string) (*domain.Merchant, error) {
	return s.transition(ctx, id, actor, reason, domain.KYCStatusRejected,
		domain.KYCStatusPending, domain.KYCStatusSubmitted, domain.KYCStatusReviewing, domain.KYCStatusNeedsMoreInfo)
}

func (s *merchantService) RequestMoreInfo(ctx context.Context, id, actor, reason string) (*domain.Merchant, error) {
	return s.transition(ctx, id, actor, reason, domain.KYCStatusNeedsMoreInfo,
		domain.KYCStatusSubmitted, domain.KYCStatusReviewing)
}

func (s *merchantService) Suspend(ctx context.Context, id, actor, reason string) (*domain.Merchant, error) {
	return s.transition(ctx, id, actor, reason, domain.KYCStatusSuspended, domain.KYCStatusApproved)
}

func (s *merchantService) Unsuspend(ctx context.Context, id, actor string) (*domain.Merchant, error) {
	return s.transition(ctx, id, actor, "", domain.KYCStatusApproved, domain.KYCStatusSuspended)
}

func (s *merchantService) Terminate(ctx context.Context, id, actor, reason string) (*domain.Merchant, error) {
	return s.transition(ctx, id, actor, reason, domain.KYCStatusTerminated,
		domain.KYCStatusApproved, domain.KYCStatusSuspended)
}

// ─── documents ───────────────────────────────────────────────────────────────

func (s *merchantService) AddDocument(ctx context.Context, d *domain.MerchantKYCDocument) (*domain.MerchantKYCDocument, error) {
	if d.ID == "" {
		seq, err := s.idg.NextID(ctx, idgen.BizTagKYCDocument)
		if err != nil {
			return nil, err
		}
		d.ID, err = shadow.EncodeIDStr(ctx, shadow.IDTypeMerchantKYCDoc, 0, seq)
		if err != nil {
			return nil, fmt.Errorf("encode kyc doc id: %w", err)
		}
	}
	if err := s.repo.AddDocument(ctx, d); err != nil {
		return nil, err
	}
	return d, nil
}

func (s *merchantService) ListDocuments(ctx context.Context, merchantID string) ([]*domain.MerchantKYCDocument, error) {
	return s.repo.ListDocuments(ctx, merchantID)
}

func (s *merchantService) ReviewDocument(ctx context.Context, id, status, note string) error {
	if status != "accepted" && status != "rejected" && status != "expired" {
		return fmt.Errorf("%w: status must be accepted/rejected/expired", domain.ErrValidation)
	}
	return s.repo.ReviewDocument(ctx, id, status, note)
}

// ─── audit ───────────────────────────────────────────────────────────────────

func (s *merchantService) ListKYCAudits(ctx context.Context, merchantID string, limit int) ([]*domain.MerchantKYCAudit, error) {
	return s.repo.ListKYCAudits(ctx, merchantID, limit)
}

// ─── API key auth ────────────────────────────────────────────────────────────

// AuthenticateByAPIKey 命中率是全服务最高的热路径（每笔 PI/Charge 一次）。
// cache-aside + singleflight，rotate/suspend 靠 mutation 主动失效；TTL 兜底。
func (s *merchantService) AuthenticateByAPIKey(ctx context.Context, plaintext string) (*domain.Merchant, error) {
	plaintext = strings.TrimSpace(plaintext)
	if plaintext == "" {
		return nil, fmt.Errorf("%w: empty api key", domain.ErrValidation)
	}
	h := hashKey(plaintext)
	if s.cache != nil {
		if m, ok := s.cache.LookupByKeyHash(h); ok {
			return m, nil
		}
	}
	v, err, _ := s.sf.Do("auth:"+h, func() (any, error) {
		m, err := s.repo.GetByKeyHash(ctx, h)
		if err != nil {
			return nil, err
		}
		s.cachePut(m)
		return m, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*domain.Merchant), nil
}

// ─── cache helpers ───────────────────────────────────────────────────────────

func (s *merchantService) cachePut(m *domain.Merchant) {
	if s.cache != nil && m != nil {
		s.cache.Put(m)
	}
}

func (s *merchantService) cacheInvalidate(id string) {
	if s.cache != nil {
		s.cache.Invalidate(id)
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// newSecretKey returns a prefixed, base32-encoded 160-bit secret. Prefix lets us
// eyeball live/test in logs + block reuse (live key rejected on test endpoint).
func newSecretKey(prefix string) string {
	var b [20]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand must not fail
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])
	return prefix + strings.ToLower(enc)
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func hashKey(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

func nonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
