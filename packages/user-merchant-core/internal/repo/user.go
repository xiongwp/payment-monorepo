package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/xiongwp/payment-util/shadow"
	"github.com/xiongwp/user-merchant-core/internal/domain"
)

// 11 张用户域表的主 / 影子表名 helper（与 init_shadow.sql 一一对应）
const (
	tblUsers           = "users"
	tblUserProfiles    = "user_profiles"
	tblUserAuths       = "user_auths"
	tblLoginLogs       = "login_logs"
	tblUserSessions    = "user_sessions"
	tblUserAccounts    = "user_accounts"
	tblUserSettings    = "user_settings"
	tblRoles           = "roles"
	tblUserRoles       = "user_roles"
	tblPermissions     = "permissions"
	tblRolePermissions = "role_permissions"
	tblEmailCodes      = "email_codes"
)

func usersTbl(ctx context.Context) string         { return shadow.TableName(ctx, tblUsers) }
func userProfilesTbl(ctx context.Context) string  { return shadow.TableName(ctx, tblUserProfiles) }
func userAuthsTbl(ctx context.Context) string     { return shadow.TableName(ctx, tblUserAuths) }
func loginLogsTbl(ctx context.Context) string     { return shadow.TableName(ctx, tblLoginLogs) }
func userSessionsTbl(ctx context.Context) string  { return shadow.TableName(ctx, tblUserSessions) }
func userAccountsTbl(ctx context.Context) string  { return shadow.TableName(ctx, tblUserAccounts) }
func userSettingsTbl(ctx context.Context) string  { return shadow.TableName(ctx, tblUserSettings) }
func rolesTbl(ctx context.Context) string         { return shadow.TableName(ctx, tblRoles) }
func userRolesTbl(ctx context.Context) string     { return shadow.TableName(ctx, tblUserRoles) }
func permissionsTbl(ctx context.Context) string   { return shadow.TableName(ctx, tblPermissions) }
func rolePermissionsTbl(ctx context.Context) string {
	return shadow.TableName(ctx, tblRolePermissions)
}
func emailCodesTbl(ctx context.Context) string { return shadow.TableName(ctx, tblEmailCodes) }

// UserRepository 用户身份 + 多渠道认证 + 会话 + 权限 + 钱包 + 设置 + 验证码
// 12 张表的 CRUD。所有走 meta DB（不分片）。
//
// 所有访问点都按 ctx 解析主 / 影子表名，shadow 流量自动落到 *_shadow。
type UserRepository interface {
	CreateUser(ctx context.Context, u *domain.User) error
	GetUser(ctx context.Context, userID int64) (*domain.User, error)
	GetUserByUsername(ctx context.Context, username string) (*domain.User, error)
	GetUserByEmail(ctx context.Context, email string) (*domain.User, error)
	GetUserByPhone(ctx context.Context, phone string) (*domain.User, error)
	UpdateUser(ctx context.Context, u *domain.User) error
	UpdateLastLogin(ctx context.Context, userID int64, t time.Time) error
	IncrFailedLogin(ctx context.Context, userID int64) (int, error)
	ResetFailedLogin(ctx context.Context, userID int64) error
	LockUser(ctx context.Context, userID int64, until time.Time) error
	SoftDelete(ctx context.Context, userID int64, reason string) error

	UpsertProfile(ctx context.Context, p *domain.UserProfile) error
	GetProfile(ctx context.Context, userID int64) (*domain.UserProfile, error)

	AddAuth(ctx context.Context, a *domain.UserAuth) error
	FindAuth(ctx context.Context, authType, identifier string) (*domain.UserAuth, error)
	ListAuths(ctx context.Context, userID int64) ([]*domain.UserAuth, error)
	RemoveAuth(ctx context.Context, userID int64, authType, identifier string) error
	MarkAuthVerified(ctx context.Context, authType, identifier string) error

	InsertLoginLog(ctx context.Context, l *domain.LoginLog) error
	CountRecentFailedLogins(ctx context.Context, userID int64, ip string, since time.Time) (int, error)

	CreateSession(ctx context.Context, s *domain.UserSession) error
	GetSession(ctx context.Context, token string) (*domain.UserSession, error)
	DeleteSession(ctx context.Context, token string) error
	DeleteAllUserSessions(ctx context.Context, userID int64) error

	AddUserAccount(ctx context.Context, a *domain.UserAccount) error
	ListUserAccounts(ctx context.Context, userID int64) ([]*domain.UserAccount, error)
	GetUserAccount(ctx context.Context, userID int64, currency string) (*domain.UserAccount, error)

	UpsertSettings(ctx context.Context, userID int64, settingsJSON string) error
	GetSettings(ctx context.Context, userID int64) (string, error)

	AssignRole(ctx context.Context, userID, roleID int64) error
	RemoveRole(ctx context.Context, userID, roleID int64) error
	ListRoles(ctx context.Context, userID int64) ([]*domain.Role, error)
	ListPermissions(ctx context.Context, userID int64) ([]*domain.Permission, error)
	HasPermission(ctx context.Context, userID int64, code string) (bool, error)
	FindRoleByName(ctx context.Context, name string) (*domain.Role, error)

	CreateEmailCode(ctx context.Context, c *domain.EmailCode) error
	FindLatestActiveCode(ctx context.Context, identifier, purpose string) (*domain.EmailCode, error)
	IncrCodeAttempts(ctx context.Context, codeID int64) error
	MarkCodeUsed(ctx context.Context, codeID int64) error
}

