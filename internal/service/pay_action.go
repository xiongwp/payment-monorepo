package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/order-core/internal/domain"
	"github.com/xiongwp/order-core/internal/idgen"
	"github.com/xiongwp/order-core/internal/repo"
	"github.com/xiongwp/order-core/internal/sharding"
)

// ─── ActionProvider：下游验证接口 ────────────────────────────────────────────

// ActionChallenge 创建挑战时下游返回的材料。
//
//	Payload        给前端展示 / 跳转的公开字段（redirect_url / qr_image_base64 /
//	               recipient_masked 等）
//	ExpectedSecret 服务端期望值（OTP 明文 / 密码哈希），仅存库、不返回给前端
//	ExpiresAt      挑战过期时间；为 nil 表示由服务层用默认 TTL
type ActionChallenge struct {
	Payload        map[string]string
	ExpectedSecret string
	ExpiresAt      *time.Time
	MaxAttempts    int
}

// ActionProvider 按 action_type 分发的下游对接接口。
//
// 每种 PayActionType 都要注册一个 Provider 实现：
//
//	three_d_secure  → 对接 acquirer 3DS v2（EMVCo），渠道返回 ACS redirect_url
//	otp             → 对接短信 / 邮件 / 语音供应商，渠道回挑战码 + 服务端保留期望值
//	pay_password    → 读取用户支付密码哈希，服务端比对
//
// Initiate：创建挑战（服务层落库后返回给调用方的 next_action）
// Verify：用户回传结果 → 下游校验 / 本地哈希比对
type ActionProvider interface {
	// Initiate 向下游发起挑战，返回需展示给前端的 payload + 服务端期望值
	Initiate(ctx context.Context, in *InitiateActionInput) (*ActionChallenge, error)
	// Verify 校验用户输入。ok=false 时返回具体 reason；err 表示下游/系统错误（与业务失败区分）
	Verify(ctx context.Context, action *domain.PayAction, input map[string]string) (ok bool, reason string, err error)
}

// InitiateActionInput 下游挑战的入参
type InitiateActionInput struct {
	PaymentIntent *domain.PaymentIntent
	Charge        *domain.Charge // 可空：未绑定 Charge 时（仅做独立风控校验）
	ActionType    domain.PayActionType
	Extra         map[string]string // 调用方传给下游的额外字段
}

// ─── Provider 注册表 ────────────────────────────────────────────────────────

// ProviderRegistry 按 action_type 存放实现
type ProviderRegistry struct {
	mu sync.RWMutex
	m  map[domain.PayActionType]ActionProvider
}

// NewProviderRegistry 新建空注册表
func NewProviderRegistry() *ProviderRegistry {
	return &ProviderRegistry{m: map[domain.PayActionType]ActionProvider{}}
}

// Register 注册 / 覆盖
func (r *ProviderRegistry) Register(t domain.PayActionType, p ActionProvider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[t] = p
}

// Get 取实现，未注册返回 nil
func (r *ProviderRegistry) Get(t domain.PayActionType) ActionProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.m[t]
}

// ─── PayActionService ────────────────────────────────────────────────────────

// PayActionService 管理 PayAction 生命周期
type PayActionService interface {
	// Create 在 Confirm PI 流程中创建挑战：调用 Provider.Initiate → 落库 PayAction →
	// 返回给调用方（由 PI 服务负责把 PI 推到 REQUIRES_ACTION）。
	Create(ctx context.Context, pi *domain.PaymentIntent, charge *domain.Charge, t domain.PayActionType, extra map[string]string) (*domain.PayAction, error)
	// Submit 用户提交结果：成功 → PayAction=succeeded + PI 推进 SUCCEEDED；
	// 失败 → 尝试次数 +1；耗尽次数 → PayAction=failed + PI 推进 FAILED（终态，商户换单）
	Submit(ctx context.Context, piID, actionID string, input map[string]string) (*domain.PayAction, *domain.PaymentIntent, error)
	// Get / List / PendingByPI
	Get(ctx context.Context, piID, actionID string) (*domain.PayAction, error)
	ListByPI(ctx context.Context, piID string) ([]*domain.PayAction, error)
	PendingByPI(ctx context.Context, piID string) (*domain.PayAction, error)
	// ExpirePending 扫描 expires_at < now 的 pending action 置为 expired，对应 PI 置 failed
	ExpirePending(ctx context.Context) (int, error)
}

