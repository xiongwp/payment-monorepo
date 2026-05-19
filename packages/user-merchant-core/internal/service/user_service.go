// Package service — User onboarding + auth + session + RBAC orchestration.
//
// 流程：
//
//	Register: validate → risk.Screen("register") → bcrypt → CreateUser →
//	          UserAuth(password / email / phone) → CreateWallet → AssignRole("user") →
//	          risk.Report("register") → 异步发 verify_email → 签 JWT + CreateSession
//
//	Login:    GetUserByUsername / Email / Phone → CheckPassword →
//	          失败 IncrFailedLogin（>5 LockUser）+ InsertLoginLog（status=0）；
//	          成功 ResetFailedLogin → risk.Screen("login") → REVIEW or totp_enabled →
//	          challenge 流程；否则签 JWT + CreateSession + InsertLoginLog(status=1) +
//	          risk.Report("login")
//
//	IntrospectToken: 验 JWT 签名 + 在 user_sessions 表里查 token 是否还在
//	          （登出 / 风控冻结时被删 → 即时撤销）。SSO 入口。
//
//	HasPermission: code 对应权限。RBAC = users → user_roles → role_permissions →
//	          permissions。
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/user-merchant-core/internal/authpkg"
	umcache "github.com/xiongwp/user-merchant-core/internal/cache"
	"github.com/xiongwp/user-merchant-core/internal/domain"
	"github.com/xiongwp/user-merchant-core/internal/repo"
)

// UserService 用户身份 + 认证 + 会话 + RBAC 服务。
type UserService struct {
	repo       repo.UserRepository
	idgen      idgenIface
	issuer     *authpkg.Issuer
	mailer     Mailer
	risk       RiskClient
	accounting AccountingClient
	logger     *zap.Logger

	// otpChallenges challenge_id → (user_id, expires_at, reason)；2FA / risk-review 临时态。
	mu            sync.Mutex
	otpChallenges map[string]otpChallenge

	defaultCurrency string
	codeTTL         time.Duration
	defaultRoleName string // 注册时自动赋的角色，默认 "user"

	// introspectCache 进程内 JWT introspect 缓存（默认 30s TTL / 100k cap）。
	// nil 兼容历史 ctor / 单元测试，IntrospectToken 会回落直接 DB 校验。
	// 配 cache 后单实例 IntrospectToken QPS 可从 ~3K → 30K（命中率 90%+）。
	introspectCache *umcache.IntrospectCache
}

type otpChallenge struct {
	userID        int64
	expiresAt     time.Time
	reason        string // "totp" / "risk_review" / "register_review"
	// register_review 专用：缓存原始 RegisterInput，VerifyOTP 通过后用它
	// 继续走 register completion（建用户 / 写 auths / accounting / JWT）。
	registerInput *RegisterInput
}

// idgenIface 仅暴露 NextID(ctx, biz_tag)，方便单测注入 fake。
type idgenIface interface {
	NextID(ctx context.Context, bizTag string) (int64, error)
}

func NewUserService(
	r repo.UserRepository,
	g idgenIface,
	issuer *authpkg.Issuer,
	mailer Mailer,
	risk RiskClient,
	accounting AccountingClient,
	defaultCurrency string,
	logger *zap.Logger,
) *UserService {
	if defaultCurrency == "" {
		defaultCurrency = "PHP"
	}
	if mailer == nil {
		mailer = &LogMailer{Logger: logger}
	}
	if risk == nil {
		risk = NoopRiskClient{}
	}
	if accounting == nil {
		accounting = NoopAccountingClient{}
	}
	return &UserService{
		repo: r, idgen: g, issuer: issuer, mailer: mailer, risk: risk,
		accounting: accounting, logger: logger,
		otpChallenges:   make(map[string]otpChallenge),
		defaultCurrency: defaultCurrency,
		codeTTL:         10 * time.Minute,
		defaultRoleName: "user",
	}
}

// ─── Register ───────────────────────────────────────────────────────────

type RegisterInput struct {
	Username string
	Email    string
	Phone    string
	Password string
	Currency string

	IPAddress       string
	DeviceID        string
	UserAgent       string
	RiskSessionID   string
	FingerprintHash string
	UAHash          string
	Metadata        map[string]string
}

type RegisterResult struct {
	User         *domain.User
	JWT          string
	RiskReview   bool
	// NeedsOTP=true 时 caller 应跳到 OTP 输入页，业务用 OTPChallenge 调
	// VerifyOTP（同 Login 流复用）。注册流走 step-up 不会建用户、不签 JWT，
	// 用户校验 code 后才走真正的 register completion。
	NeedsOTP     bool
	OTPChallenge string
}