type userRepo struct{ mgr *Manager }

func NewUserRepository(mgr *Manager) UserRepository {
	return &userRepo{mgr: mgr}
}

func (r *userRepo) db() *gorm.DB   { return r.mgr.GetMeta() }
func (r *userRepo) dbRO() *gorm.DB { return r.mgr.GetMetaRO() }

// ── User ─────────────────────────────────────────────────────────────

func (r *userRepo) CreateUser(ctx context.Context, u *domain.User) error {
	if u.PasswordHash == "" {
		return fmt.Errorf("%w: password_hash required", domain.ErrValidation)
	}
	if u.Status == 0 {
		u.Status = domain.UserStatusActive
	}
	err := r.db().WithContext(ctx).Table(usersTbl(ctx)).Create(u).Error
	if err != nil {
		msg := err.Error()
		if errors.Is(err, gorm.ErrDuplicatedKey) || containsAny(msg, "Duplicate", "duplicate") {
			if containsAny(msg, "username") {
				return domain.ErrUsernameExists
			}
			if containsAny(msg, "email") {
				return domain.ErrEmailExists
			}
			if containsAny(msg, "phone") {
				return domain.ErrPhoneExists
			}
			return domain.ErrUsernameExists
		}
	}
	return err
}

func (r *userRepo) GetUser(ctx context.Context, userID int64) (*domain.User, error) {
	var u domain.User
	err := r.dbRO().WithContext(ctx).Table(usersTbl(ctx)).Where("id = ?", userID).First(&u).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrUserNotFound
	}
	return &u, err
}

func (r *userRepo) GetUserByUsername(ctx context.Context, username string) (*domain.User, error) {
	var u domain.User
	err := r.dbRO().WithContext(ctx).Table(usersTbl(ctx)).Where("username = ?", username).Take(&u).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrUserNotFound
	}
	return &u, err
}

func (r *userRepo) GetUserByEmail(ctx context.Context, email string) (*domain.User, error) {
	var u domain.User
	err := r.dbRO().WithContext(ctx).Table(usersTbl(ctx)).Where("email = ?", email).Take(&u).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrUserNotFound
	}
	return &u, err
}

func (r *userRepo) GetUserByPhone(ctx context.Context, phone string) (*domain.User, error) {
	var u domain.User
	err := r.dbRO().WithContext(ctx).Table(usersTbl(ctx)).Where("phone = ?", phone).Take(&u).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrUserNotFound
	}
	return &u, err
}

func (r *userRepo) UpdateUser(ctx context.Context, u *domain.User) error {
	return r.db().WithContext(ctx).Table(usersTbl(ctx)).Save(u).Error
}

func (r *userRepo) UpdateLastLogin(ctx context.Context, userID int64, t time.Time) error {
	return r.db().WithContext(ctx).Table(usersTbl(ctx)).
		Where("id = ?", userID).
		Updates(map[string]any{"last_login_at": t, "failed_login_count": 0, "locked_until": nil}).Error
}