type payActionService struct {
	piSvc      PaymentIntentService
	piRepo     repo.PaymentIntentRepository
	actionRepo repo.PayActionRepository
	chargeRepo repo.ChargeRepository
	registry   *ProviderRegistry
	idgen      idgen.IDGenerator
	router     *sharding.Router
	logger     *zap.Logger

	defaultTTL time.Duration
}

// NewPayActionService 构造
func NewPayActionService(
	pi PaymentIntentService,
	piRepo repo.PaymentIntentRepository,
	ar repo.PayActionRepository,
	cr repo.ChargeRepository,
	registry *ProviderRegistry,
	g idgen.IDGenerator,
	r *sharding.Router,
	defaultTTL time.Duration,
	logger *zap.Logger,
) PayActionService {
	if logger == nil {
		logger = zap.NewNop()
	}
	if defaultTTL <= 0 {
		defaultTTL = 5 * time.Minute
	}
	return &payActionService{
		piSvc: pi, piRepo: piRepo, actionRepo: ar, chargeRepo: cr,
		registry: registry, idgen: g, router: r, defaultTTL: defaultTTL, logger: logger,
	}
}

// Create 创建一条 pending 挑战
func (s *payActionService) Create(ctx context.Context, pi *domain.PaymentIntent, charge *domain.Charge, t domain.PayActionType, extra map[string]string) (*domain.PayAction, error) {
	if pi == nil {
		return nil, fmt.Errorf("%w: payment_intent required", domain.ErrValidation)
	}
	prov := s.registry.Get(t)
	if prov == nil {
		return nil, fmt.Errorf("%w: no provider registered for action_type %s", domain.ErrValidation, t)
	}
	ch, err := prov.Initiate(ctx, &InitiateActionInput{
		PaymentIntent: pi,
		Charge:        charge,
		ActionType:    t,
		Extra:         extra,
	})
	if err != nil {
		return nil, fmt.Errorf("provider initiate %s: %w", t, err)
	}

	dbIdx, tblIdx := s.router.RouteByPrefixedID(pi.ID)
	seq, err := s.idgen.NextID(ctx, idgen.BizTagPaymentIntent) // 复用 biz_tag；如要独立可另加
	if err != nil {
		return nil, err
	}
	id := s.router.FormatID("act", dbIdx, tblIdx, seq)

	now := time.Now().UTC()
	expires := ch.ExpiresAt
	if expires == nil {
		t := now.Add(s.defaultTTL)
		expires = &t
	}
	maxAttempts := ch.MaxAttempts
	if maxAttempts <= 0 {
		switch t {
		case domain.PayActionOTP, domain.PayActionPayPassword:
			maxAttempts = 3
		default:
			maxAttempts = 1
		}
	}
	var chargeID string
	if charge != nil {
		chargeID = charge.ID
	}
	act := &domain.PayAction{
		ID:              id,
		PaymentIntentID: pi.ID,
		ChargeID:        chargeID,
		ActionType:      t,
		Status:          domain.PayActionStatusPending,
		Payload:         domain.Metadata(ch.Payload),
		ExpectedSecret:  ch.ExpectedSecret,
		MaxAttempts:     maxAttempts,
		ExpiresAt:       expires,
		Created:         now,
		Updated:         now,
	}
	// 并行：actionRepo.Create 和 piSvc.RequireAction 各自一次 DB 往返，
	// 落同一物理分片但是独立事务。串行 ~30-50ms（两次 RTT），并行后取单次。
	// 失败语义保持和原代码一致：
	//   - actionRepo.Create 失败 → 整笔失败（PayAction 没落表，调用方需重试）
	//   - RequireAction 失败 → 仅 warn（PI 状态没推到 REQUIRES_ACTION 不阻塞，
	//     ExpireWorker / 后续轮询会兜底，业界经验是这种 transition 错误极少
	//     发生且重试自愈）
	needTransition := pi.Status != domain.PIStatusRequiresAction
	var (
		wg            sync.WaitGroup
		createErr     error
		transitionErr error
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		createErr = s.actionRepo.Create(ctx, act)
	}()
	if needTransition {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, transitionErr = s.piSvc.RequireAction(ctx, pi.ID)
		}()
	}
	wg.Wait()
	if createErr != nil {
		return nil, createErr
	}
	if needTransition && transitionErr != nil {
		s.logger.Warn("transition to requires_action failed", zap.Error(transitionErr), zap.String("pi_id", pi.ID))
	}
	return act, nil
}