func (s *UserService) Register(ctx context.Context, in *RegisterInput) (*RegisterResult, error) {
	if in == nil || (in.Username == "" && in.Email == "" && in.Phone == "") || in.Password == "" {
		return nil, fmt.Errorf("%w: identifier (username/email/phone) + password required", domain.ErrValidation)
	}

	// 1) 风控前置
	// metadata 字段给 dsl 规则用。account_age_days="0" 让 account_age /
	// welcome_bonus_immediate_withdraw 等规则在 register 路径上能直接拿到上下文。
	// recent_login_failures 是按 IP 维度（userID 还没生成），抓"扫号 → 注册"型 bot。
	failsByIP, _ := s.repo.CountRecentFailedLogins(ctx, 0, in.IPAddress, time.Now().Add(-5*time.Minute))
	rr, err := s.risk.Screen(ctx, &ScreenRequest{
		EventType:       "register",
		MerchantId:      "platform",
		IpAddress:       in.IPAddress,
		DeviceId:        in.DeviceID,
		UserAgent:       in.UserAgent,
		RiskSessionId:   in.RiskSessionID,
		FingerprintHash: in.FingerprintHash,
		IdempotencyKey:  "register:" + identifierOf(in),
		Metadata: mergeMeta(in.Metadata, map[string]string{
			"username":                in.Username,
			"email_domain":            emailDomainOf(in.Email),
			"phone_prefix":            phonePrefixOf(in.Phone),
			"ua_hash":                 in.UAHash,
			"account_age_days":        "0", // 注册即新账号
			"recent_login_failures":   fmt.Sprintf("%d", failsByIP),
		}),
	})
	if err != nil {
		s.logger.Warn("risk screen register failed (fail-open)", zap.Error(err))
		rr = &ScreenResponse{Decision: Decision_ALLOW}
	}
	// DENY 直接拒（含 reason 内部细节落 log，给前端只返通用文案）。
	if rr.Decision == Decision_DENY {
		s.logger.Warn("register blocked by risk",
			zap.String("decision_id", rr.DecisionId),
			zap.String("reason", rr.Reason),
			zap.String("ip", in.IPAddress),
			zap.String("device", in.DeviceID))
		return nil, fmt.Errorf("%w: 无法注册，请稍后再试", domain.ErrValidation)
	}
	// REVIEW → step-up：发邮件 OTP，前端拿到 challenge 跳验证页，验证通过
	// 后才真正建用户（VerifyRegisterOTP）。降低误伤：合法用户 30s 完成 OTP
	// 即可继续，不像之前 hard-DENY 直接被锁出。
	if rr.Decision == Decision_REVIEW {
		if in.Email == "" {
			// 没邮箱不能发 OTP → 退回 DENY
			s.logger.Warn("register review without email; degrade to DENY",
				zap.String("decision_id", rr.DecisionId), zap.String("reason", rr.Reason))
			return nil, fmt.Errorf("%w: 无法注册，请稍后再试", domain.ErrValidation)
		}
		challenge, _ := authpkg.RandomBase32Token()
		s.mu.Lock()
		s.otpChallenges[challenge] = otpChallenge{
			expiresAt:    time.Now().Add(5 * time.Minute),
			reason:       "register_review",
			registerInput: in, // 缓存原始输入，VerifyOTP 通过后用它继续 register
		}
		s.mu.Unlock()
		go s.sendCodeAsync(strings.ToLower(in.Email), "register_otp")
		s.logger.Info("register step-up issued",
			zap.String("decision_id", rr.DecisionId),
			zap.String("email", in.Email))
		return &RegisterResult{NeedsOTP: true, OTPChallenge: challenge, RiskReview: true}, nil
	}
	return s.completeRegister(ctx, in, false)
}

// completeRegister 真正建用户 + 配套副作用（auths / role / accounting / Report / JWT）。
// stepUpVerified=true 表示 caller 已经过了 step-up OTP 校验（VerifyOTP 进来），
// 用于打 audit log 和将来给 risk.Report 加 trust 信号。
func (s *UserService) completeRegister(ctx context.Context, in *RegisterInput, stepUpVerified bool) (*RegisterResult, error) {
	uid, err := s.idgen.NextID(ctx, "user_merchant.user")
	if err != nil {
		return nil, fmt.Errorf("idgen: %w", err)
	}
	hash, err := authpkg.HashPassword(in.Password)
	if err != nil {
		return nil, err
	}
	currency := strings.ToUpper(in.Currency)
	if currency == "" {
		currency = s.defaultCurrency
	}
	user := &domain.User{
		ID:           uid,
		Username:     ptrIfNonEmpty(in.Username),
		Email:        ptrIfNonEmpty(strings.ToLower(in.Email)),
		Phone:        ptrIfNonEmpty(in.Phone),
		PasswordHash: hash,
		Status:       domain.UserStatusActive,
		// step-up 验证过的邮箱直接标记 EmailVerified（用户都收到验证码并填回来了）
		EmailVerified: stepUpVerified && in.Email != "",
	}
	if err := s.repo.CreateUser(ctx, user); err != nil {
		return nil, err
	}

	// user_auths：每个 identifier 一条便于"用 X 登录"反查 user_id
	if in.Username != "" {
		_ = s.repo.AddAuth(ctx, &domain.UserAuth{
			UserID: user.ID, AuthType: domain.AuthTypePassword,
			Identifier: in.Username, Verified: true,
		})
	}
	if in.Email != "" {
		_ = s.repo.AddAuth(ctx, &domain.UserAuth{
			UserID: user.ID, AuthType: domain.AuthTypeEmail,
			Identifier: strings.ToLower(in.Email), Verified: stepUpVerified,
		})
	}
	if in.Phone != "" {
		_ = s.repo.AddAuth(ctx, &domain.UserAuth{
			UserID: user.ID, AuthType: domain.AuthTypePhone,
			Identifier: in.Phone, Verified: false,
		})
	}

	// 默认角色
	if rid := s.defaultRoleID(ctx); rid > 0 {
		_ = s.repo.AssignRole(ctx, user.ID, rid)
	}

	// 开 accounting USER_BALANCE 主账户
	s.openUserBalanceAccount(ctx, user.ID, currency)

	// risk.Report 写图边
	_ = s.risk.Report(ctx, &ReportRequest{
		EventType:  "register",
		MerchantId: "platform",
		CustomerId: fmt.Sprintf("%d", user.ID),
		IpAddress:  in.IPAddress,
		DeviceId:   in.DeviceID,
	})

	// step-up 没验证过 → 仍发常规 verify_email；step-up 已验证 → 邮箱已经
	// 标记 verified，不必再发码
	if in.Email != "" && !stepUpVerified {
		go s.sendCodeAsync(strings.ToLower(in.Email), "verify_email")
	}

	jwt, exp, err := s.issuer.Issue(fmt.Sprintf("%d", user.ID), user.EmailVerified, "user")
	if err != nil {
		return nil, err
	}
	_ = s.repo.CreateSession(ctx, &domain.UserSession{
		UserID: user.ID, Token: jwt, IP: in.IPAddress, UserAgent: in.UserAgent, ExpiresAt: exp,
	})

	return &RegisterResult{User: user, JWT: jwt}, nil
}