func (r *userRepo) IncrFailedLogin(ctx context.Context, userID int64) (int, error) {
	tbl := usersTbl(ctx)
	tx := r.db().WithContext(ctx).Begin()
	defer func() {
		if p := recover(); p != nil {
			tx.Rollback()
			panic(p)
		}
	}()
	if err := tx.Table(tbl).
		Where("id = ?", userID).
		UpdateColumn("failed_login_count", gorm.Expr("failed_login_count + 1")).Error; err != nil {
		tx.Rollback()
		return 0, err
	}
	var u domain.User
	if err := tx.Table(tbl).Where("id = ?", userID).First(&u).Error; err != nil {
		tx.Rollback()
		return 0, err
	}
	if err := tx.Commit().Error; err != nil {
		return 0, err
	}
	return u.FailedLoginCount, nil
}

func (r *userRepo) ResetFailedLogin(ctx context.Context, userID int64) error {
	return r.db().WithContext(ctx).Table(usersTbl(ctx)).
		Where("id = ?", userID).
		Updates(map[string]any{"failed_login_count": 0, "locked_until": nil}).Error
}

func (r *userRepo) LockUser(ctx context.Context, userID int64, until time.Time) error {
	return r.db().WithContext(ctx).Table(usersTbl(ctx)).
		Where("id = ?", userID).
		Updates(map[string]any{"status": domain.UserStatusLocked, "locked_until": until}).Error
}

func (r *userRepo) SoftDelete(ctx context.Context, userID int64, _ string) error {
	return r.db().WithContext(ctx).Table(usersTbl(ctx)).
		Where("id = ?", userID).
		Updates(map[string]any{
			"status":   domain.UserStatusDeleted,
			"username": nil, "email": nil, "phone": nil,
			"password_hash": "", "totp_secret": "",
		}).Error
}

// ── Profile ──────────────────────────────────────────────────────────

func (r *userRepo) UpsertProfile(ctx context.Context, p *domain.UserProfile) error {
	return r.db().WithContext(ctx).Table(userProfilesTbl(ctx)).Save(p).Error
}

func (r *userRepo) GetProfile(ctx context.Context, userID int64) (*domain.UserProfile, error) {
	var p domain.UserProfile
	err := r.dbRO().WithContext(ctx).Table(userProfilesTbl(ctx)).Where("user_id = ?", userID).Take(&p).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &p, err
}

// ── Auth ─────────────────────────────────────────────────────────────

func (r *userRepo) AddAuth(ctx context.Context, a *domain.UserAuth) error {
	if a.UserID == 0 || a.AuthType == "" || a.Identifier == "" {
		return fmt.Errorf("%w: user_id/auth_type/identifier required", domain.ErrValidation)
	}
	err := r.db().WithContext(ctx).Table(userAuthsTbl(ctx)).Create(a).Error
	if err != nil {
		msg := err.Error()
		if errors.Is(err, gorm.ErrDuplicatedKey) || containsAny(msg, "Duplicate", "duplicate") {
			return domain.ErrAuthExists
		}
	}
	return err
}

func (r *userRepo) FindAuth(ctx context.Context, authType, identifier string) (*domain.UserAuth, error) {
	var a domain.UserAuth
	err := r.dbRO().WithContext(ctx).Table(userAuthsTbl(ctx)).
		Where("auth_type = ? AND identifier = ?", authType, identifier).Take(&a).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrAuthNotFound
	}
	return &a, err
}

func (r *userRepo) ListAuths(ctx context.Context, userID int64) ([]*domain.UserAuth, error) {
	var out []*domain.UserAuth
	err := r.dbRO().WithContext(ctx).Table(userAuthsTbl(ctx)).Where("user_id = ?", userID).Find(&out).Error
	return out, err
}

func (r *userRepo) RemoveAuth(ctx context.Context, userID int64, authType, identifier string) error {
	return r.db().WithContext(ctx).Table(userAuthsTbl(ctx)).
		Where("user_id = ? AND auth_type = ? AND identifier = ?", userID, authType, identifier).
		Delete(&domain.UserAuth{}).Error
}

func (r *userRepo) MarkAuthVerified(ctx context.Context, authType, identifier string) error {
	return r.db().WithContext(ctx).Table(userAuthsTbl(ctx)).
		Where("auth_type = ? AND identifier = ?", authType, identifier).
		Update("verified", true).Error
}