// Submit 用户提交
func (s *payActionService) Submit(ctx context.Context, piID, actionID string, input map[string]string) (*domain.PayAction, *domain.PaymentIntent, error) {
	act, err := s.actionRepo.Get(ctx, piID, actionID)
	if err != nil {
		return nil, nil, err
	}
	if act.Status != domain.PayActionStatusPending {
		return nil, nil, domain.ErrPayActionNotPending
	}
	if act.ExpiresAt != nil && time.Now().UTC().After(*act.ExpiresAt) {
		return s.expireAction(ctx, piID, act, "expired before submit")
	}

	prov := s.registry.Get(act.ActionType)
	if prov == nil {
		return nil, nil, fmt.Errorf("%w: no provider for %s", domain.ErrValidation, act.ActionType)
	}
	ok, reason, err := prov.Verify(ctx, act, input)
	if err != nil {
		return nil, nil, fmt.Errorf("provider verify: %w", err)
	}
	_ = s.actionRepo.IncrementAttempt(ctx, piID, actionID)

	if ok {
		now := time.Now().UTC()
		updated, _ := s.actionRepo.UpdateFields(ctx, piID, actionID, map[string]any{
			"status":       domain.PayActionStatusSucceeded,
			"completed_at": now,
		})
		// PI: REQUIRES_ACTION → SUCCEEDED（完成所有挑战）
		pi, err := s.piSvc.MarkSucceeded(ctx, piID, act.ChargeID, 0)
		if err != nil {
			return updated, nil, err
		}
		return updated, pi, nil
	}

	// 校验失败：次数耗尽 → action failed + PI → FAILED；否则保持 pending 让用户重试
	reloaded, _ := s.actionRepo.Get(ctx, piID, actionID)
	if reloaded.MaxAttempts > 0 && reloaded.AttemptCount >= reloaded.MaxAttempts {
		now := time.Now().UTC()
		failed, _ := s.actionRepo.UpdateFields(ctx, piID, actionID, map[string]any{
			"status":         domain.PayActionStatusFailed,
			"failure_reason": "max attempts reached: " + reason,
			"completed_at":   now,
		})
		// 先把 PI 从 REQUIRES_ACTION 推回 PROCESSING，再 → FAILED（状态机约束）
		_, _ = s.piRepo.UpdateStatus(ctx, piID, domain.PIStatusRequiresAction, domain.PIStatusProcessing, nil)
		pi, err := s.piSvc.MarkFailed(ctx, piID, act.ChargeID, "action_failed", reason)
		return failed, pi, err
	}
	return reloaded, nil, fmt.Errorf("%w: %s", domain.ErrValidation, reason)
}

// Get / List / PendingByPI
func (s *payActionService) Get(ctx context.Context, piID, actionID string) (*domain.PayAction, error) {
	return s.actionRepo.Get(ctx, piID, actionID)
}
func (s *payActionService) ListByPI(ctx context.Context, piID string) ([]*domain.PayAction, error) {
	return s.actionRepo.ListByPI(ctx, piID)
}
func (s *payActionService) PendingByPI(ctx context.Context, piID string) (*domain.PayAction, error) {
	return s.actionRepo.PendingByPI(ctx, piID)
}

// ExpirePending 暂不提供全分片扫描；cron 若需要可另实现：此处占位返回 0。
func (s *payActionService) ExpirePending(ctx context.Context) (int, error) { return 0, nil }

// expireAction 标记单条 action 为 expired 并推进 PI → FAILED（失败终态）
func (s *payActionService) expireAction(ctx context.Context, piID string, act *domain.PayAction, reason string) (*domain.PayAction, *domain.PaymentIntent, error) {
	now := time.Now().UTC()
	expired, _ := s.actionRepo.UpdateFields(ctx, piID, act.ID, map[string]any{
		"status":         domain.PayActionStatusExpired,
		"failure_reason": reason,
		"completed_at":   now,
	})
	_, _ = s.piRepo.UpdateStatus(ctx, piID, domain.PIStatusRequiresAction, domain.PIStatusProcessing, nil)
	pi, err := s.piSvc.MarkFailed(ctx, piID, act.ChargeID, "action_expired", reason)
	return expired, pi, err
}