// ─── Login ──────────────────────────────────────────────────────────────

type LoginInput struct {
	LoginID  string // 也接受 username / email / phone
	Password string

	IPAddress       string
	DeviceID        string
	UserAgent       string
	RiskSessionID   string
	FingerprintHash string
	Metadata        map[string]string
}

type LoginResult struct {
	JWT          string
	User         *domain.User
	NeedsOTP     bool
	OTPChallenge string
}

func (s *UserService) Login(ctx context.Context, in *LoginInput) (*LoginResult, error) {
	if in == nil || in.LoginID == "" || in.Password == "" {
		return nil, fmt.Errorf("%w: login_id/password required", domain.ErrValidation)
	}

	// 解析 login_id：先 username → email → phone
	user, err := s.lookupUserForLogin(ctx, in.LoginID)
	if err != nil {
		_ = s.repo.InsertLoginLog(ctx, &domain.LoginLog{
			IP: in.IPAddress, UserAgent: in.UserAgent, AuthType: domain.AuthTypePassword,
			Status: 0, Reason: "user_not_found",
		})
		_ = s.risk.Report(ctx, &ReportRequest{
			EventType: "login.failed", MerchantId: "platform",
			IpAddress: in.IPAddress, DeviceId: in.DeviceID,
		})
		return nil, domain.ErrPasswordWrong // 不暴露用户是否存在
	}

	// 状态检查
	if user.Status == domain.UserStatusDisabled || user.Status == domain.UserStatusDeleted {
		_ = s.repo.InsertLoginLog(ctx, &domain.LoginLog{
			UserID: ptrInt64(user.ID), IP: in.IPAddress, UserAgent: in.UserAgent,
			AuthType: domain.AuthTypePassword, Status: 0, Reason: "user_disabled",
		})
		return nil, domain.ErrUserDisabled
	}
	if user.Status == domain.UserStatusLocked {
		if user.LockedUntil != nil && time.Now().Before(*user.LockedUntil) {
			return nil, domain.ErrUserLocked
		}
	}

	// 密码校验
	if !authpkg.CheckPassword(user.PasswordHash, in.Password) {
		n, _ := s.repo.IncrFailedLogin(ctx, user.ID)
		if n >= 5 {
			until := time.Now().Add(15 * time.Minute)
			_ = s.repo.LockUser(ctx, user.ID, until)
			// 锁定后立即让该用户的所有现有 token 失效，
			// 避免攻击者已经持有合法 JWT + 锁定后还能用 cached IntrospectToken
			// 通过校验 30s。
			if s.introspectCache != nil {
				s.introspectCache.InvalidateUser(user.ID)
			}
		}
		_ = s.repo.InsertLoginLog(ctx, &domain.LoginLog{
			UserID: ptrInt64(user.ID), IP: in.IPAddress, UserAgent: in.UserAgent,
			AuthType: domain.AuthTypePassword, Status: 0, Reason: "wrong_password",
		})
		_ = s.risk.Report(ctx, &ReportRequest{
			EventType: "login.failed", MerchantId: "platform",
			CustomerId: fmt.Sprintf("%d", user.ID),
			IpAddress:  in.IPAddress, DeviceId: in.DeviceID,
		})
		return nil, domain.ErrPasswordWrong
	}

	// 风控决策
	// metadata 富化：
	//   - account_age_days：用户注册到现在天数（welcome_bonus / new_account_high_value 用）
	//   - recent_login_failures：最近 5min 失败登录次数（login_fail_burst_5m / login_fail_ip_burst 用）
	//   - login_failed_count_24h：累计 fail 计数（保留旧字段）
	ageDays := int(time.Since(user.CreatedAt).Hours() / 24)
	if ageDays < 0 {
		ageDays = 0
	}
	recentFails, _ := s.repo.CountRecentFailedLogins(ctx, user.ID, in.IPAddress, time.Now().Add(-5*time.Minute))
	// minutes_since_password_change 给 ATO 规则
	// （ato_withdraw_after_password_change 等）。改过密码用 PasswordChangedAt；
	// 没改过用 CreatedAt 兜底（注册即"刚改过密码"）。
	pwdRef := user.CreatedAt
	if user.PasswordChangedAt != nil {
		pwdRef = *user.PasswordChangedAt
	}
	minSincePwd := int(time.Since(pwdRef).Minutes())
	if minSincePwd < 0 {
		minSincePwd = 0
	}
	rr, err := s.risk.Screen(ctx, &ScreenRequest{
		EventType:       "login",
		MerchantId:      "platform",
		CustomerId:      fmt.Sprintf("%d", user.ID),
		IpAddress:       in.IPAddress,
		DeviceId:        in.DeviceID,
		UserAgent:       in.UserAgent,
		RiskSessionId:   in.RiskSessionID,
		FingerprintHash: in.FingerprintHash,
		IdempotencyKey:  fmt.Sprintf("login:%d:%s", user.ID, in.RiskSessionID),
		Metadata: mergeMeta(in.Metadata, map[string]string{
			"login_failed_count_24h":        fmt.Sprintf("%d", user.FailedLoginCount),
			"account_age_days":              fmt.Sprintf("%d", ageDays),
			"recent_login_failures":         fmt.Sprintf("%d", recentFails),
			"minutes_since_password_change": fmt.Sprintf("%d", minSincePwd),
		}),
	})
	if err != nil {
		s.logger.Warn("risk screen login failed (fail-open)", zap.Error(err))
		rr = &ScreenResponse{Decision: Decision_ALLOW}
	}
	if rr.Decision == Decision_DENY {
		s.logger.Warn("login blocked by risk",
			zap.String("decision_id", rr.DecisionId),
			zap.String("reason", rr.Reason),
			zap.String("ip", in.IPAddress),
			zap.String("device", in.DeviceID))
		return nil, fmt.Errorf("%w: 无法登录，请稍后再试", domain.ErrValidation)
	}
	needsOTP := user.TOTPEnabled || rr.Decision == Decision_REVIEW

	if needsOTP {
		challenge, _ := authpkg.RandomBase32Token()
		reason := "totp"
		if !user.TOTPEnabled {
			reason = "risk_review"
			if user.Email != nil && *user.Email != "" {
				go s.sendCodeAsync(*user.Email, "login_otp")
			}
		}
		s.mu.Lock()
		s.otpChallenges[challenge] = otpChallenge{
			userID: user.ID, expiresAt: time.Now().Add(5 * time.Minute), reason: reason,
		}
		s.mu.Unlock()
		return &LoginResult{User: user, NeedsOTP: true, OTPChallenge: challenge}, nil
	}

	return s.completeLogin(ctx, user, in.IPAddress, in.UserAgent, in.DeviceID)
}