// ── Login log / Session / Wallet / Settings / RBAC / EmailCode ──────────

func (r *userRepo) InsertLoginLog(ctx context.Context, l *domain.LoginLog) error {
	return r.db().WithContext(ctx).Table(loginLogsTbl(ctx)).Create(l).Error
}

func (r *userRepo) CountRecentFailedLogins(ctx context.Context, userID int64, ip string, since time.Time) (int, error) {
	var n int64
	q := r.dbRO().WithContext(ctx).Table(loginLogsTbl(ctx)).
		Where("status = 0 AND created_at >= ?", since)
	if userID > 0 && ip != "" {
		q = q.Where("user_id = ? OR ip = ?", userID, ip)
	} else if userID > 0 {
		q = q.Where("user_id = ?", userID)
	} else if ip != "" {
		q = q.Where("ip = ?", ip)
	} else {
		return 0, nil
	}
	err := q.Count(&n).Error
	return int(n), err
}

func (r *userRepo) CreateSession(ctx context.Context, s *domain.UserSession) error {
	return r.db().WithContext(ctx).Table(userSessionsTbl(ctx)).Create(s).Error
}

func (r *userRepo) GetSession(ctx context.Context, token string) (*domain.UserSession, error) {
	var s domain.UserSession
	err := r.dbRO().WithContext(ctx).Table(userSessionsTbl(ctx)).Where("token = ?", token).Take(&s).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrSessionNotFound
	}
	return &s, err
}

func (r *userRepo) DeleteSession(ctx context.Context, token string) error {
	return r.db().WithContext(ctx).Table(userSessionsTbl(ctx)).
		Where("token = ?", token).Delete(&domain.UserSession{}).Error
}

func (r *userRepo) DeleteAllUserSessions(ctx context.Context, userID int64) error {
	return r.db().WithContext(ctx).Table(userSessionsTbl(ctx)).
		Where("user_id = ?", userID).Delete(&domain.UserSession{}).Error
}

func (r *userRepo) AddUserAccount(ctx context.Context, a *domain.UserAccount) error {
	return r.db().WithContext(ctx).Table(userAccountsTbl(ctx)).Create(a).Error
}

func (r *userRepo) ListUserAccounts(ctx context.Context, userID int64) ([]*domain.UserAccount, error) {
	var out []*domain.UserAccount
	err := r.dbRO().WithContext(ctx).Table(userAccountsTbl(ctx)).Where("user_id = ?", userID).Find(&out).Error
	return out, err
}

func (r *userRepo) GetUserAccount(ctx context.Context, userID int64, currency string) (*domain.UserAccount, error) {
	var a domain.UserAccount
	err := r.dbRO().WithContext(ctx).Table(userAccountsTbl(ctx)).
		Where("user_id = ? AND currency = ?", userID, currency).Take(&a).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &a, err
}

func (r *userRepo) UpsertSettings(ctx context.Context, userID int64, settingsJSON string) error {
	s := &domain.UserSettings{UserID: userID, Settings: settingsJSON}
	return r.db().WithContext(ctx).Table(userSettingsTbl(ctx)).Save(s).Error
}

func (r *userRepo) GetSettings(ctx context.Context, userID int64) (string, error) {
	var s domain.UserSettings
	err := r.dbRO().WithContext(ctx).Table(userSettingsTbl(ctx)).Where("user_id = ?", userID).Take(&s).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil
	}
	return s.Settings, err
}

func (r *userRepo) AssignRole(ctx context.Context, userID, roleID int64) error {
	return r.db().WithContext(ctx).Table(userRolesTbl(ctx)).
		Create(&domain.UserRole{UserID: userID, RoleID: roleID}).Error
}

func (r *userRepo) RemoveRole(ctx context.Context, userID, roleID int64) error {
	return r.db().WithContext(ctx).Table(userRolesTbl(ctx)).
		Where("user_id = ? AND role_id = ?", userID, roleID).
		Delete(&domain.UserRole{}).Error
}