// VerifyOTP 校验 challenge + code。返回的 LoginResult 在 register step-up
// 完成后也用相同结构（user + jwt）—— caller 看 needsOTP=false + jwt 非空即认为
// 流程完成（不论原 entrypoint 是 register 还是 login）。
func (s *UserService) VerifyOTP(ctx context.Context, challenge, code string) (*LoginResult, error) {
	s.mu.Lock()
	c, ok := s.otpChallenges[challenge]
	if ok {
		delete(s.otpChallenges, challenge)
	}
	s.mu.Unlock()
	if !ok || time.Now().After(c.expiresAt) {
		return nil, domain.ErrOTPWrong
	}
	// register_review 路径：用户还没建，先验码再继续 register
	if c.reason == "register_review" {
		if c.registerInput == nil || c.registerInput.Email == "" {
			return nil, domain.ErrOTPWrong
		}
		ec, err := s.repo.FindLatestActiveCode(ctx, strings.ToLower(c.registerInput.Email), "register_otp")
		if err != nil {
			return nil, domain.ErrOTPWrong
		}
		if hashCode(code) != ec.CodeHash {
			_ = s.repo.IncrCodeAttempts(ctx, ec.ID)
			return nil, domain.ErrOTPWrong
		}
		_ = s.repo.MarkCodeUsed(ctx, ec.ID)
		// 验证通过 → 真正建用户。skipRiskScreen 标记让 completeRegister 不再
		// 二次 risk.Screen（已经过审，避免回到 REVIEW 死循环）。
		res, err := s.completeRegister(ctx, c.registerInput, true)
		if err != nil {
			return nil, err
		}
		return &LoginResult{User: res.User, JWT: res.JWT}, nil
	}
	user, err := s.repo.GetUser(ctx, c.userID)
	if err != nil {
		return nil, err
	}
	switch c.reason {
	case "totp":
		if !authpkg.VerifyTOTP(user.TOTPSecret, code) {
			return nil, domain.ErrOTPWrong
		}
	case "risk_review":
		if user.Email == nil || *user.Email == "" {
			return nil, domain.ErrOTPWrong
		}
		ec, err := s.repo.FindLatestActiveCode(ctx, *user.Email, "login_otp")
		if err != nil {
			return nil, domain.ErrOTPWrong
		}
		if hashCode(code) != ec.CodeHash {
			_ = s.repo.IncrCodeAttempts(ctx, ec.ID)
			return nil, domain.ErrOTPWrong
		}
		_ = s.repo.MarkCodeUsed(ctx, ec.ID)
	}
	return s.completeLogin(ctx, user, "", "", "")
}

func (s *UserService) completeLogin(ctx context.Context, user *domain.User, ip, ua, dev string) (*LoginResult, error) {
	_ = s.repo.UpdateLastLogin(ctx, user.ID, time.Now())
	_ = s.risk.Report(ctx, &ReportRequest{
		EventType: "login", MerchantId: "platform",
		CustomerId: fmt.Sprintf("%d", user.ID),
		IpAddress:  ip, DeviceId: dev,
	})
	jwt, exp, err := s.issuer.Issue(fmt.Sprintf("%d", user.ID), user.EmailVerified, "user")
	if err != nil {
		return nil, err
	}
	_ = s.repo.CreateSession(ctx, &domain.UserSession{
		UserID: user.ID, Token: jwt, IP: ip, UserAgent: ua, ExpiresAt: exp,
	})
	_ = s.repo.InsertLoginLog(ctx, &domain.LoginLog{
		UserID: ptrInt64(user.ID), IP: ip, UserAgent: ua,
		AuthType: domain.AuthTypePassword, Status: 1,
	})
	return &LoginResult{JWT: jwt, User: user}, nil
}

// ─── lookupUserForLogin ────────────────────────────────────────────────

// lookupUserForLogin 按 username → email → phone 查 user_id。
func (s *UserService) lookupUserForLogin(ctx context.Context, loginID string) (*domain.User, error) {
	loginID = strings.TrimSpace(loginID)
	if u, err := s.repo.GetUserByUsername(ctx, loginID); err == nil {
		return u, nil
	}
	if strings.Contains(loginID, "@") {
		if u, err := s.repo.GetUserByEmail(ctx, strings.ToLower(loginID)); err == nil {
			return u, nil
		}
	}
	if strings.HasPrefix(loginID, "+") || isAllDigits(loginID) {
		if u, err := s.repo.GetUserByPhone(ctx, loginID); err == nil {
			return u, nil
		}
	}
	return nil, domain.ErrUserNotFound
}

// ─── SendCode / VerifyCode ──────────────────────────────────────────────

func (s *UserService) SendCode(ctx context.Context, identifier, purpose string) error {
	if identifier == "" || purpose == "" {
		return fmt.Errorf("%w: identifier/purpose required", domain.ErrValidation)
	}
	code, err := authpkg.GenerateNumericCode(6)
	if err != nil {
		return err
	}
	if err := s.repo.CreateEmailCode(ctx, &domain.EmailCode{
		Identifier: identifier, CodeHash: hashCode(code), Purpose: purpose,
		ExpiresAt: time.Now().Add(s.codeTTL),
	}); err != nil {
		return err
	}
	return s.mailer.Send(ctx, identifier, purpose, code)
}

func (s *UserService) VerifyCode(ctx context.Context, identifier, code, purpose string) error {
	ec, err := s.repo.FindLatestActiveCode(ctx, identifier, purpose)
	if err != nil {
		return err
	}
	if ec.Attempts > 5 {
		return domain.ErrEmailCodeExpired
	}
	if hashCode(code) != ec.CodeHash {
		_ = s.repo.IncrCodeAttempts(ctx, ec.ID)
		return domain.ErrOTPWrong
	}
	// 资金 / 安全修复：MarkCodeUsed 现在 CAS（WHERE used_at IS NULL）。
	// 并发 race 输的请求拿 ErrEmailCodeUsed → 不能继续做敏感操作（reset
	// password / bind phone / verify email）。
	if err := s.repo.MarkCodeUsed(ctx, ec.ID); err != nil {
		return err
	}
	switch purpose {
	case "verify_email":
		_ = s.repo.MarkAuthVerified(ctx, domain.AuthTypeEmail, identifier)
	case "bind_phone":
		_ = s.repo.MarkAuthVerified(ctx, domain.AuthTypePhone, identifier)
	}
	return nil
}

func (s *UserService) sendCodeAsync(identifier, purpose string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.SendCode(ctx, identifier, purpose); err != nil {
		s.logger.Warn("sendCodeAsync failed",
			zap.String("identifier", identifier),
			zap.String("purpose", purpose), zap.Error(err))
	}
}

// ─── Identity binding ───────────────────────────────────────────────────

func (s *UserService) BindAuth(ctx context.Context, userID int64, authType, identifier, credential, emailCode string) (*domain.UserAuth, error) {
	if authType == domain.AuthTypeEmail || authType == domain.AuthTypePhone {
		purpose := "verify_email"
		if authType == domain.AuthTypePhone {
			purpose = "bind_phone"
		}
		if err := s.VerifyCode(ctx, identifier, emailCode, purpose); err != nil {
			return nil, err
		}
	}
	a := &domain.UserAuth{
		UserID: userID, AuthType: authType, Identifier: identifier,
		Credential: credential, Verified: true,
	}
	if err := s.repo.AddAuth(ctx, a); err != nil {
		return nil, err
	}
	return a, nil
}