// ListRoles / ListPermissions / HasPermission 用 Joins 跨表，shadow 路由要求
// 所有参与表都跟随 ctx；用 SQL alias（"AS roles" 等）保留旧字段引用，
// 这样 WHERE / ON 子句不需要重写。
func (r *userRepo) ListRoles(ctx context.Context, userID int64) ([]*domain.Role, error) {
	var roles []*domain.Role
	err := r.dbRO().WithContext(ctx).
		Table(rolesTbl(ctx) + " AS roles").
		Joins("INNER JOIN " + userRolesTbl(ctx) + " AS user_roles ON user_roles.role_id = roles.id").
		Where("user_roles.user_id = ?", userID).
		Find(&roles).Error
	return roles, err
}

func (r *userRepo) ListPermissions(ctx context.Context, userID int64) ([]*domain.Permission, error) {
	var perms []*domain.Permission
	err := r.dbRO().WithContext(ctx).
		Table(permissionsTbl(ctx) + " AS permissions").
		Joins("INNER JOIN " + rolePermissionsTbl(ctx) + " AS role_permissions ON role_permissions.permission_id = permissions.id").
		Joins("INNER JOIN " + userRolesTbl(ctx) + " AS user_roles ON user_roles.role_id = role_permissions.role_id").
		Where("user_roles.user_id = ?", userID).
		Distinct("permissions.id, permissions.name, permissions.code").
		Find(&perms).Error
	return perms, err
}

func (r *userRepo) FindRoleByName(ctx context.Context, name string) (*domain.Role, error) {
	var role domain.Role
	err := r.dbRO().WithContext(ctx).Table(rolesTbl(ctx)).Where("name = ?", name).Take(&role).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &role, err
}

func (r *userRepo) HasPermission(ctx context.Context, userID int64, code string) (bool, error) {
	var n int64
	err := r.dbRO().WithContext(ctx).
		Table(permissionsTbl(ctx) + " AS permissions").
		Joins("INNER JOIN " + rolePermissionsTbl(ctx) + " AS role_permissions ON role_permissions.permission_id = permissions.id").
		Joins("INNER JOIN " + userRolesTbl(ctx) + " AS user_roles ON user_roles.role_id = role_permissions.role_id").
		Where("user_roles.user_id = ? AND permissions.code = ?", userID, code).
		Count(&n).Error
	return n > 0, err
}

func (r *userRepo) CreateEmailCode(ctx context.Context, c *domain.EmailCode) error {
	return r.db().WithContext(ctx).Table(emailCodesTbl(ctx)).Create(c).Error
}

func (r *userRepo) FindLatestActiveCode(ctx context.Context, identifier, purpose string) (*domain.EmailCode, error) {
	var c domain.EmailCode
	err := r.dbRO().WithContext(ctx).Table(emailCodesTbl(ctx)).
		Where("identifier = ? AND purpose = ? AND used_at IS NULL AND expires_at > ?",
			identifier, purpose, time.Now()).
		Order("id DESC").Take(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrEmailCodeExpired
	}
	return &c, err
}

func (r *userRepo) IncrCodeAttempts(ctx context.Context, codeID int64) error {
	return r.db().WithContext(ctx).Table(emailCodesTbl(ctx)).
		Where("id = ?", codeID).
		UpdateColumn("attempts", gorm.Expr("attempts + 1")).Error
}

// MarkCodeUsed 资金 / 安全修复：CAS 消费 OTP，仅当 used_at IS NULL 时才标记。
// RowsAffected==0 → 该 code 已被并发请求消费过 → 返 ErrEmailCodeUsed 让
// 调用方拒绝本次操作。
//
// 攻击场景：OTP 泄漏（嗅探、剪贴板、社工）后，攻击者跟合法用户同时
// VerifyCode → 各自完成各自的操作（双重 reset password / 抢绑手机等）→
// 账号接管。CAS 后只有第一个赢 transition 的请求继续，第二个直接拒。
func (r *userRepo) MarkCodeUsed(ctx context.Context, codeID int64) error {
	now := time.Now()
	res := r.db().WithContext(ctx).Table(emailCodesTbl(ctx)).
		Where("id = ? AND used_at IS NULL", codeID).
		Update("used_at", &now)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return domain.ErrEmailCodeUsed
	}
	return nil
}

// ── helpers ──────────────────────────────────────────────────────────

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if indexOfSub(s, sub) >= 0 {
			return true
		}
	}
	return false
}

func indexOfSub(s, sub string) int {
	if len(sub) == 0 || len(s) < len(sub) {
		return -1
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