func (s *UserService) UnbindAuth(ctx context.Context, userID int64, authType, identifier string) error {
	return s.repo.RemoveAuth(ctx, userID, authType, identifier)
}

func (s *UserService) ListAuths(ctx context.Context, userID int64) ([]*domain.UserAuth, error) {
	return s.repo.ListAuths(ctx, userID)
}

// ─── 2FA ────────────────────────────────────────────────────────────────

func (s *UserService) EnableTOTP(ctx context.Context, userID int64) (secret, otpauthURL string, err error) {
	user, err := s.repo.GetUser(ctx, userID)
	if err != nil {
		return "", "", err
	}
	accountName := fmt.Sprintf("user_%d", user.ID)
	if user.Username != nil && *user.Username != "" {
		accountName = *user.Username
	} else if user.Email != nil && *user.Email != "" {
		accountName = *user.Email
	}
	secret, otpauthURL, err = authpkg.NewTOTPSecret("user-merchant-core", accountName)
	if err != nil {
		return "", "", err
	}
	user.TOTPSecret = secret
	user.TOTPEnabled = true
	if err := s.repo.UpdateUser(ctx, user); err != nil {
		return "", "", err
	}
	return secret, otpauthURL, nil
}

// ─── ChangePassword ─────────────────────────────────────────────────────

func (s *UserService) ChangePassword(ctx context.Context, userID int64, oldPassword, emailCode, newPassword string) error {
	user, err := s.repo.GetUser(ctx, userID)
	if err != nil {
		return err
	}
	if oldPassword != "" {
		if !authpkg.CheckPassword(user.PasswordHash, oldPassword) {
			return domain.ErrPasswordWrong
		}
	} else if emailCode != "" {
		if user.Email == nil || *user.Email == "" {
			return fmt.Errorf("%w: no email bound", domain.ErrValidation)
		}
		if err := s.VerifyCode(ctx, *user.Email, emailCode, "reset_password"); err != nil {
			return err
		}
	} else {
		return fmt.Errorf("%w: old_password or email_code required", domain.ErrValidation)
	}
	hash, err := authpkg.HashPassword(newPassword)
	if err != nil {
		return err
	}
	user.PasswordHash = hash
	now := time.Now()
	user.PasswordChangedAt = &now
	// 改密码 → 撤销所有现有 session（用户在所有设备重新登录）。
	// cache 必须同步清，否则该用户用旧 JWT 在 30s 内仍可访问受保护资源。
	if s.introspectCache != nil {
		s.introspectCache.InvalidateUser(user.ID)
	}
	_ = s.repo.DeleteAllUserSessions(ctx, user.ID)
	if err := s.repo.UpdateUser(ctx, user); err != nil {
		return err
	}
	// 异步上报 password_change 事件给风控（ATO 规则用）。
	// 不等结果，不阻断本次改密返回；超时 1s 兜底。
	go func() {
		rctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.risk.Report(rctx, &ReportRequest{
			EventType:  "password_change",
			MerchantId: "platform",
			CustomerId: fmt.Sprintf("%d", user.ID),
		})
	}()
	return nil
}

// ─── Session / Introspect / Logout ──────────────────────────────────────

type IntrospectResult struct {
	Valid         bool
	UserID        int64
	EmailVerified bool
	ExpiresAt     time.Time
	Scopes        []string
	Permissions   []string // 权限码列表（admin 端 RBAC 检查用）
}

// SetIntrospectCache 注入进程内 introspect 缓存。
// 应该在 NewUserService 之后立即调（main.go 启动期）。
// 可重入：多次调会替换实例（测试场景换 cache）；nil 表示关闭缓存。
func (s *UserService) SetIntrospectCache(c *umcache.IntrospectCache) {
	s.introspectCache = c
}

// IntrospectToken SSO 入口：验签 JWT + 在 user_sessions 查 token 是否仍有效。
// 双重验证防 JWT 泄漏后无法即时撤销的问题（登出 / 风控冻结时删 session row）。
//
// 性能：
//
//	无 cache：每次调用 1 GetSession + 1 ListPermissions = 2 DB roundtrip。
//	         10K TPS × 3 跳 = 30K DB SELECT/sec（user_session 单 shard ~3K SELECT/sec
//	         勉强支撑，延迟堆积）。
//	有 cache（30s TTL）：典型命中率 90%+。
//	         实际 DB ~3K SELECT/sec，单 shard 余量充足。
//
// 安全（缓存层）：
//
//	(1) Logout / LogoutAll / 改密 / 风控冻结后 30s 内仍可能命中缓存。
//	    → IntrospectCache.InvalidateUser 在所有 session 撤销路径上同步调用，
//	      保证 100% 流量看到撤销事件。
//	(2) 缓存只装 Valid==true 的结果。无效 token 仍每次走 DB 校验，
//	    避免攻击者通过暴力构造 invalid token 把 cache 撑爆。
//	(3) Cache key = SHA256(jwt)，进程 heap dump 也拿不到原 token。
func (s *UserService) IntrospectToken(ctx context.Context, jwt string) (*IntrospectResult, error) {
	// fast path: 进程内缓存。
	if s.introspectCache != nil {
		if v, ok := s.introspectCache.Get(jwt); ok {
			return &IntrospectResult{
				Valid:         true,
				UserID:        v.UserID,
				EmailVerified: v.EmailVerified,
				ExpiresAt:     v.ExpiresAt,
				Scopes:        v.Scopes,
				Permissions:   v.Permissions,
			}, nil
		}
	}

	c, err := s.issuer.Verify(jwt)
	if err != nil {
		return &IntrospectResult{Valid: false}, nil
	}
	// session 表二次校验
	sess, err := s.repo.GetSession(ctx, jwt)
	if err != nil || time.Now().After(sess.ExpiresAt) {
		return &IntrospectResult{Valid: false}, nil
	}
	uid := sess.UserID
	// permissions 计算
	perms, _ := s.repo.ListPermissions(ctx, uid)
	codes := make([]string, 0, len(perms))
	for _, p := range perms {
		codes = append(codes, p.Code)
	}
	res := &IntrospectResult{
		Valid: true, UserID: uid, EmailVerified: c.EmailVerified,
		ExpiresAt: c.ExpiresAt.Time, Scopes: c.Scopes, Permissions: codes,
	}

	// populate cache：只装 Valid==true，按 ExpiresAt 截断到 cache TTL。
	if s.introspectCache != nil {
		s.introspectCache.Put(jwt, umcache.IntrospectCacheValue{
			UserID:        uid,
			EmailVerified: c.EmailVerified,
			ExpiresAt:     c.ExpiresAt.Time,
			Scopes:        c.Scopes,
			Permissions:   codes,
		})
	}
	return res, nil
}

// Logout 删除 session row；JWT 仍可被验签但 IntrospectToken 会判 invalid。
//
// 缓存：先 Invalidate(jwt) 再 DeleteSession。顺序不能反 ——
// 反过来若 DeleteSession 后 cache 被并发命中，会让该 token 撤销前
// 30s 仍可用。先 Invalidate 即使 DeleteSession 失败下次还会重试（idempotent）。
func (s *UserService) Logout(ctx context.Context, jwt string) error {
	if s.introspectCache != nil {
		s.introspectCache.Invalidate(jwt)
	}
	return s.repo.DeleteSession(ctx, jwt)
}

// LogoutAll 删除该用户所有设备 session（改密码 / 风控冻结时调）。
// 缓存按 user_id 一并清，确保多设备 token 全部即时失效。
func (s *UserService) LogoutAll(ctx context.Context, userID int64) error {
	if s.introspectCache != nil {
		s.introspectCache.InvalidateUser(userID)
	}
	return s.repo.DeleteAllUserSessions(ctx, userID)
}

// ─── RBAC ──────────────────────────────────────────────────────────────

func (s *UserService) HasPermission(ctx context.Context, userID int64, code string) (bool, error) {
	return s.repo.HasPermission(ctx, userID, code)
}

func (s *UserService) ListPermissions(ctx context.Context, userID int64) ([]*domain.Permission, error) {
	return s.repo.ListPermissions(ctx, userID)
}

func (s *UserService) ListRoles(ctx context.Context, userID int64) ([]*domain.Role, error) {
	return s.repo.ListRoles(ctx, userID)
}

func (s *UserService) AssignRole(ctx context.Context, userID, roleID int64) error {
	return s.repo.AssignRole(ctx, userID, roleID)
}

func (s *UserService) RemoveRole(ctx context.Context, userID, roleID int64) error {
	return s.repo.RemoveRole(ctx, userID, roleID)
}

// ─── Profile / Get / Wallet / Settings ────────────────────────────────

func (s *UserService) GetUser(ctx context.Context, userID int64) (*domain.User, error) {
	return s.repo.GetUser(ctx, userID)
}

func (s *UserService) GetProfile(ctx context.Context, userID int64) (*domain.UserProfile, error) {
	return s.repo.GetProfile(ctx, userID)
}

func (s *UserService) UpsertProfile(ctx context.Context, p *domain.UserProfile) error {
	return s.repo.UpsertProfile(ctx, p)
}

// ListUserAccounts 列出用户已开通的多币种账户（链接到 accounting-system 的 account_id）。
func (s *UserService) ListUserAccounts(ctx context.Context, userID int64) ([]*domain.UserAccount, error) {
	return s.repo.ListUserAccounts(ctx, userID)
}

// GetUserAccount 拿到用户在某币种下的账户引用。未开通返回 (nil, nil)。
func (s *UserService) GetUserAccount(ctx context.Context, userID int64, currency string) (*domain.UserAccount, error) {
	return s.repo.GetUserAccount(ctx, userID, strings.ToUpper(currency))
}

// BindUserAccount 调用方在 accounting-system CreateAccount 拿到 account_id 后调本接口。
// 同 user_id+currency 已存在直接返回 nil（幂等）。
func (s *UserService) BindUserAccount(ctx context.Context, userID int64, currency, accountID string) error {
	cur := strings.ToUpper(currency)
	if cur == "" || accountID == "" {
		return fmt.Errorf("%w: currency/account_id required", domain.ErrValidation)
	}
	exists, _ := s.repo.GetUserAccount(ctx, userID, cur)
	if exists != nil {
		return nil
	}
	return s.repo.AddUserAccount(ctx, &domain.UserAccount{
		UserID: userID, Currency: cur, AccountID: accountID, Status: "active",
	})
}

// openUserBalanceAccount 调 accounting-system 建 USER_BALANCE 账户，拿 account_no
// 后写 user_accounts 表绑定。失败只 warn 不阻断注册（首次支付兜底重试）。
//
// 幂等：accounting (user_id, ACCOUNT_BUSINESS_TYPE_USER_BALANCE) 是唯一键，
// 重复调返回 code=409；本仓 user_accounts (user_id, currency) 也是唯一键，
// 重复 AddUserAccount 由 GetUserAccount 前置去重。
func (s *UserService) openUserBalanceAccount(ctx context.Context, userID int64, currency string) {
	cur := strings.ToUpper(currency)
	if cur == "" {
		cur = s.defaultCurrency
	}
	// 已绑定 → skip（idempotent）
	if exists, _ := s.repo.GetUserAccount(ctx, userID, cur); exists != nil {
		return
	}
	resp, err := s.accounting.CreateAccount(ctx, &CreateAccountRequest{
		UserId:              userID,
		AccountType:         AccountType_ACCOUNT_TYPE_USER,
		Category:            AccountCategory_ACCOUNT_CATEGORY_LIABILITY, // 用户余额是平台对用户的负债
		Currency:            cur,
		Description:         fmt.Sprintf("user %d default %s balance", userID, cur),
		AccountBusinessType: AccountBusinessType_ACCOUNT_BUSINESS_TYPE_USER_BALANCE,
	})
	if err != nil {
		s.logger.Warn("accounting.CreateAccount failed; skip bind (will retry on first payment)",
			zap.Int64("user_id", userID), zap.String("currency", cur), zap.Error(err))
		return
	}
	if resp.GetAccount() == nil || resp.GetAccount().GetAccountNo() == "" {
		s.logger.Warn("accounting.CreateAccount returned empty account",
			zap.Int64("user_id", userID), zap.String("currency", cur),
			zap.Int32("code", resp.GetCode()), zap.String("message", resp.GetMessage()))
		return
	}
	if err := s.repo.AddUserAccount(ctx, &domain.UserAccount{
		UserID:    userID,
		Currency:  cur,
		AccountID: resp.GetAccount().GetAccountNo(),
		Status:    "active",
	}); err != nil {
		s.logger.Warn("AddUserAccount bind failed",
			zap.Int64("user_id", userID), zap.String("currency", cur), zap.Error(err))
	}
}

// defaultRoleID 通过 name 查 roles 表（默认角色 "user"）；找不到返回 0，调用方
// skip AssignRole（首次部署 RBAC 表数据没初始化时不阻断注册）。
func (s *UserService) defaultRoleID(ctx context.Context) int64 {
	r, err := s.repo.FindRoleByName(ctx, s.defaultRoleName)
	if err != nil || r == nil {
		return 0
	}
	return r.ID
}

func (s *UserService) GetSettings(ctx context.Context, userID int64) (string, error) {
	return s.repo.GetSettings(ctx, userID)
}

func (s *UserService) UpsertSettings(ctx context.Context, userID int64, settingsJSON string) error {
	return s.repo.UpsertSettings(ctx, userID, settingsJSON)
}

func (s *UserService) DeleteUser(ctx context.Context, userID int64, reason string) error {
	// 软删用户：先 cache invalidate（避免旧 token 30s 残留），再清 session row、软删记录。
	if s.introspectCache != nil {
		s.introspectCache.InvalidateUser(userID)
	}
	_ = s.repo.DeleteAllUserSessions(ctx, userID)
	return s.repo.SoftDelete(ctx, userID, reason)
}

// ─── helpers ────────────────────────────────────────────────────────────

func hashCode(code string) string {
	h := sha256.Sum256([]byte(code))
	return hex.EncodeToString(h[:])
}

func emailDomainOf(email string) string {
	if i := strings.IndexByte(email, '@'); i >= 0 {
		return strings.ToLower(email[i+1:])
	}
	return ""
}

func phonePrefixOf(phone string) string {
	if len(phone) >= 4 && phone[0] == '+' {
		return phone[:4]
	}
	return ""
}

func mergeMeta(a, b map[string]string) map[string]string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		if v != "" {
			out[k] = v
		}
	}
	for k, v := range b {
		if v != "" {
			out[k] = v
		}
	}
	return out
}

func ptrIfNonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func ptrInt64(v int64) *int64 { return &v }

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func identifierOf(in *RegisterInput) string {
	if in.Username != "" {
		return in.Username
	}
	if in.Email != "" {
		return strings.ToLower(in.Email)
	}
	return in.Phone
}

// ─── unused import suppressor ───
var _ = errors.New
