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

// ─── 表名常量 ────────────────────────────────────────────────────────────────
//
// 分片表（按 user_id 路由到 user_merchant_db_0..9 的 base_NN(_shadow)）：
//
//	users / user_profiles / user_auths / login_logs / user_sessions /
//	user_roles / user_accounts / user_settings
//
// meta-only（不分片，跨 user / merchant 全局）：
//
//	roles / permissions / role_permissions / email_codes
//
// 反查二级索引（meta，避免跨 100 张分片表 fanout）：
//
//	user_lookup     (lookup_type, lookup_value) → user_id
//	auth_lookup     (auth_type, identifier)     → user_id
//	session_lookup  (token)                     → user_id
const (
	// 分片
	tblUsers        = "users"
	tblUserProfiles = "user_profiles"
	tblUserAuths    = "user_auths"
	tblLoginLogs    = "login_logs"
	tblUserSessions = "user_sessions"
	tblUserRoles    = "user_roles"
	tblUserAccounts = "user_accounts"
	tblUserSettings = "user_settings"

	// meta（字典 / 验证码）
	tblRoles           = "roles"
	tblPermissions     = "permissions"
	tblRolePermissions = "role_permissions"
	tblEmailCodes      = "email_codes"

	// meta（反查索引）
	tblUserLookup    = "user_lookup"
	tblAuthLookup    = "auth_lookup"
	tblSessionLookup = "session_lookup"
)

// UserRepository 用户身份 + 多渠道认证 + 会话 + 权限 + 钱包 + 设置 + 验证码
// 全部 CRUD。所有按 user_id 路由的写入都自动落对应 shard；反查（by email /
// username / phone / token / auth identifier）通过 meta 二级索引拿到 user_id
// 后再路由。
//
// shadow 流量由 ctx 透传：分片表名 router.TableName 内置 shadow.TableName 包装，
// 2meta 表用 r.metaTbl(ctx, base) 同步加 _shadow 后缀。
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

type userRepo struct {
	mgr    *Manager
	router *ShardRouter
}

// NewUserRepository 构造。router 为 nil（dev / 单元测试）时退化到单 meta DB
// + 不带 _NN 后缀的表名。生产路径：传入 sharding.NewRouter()。
func NewUserRepository(mgr *Manager, router *ShardRouter) UserRepository {
	return &userRepo{mgr: mgr, router: router}
}

// shard 路由 helper：给定 (base, userID) 返回 (db 连接, 完整表名)。
func (r *userRepo) shard(ctx context.Context, base string, userID int64) (*gorm.DB, string) {
	return shardForUser(ctx, r.mgr, r.router, base, userID)
}

// metaTbl meta 上的表名（带 shadow 后缀）。lookup / RBAC 字典 / email_codes 用。
func (r *userRepo) metaTbl(ctx context.Context, base string) string {
	return shadow.TableName(ctx, base)
}

func (r *userRepo) meta() *gorm.DB   { return r.mgr.GetMeta() }
func (r *userRepo) metaRO() *gorm.DB { return r.mgr.GetMetaRO() }

// ─── user_lookup（meta 反查索引）─────────────────────────────────────────────

// reserveUserLookup 占位 user_lookup 一行；INSERT 失败 + dup → 返 errOnDup
// （业务语义错），其它错原样返回。给 CreateUser / UpdateUser 兜跨 shard 的
// username/email/phone 唯一约束。
func (r *userRepo) reserveUserLookup(ctx context.Context, lookupType, lookupValue string, userID int64, errOnDup error) error {
	if lookupValue == "" {
		return nil
	}
	err := r.meta().WithContext(ctx).Exec(
		"INSERT INTO "+r.metaTbl(ctx, tblUserLookup)+
			" (lookup_type, lookup_value, user_id) VALUES (?, ?, ?)",
		lookupType, lookupValue, userID,
	).Error
	if err != nil && isDupKey(err) {
		return errOnDup
	}
	return err
}

func (r *userRepo) deleteUserLookup(ctx context.Context, lookupType, lookupValue string) {
	if lookupValue == "" {
		return
	}
	_ = r.meta().WithContext(ctx).Exec(
		"DELETE FROM "+r.metaTbl(ctx, tblUserLookup)+
			" WHERE lookup_type = ? AND lookup_value = ?",
		lookupType, lookupValue,
	).Error
}

// resolveUserLookup meta 查 (lookup_type, lookup_value) → user_id。
// 缺失 / 错误 → (0, false)。
func (r *userRepo) resolveUserLookup(ctx context.Context, lookupType, lookupValue string) (int64, bool) {
	type row struct {
		UserID int64 `gorm:"column:user_id"`
	}
	var out row
	err := r.metaRO().WithContext(ctx).Table(r.metaTbl(ctx, tblUserLookup)).
		Where("lookup_type = ? AND lookup_value = ?", lookupType, lookupValue).
		Take(&out).Error
	if err != nil {
		return 0, false
	}
	return out.UserID, true
}

// ─── User CRUD ──────────────────────────────────────────────────────────────

func (r *userRepo) CreateUser(ctx context.Context, u *domain.User) error {
	if u.PasswordHash == "" {
		return fmt.Errorf("%w: password_hash required", domain.ErrValidation)
	}
	if u.Status == 0 {
		u.Status = domain.UserStatusActive
	}
	// 跨分片唯一约束：先在 meta user_lookup 占坑，hit dup → 返业务错。
	type rsv struct {
		kind, val string
		errMap    error
	}
	var pending []rsv
	if u.Username != nil && *u.Username != "" {
		pending = append(pending, rsv{"username", *u.Username, domain.ErrUsernameExists})
	}
	if u.Email != nil && *u.Email != "" {
		pending = append(pending, rsv{"email", *u.Email, domain.ErrEmailExists})
	}
	if u.Phone != nil && *u.Phone != "" {
		pending = append(pending, rsv{"phone", *u.Phone, domain.ErrPhoneExists})
	}
	var reserved []rsv
	rollback := func() {
		for _, k := range reserved {
			r.deleteUserLookup(ctx, k.kind, k.val)
		}
	}
	for _, k := range pending {
		if err := r.reserveUserLookup(ctx, k.kind, k.val, u.ID, k.errMap); err != nil {
			rollback()
			return err
		}
		reserved = append(reserved, k)
	}
	// 写 user 行到对应 shard。
	db, tbl := r.shard(ctx, tblUsers, u.ID)
	if err := db.WithContext(ctx).Table(tbl).Create(u).Error; err != nil {
		rollback()
		if isDupKey(err) {
			// shard 内 PK 冲突（id 复用）—— 罕见，理论上 leaf_alloc 不会发；
			// 兜底当 username 冲突处理（让 service 层返 4xx 即可）。
			return domain.ErrUsernameExists
		}
		return err
	}
	return nil
}

func (r *userRepo) GetUser(ctx context.Context, userID int64) (*domain.User, error) {
	db, tbl := r.shard(ctx, tblUsers, userID)
	var u domain.User
	err := db.WithContext(ctx).Table(tbl).Where("id = ?", userID).First(&u).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrUserNotFound
	}
	return &u, err
}

func (r *userRepo) GetUserByUsername(ctx context.Context, username string) (*domain.User, error) {
	uid, ok := r.resolveUserLookup(ctx, "username", username)
	if !ok {
		return nil, domain.ErrUserNotFound
	}
	return r.GetUser(ctx, uid)
}

func (r *userRepo) GetUserByEmail(ctx context.Context, email string) (*domain.User, error) {
	uid, ok := r.resolveUserLookup(ctx, "email", email)
	if !ok {
		return nil, domain.ErrUserNotFound
	}
	return r.GetUser(ctx, uid)
}

func (r *userRepo) GetUserByPhone(ctx context.Context, phone string) (*domain.User, error) {
	uid, ok := r.resolveUserLookup(ctx, "phone", phone)
	if !ok {
		return nil, domain.ErrUserNotFound
	}
	return r.GetUser(ctx, uid)
}

// UpdateUser Save 全字段。username/email/phone 改了 → 同步刷 user_lookup
// （reserve 新值、delete 旧值）；reserve 撞 dup 直接返业务错。
func (r *userRepo) UpdateUser(ctx context.Context, u *domain.User) error {
	old, err := r.GetUser(ctx, u.ID)
	if err != nil {
		return err
	}
	type rsv struct {
		kind             string
		oldVal, newVal   string
		errMap           error
	}
	var pending []rsv
	addIfChanged := func(kind, oldV, newV string, errMap error) {
		if newV == oldV {
			return
		}
		pending = append(pending, rsv{kind: kind, oldVal: oldV, newVal: newV, errMap: errMap})
	}
	addIfChanged("username", ptrStr(old.Username), ptrStr(u.Username), domain.ErrUsernameExists)
	addIfChanged("email", ptrStr(old.Email), ptrStr(u.Email), domain.ErrEmailExists)
	addIfChanged("phone", ptrStr(old.Phone), ptrStr(u.Phone), domain.ErrPhoneExists)

	// reserve 新值（按顺序，失败时回滚已 reserve 的）
	var reserved []rsv
	rollback := func() {
		for _, k := range reserved {
			r.deleteUserLookup(ctx, k.kind, k.newVal)
		}
	}
	for _, k := range pending {
		if k.newVal == "" {
			// 改为 NULL：跳过 reserve
			continue
		}
		if err := r.reserveUserLookup(ctx, k.kind, k.newVal, u.ID, k.errMap); err != nil {
			rollback()
			return err
		}
		reserved = append(reserved, k)
	}

	// shard save
	db, tbl := r.shard(ctx, tblUsers, u.ID)
	if err := db.WithContext(ctx).Table(tbl).Save(u).Error; err != nil {
		rollback()
		return err
	}

	// 删旧值（best-effort）
	for _, k := range pending {
		if k.oldVal != "" {
			r.deleteUserLookup(ctx, k.kind, k.oldVal)
		}
	}
	return nil
}

func (r *userRepo) UpdateLastLogin(ctx context.Context, userID int64, t time.Time) error {
	db, tbl := r.shard(ctx, tblUsers, userID)
	return db.WithContext(ctx).Table(tbl).
		Where("id = ?", userID).
		Updates(map[string]any{"last_login_at": t, "failed_login_count": 0, "locked_until": nil}).Error
}

func (r *userRepo) IncrFailedLogin(ctx context.Context, userID int64) (int, error) {
	db, tbl := r.shard(ctx, tblUsers, userID)
	tx := db.WithContext(ctx).Begin()
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
	db, tbl := r.shard(ctx, tblUsers, userID)
	return db.WithContext(ctx).Table(tbl).
		Where("id = ?", userID).
		Updates(map[string]any{"failed_login_count": 0, "locked_until": nil}).Error
}

func (r *userRepo) LockUser(ctx context.Context, userID int64, until time.Time) error {
	db, tbl := r.shard(ctx, tblUsers, userID)
	return db.WithContext(ctx).Table(tbl).
		Where("id = ?", userID).
		Updates(map[string]any{"status": domain.UserStatusLocked, "locked_until": until}).Error
}

// SoftDelete 软删 + 清空 PII + 删 lookup（让 GetByEmail/GetByUsername 立即返
// NotFound）。若 user 行不存在直接返；lookup 删除是 best-effort。
func (r *userRepo) SoftDelete(ctx context.Context, userID int64, _ string) error {
	old, err := r.GetUser(ctx, userID)
	if err != nil && !errors.Is(err, domain.ErrUserNotFound) {
		return err
	}
	db, tbl := r.shard(ctx, tblUsers, userID)
	if err := db.WithContext(ctx).Table(tbl).
		Where("id = ?", userID).
		Updates(map[string]any{
			"status":        domain.UserStatusDeleted,
			"username":      nil,
			"email":         nil,
			"phone":         nil,
			"password_hash": "",
			"totp_secret":   "",
		}).Error; err != nil {
		return err
	}
	if old != nil {
		r.deleteUserLookup(ctx, "username", ptrStr(old.Username))
		r.deleteUserLookup(ctx, "email", ptrStr(old.Email))
		r.deleteUserLookup(ctx, "phone", ptrStr(old.Phone))
	}
	return nil
}

// ─── Profile ────────────────────────────────────────────────────────────────

func (r *userRepo) UpsertProfile(ctx context.Context, p *domain.UserProfile) error {
	db, tbl := r.shard(ctx, tblUserProfiles, p.UserID)
	return db.WithContext(ctx).Table(tbl).Save(p).Error
}

func (r *userRepo) GetProfile(ctx context.Context, userID int64) (*domain.UserProfile, error) {
	db, tbl := r.shard(ctx, tblUserProfiles, userID)
	var p domain.UserProfile
	err := db.WithContext(ctx).Table(tbl).Where("user_id = ?", userID).Take(&p).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &p, err
}

// ─── Auth（按 user_id 分片）+ auth_lookup（meta）────────────────────────────

// reserveAuthLookup 占位 auth_lookup（跨 shard 唯一）；isDupKey → ErrAuthExists。
func (r *userRepo) reserveAuthLookup(ctx context.Context, authType, identifier string, userID int64) error {
	err := r.meta().WithContext(ctx).Exec(
		"INSERT INTO "+r.metaTbl(ctx, tblAuthLookup)+
			" (auth_type, identifier, user_id) VALUES (?, ?, ?)",
		authType, identifier, userID,
	).Error
	if err != nil && isDupKey(err) {
		return domain.ErrAuthExists
	}
	return err
}

func (r *userRepo) deleteAuthLookup(ctx context.Context, authType, identifier string) {
	_ = r.meta().WithContext(ctx).Exec(
		"DELETE FROM "+r.metaTbl(ctx, tblAuthLookup)+
			" WHERE auth_type = ? AND identifier = ?",
		authType, identifier,
	).Error
}

func (r *userRepo) resolveAuthLookup(ctx context.Context, authType, identifier string) (int64, bool) {
	type row struct {
		UserID int64 `gorm:"column:user_id"`
	}
	var out row
	err := r.metaRO().WithContext(ctx).Table(r.metaTbl(ctx, tblAuthLookup)).
		Where("auth_type = ? AND identifier = ?", authType, identifier).
		Take(&out).Error
	if err != nil {
		return 0, false
	}
	return out.UserID, true
}

func (r *userRepo) AddAuth(ctx context.Context, a *domain.UserAuth) error {
	if a.UserID == 0 || a.AuthType == "" || a.Identifier == "" {
		return fmt.Errorf("%w: user_id/auth_type/identifier required", domain.ErrValidation)
	}
	// 先 meta reserve（跨 shard 唯一），再 shard insert。
	if err := r.reserveAuthLookup(ctx, a.AuthType, a.Identifier, a.UserID); err != nil {
		return err
	}
	db, tbl := r.shard(ctx, tblUserAuths, a.UserID)
	if err := db.WithContext(ctx).Table(tbl).Create(a).Error; err != nil {
		// 回滚 reserve
		r.deleteAuthLookup(ctx, a.AuthType, a.Identifier)
		if isDupKey(err) {
			return domain.ErrAuthExists
		}
		return err
	}
	return nil
}

func (r *userRepo) FindAuth(ctx context.Context, authType, identifier string) (*domain.UserAuth, error) {
	uid, ok := r.resolveAuthLookup(ctx, authType, identifier)
	if !ok {
		return nil, domain.ErrAuthNotFound
	}
	db, tbl := r.shard(ctx, tblUserAuths, uid)
	var a domain.UserAuth
	err := db.WithContext(ctx).Table(tbl).
		Where("user_id = ? AND auth_type = ? AND identifier = ?", uid, authType, identifier).
		Take(&a).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrAuthNotFound
	}
	return &a, err
}

func (r *userRepo) ListAuths(ctx context.Context, userID int64) ([]*domain.UserAuth, error) {
	db, tbl := r.shard(ctx, tblUserAuths, userID)
	var out []*domain.UserAuth
	err := db.WithContext(ctx).Table(tbl).Where("user_id = ?", userID).Find(&out).Error
	return out, err
}

func (r *userRepo) RemoveAuth(ctx context.Context, userID int64, authType, identifier string) error {
	db, tbl := r.shard(ctx, tblUserAuths, userID)
	if err := db.WithContext(ctx).Table(tbl).
		Where("user_id = ? AND auth_type = ? AND identifier = ?", userID, authType, identifier).
		Delete(&domain.UserAuth{}).Error; err != nil {
		return err
	}
	r.deleteAuthLookup(ctx, authType, identifier)
	return nil
}

// MarkAuthVerified meta lookup → shard update。
func (r *userRepo) MarkAuthVerified(ctx context.Context, authType, identifier string) error {
	uid, ok := r.resolveAuthLookup(ctx, authType, identifier)
	if !ok {
		return domain.ErrAuthNotFound
	}
	db, tbl := r.shard(ctx, tblUserAuths, uid)
	return db.WithContext(ctx).Table(tbl).
		Where("user_id = ? AND auth_type = ? AND identifier = ?", uid, authType, identifier).
		Update("verified", true).Error
}

// ─── Login log（按 user_id 分片）─────────────────────────────────────────────

// InsertLoginLog l.UserID 为 nil（unknown user 失败登录）时落 shard 0 当
// "anonymous shard"。生产里这种 row 量很小，限于 shard 0 不影响其他 shard。
func (r *userRepo) InsertLoginLog(ctx context.Context, l *domain.LoginLog) error {
	var routeID int64
	if l.UserID != nil {
		routeID = *l.UserID
	}
	db, tbl := r.shard(ctx, tblLoginLogs, routeID)
	return db.WithContext(ctx).Table(tbl).Create(l).Error
}

// CountRecentFailedLogins 风控：查最近一段时间内的失败登录次数。
//
//   - userID > 0 && ip != ""  → 单 shard：(user_id = ? OR ip = ?) AND status = 0
//     注意：跨 shard 的 IP 撞库不会被这条 SQL 看到（IP 命中其他 user 的
//     失败被记到那些 user 自己的 shard）。能接受这点近似的换"账户级降级"
//     即可。完全准确的"全局 IP 撞库"统计需要 fanout 100 shards。
//   - userID > 0 only         → 单 shard 查 user_id
//   - ip != "" only           → fanout 100 shards 求和（unknown user 路径）
//   - 都为空                  → 0
func (r *userRepo) CountRecentFailedLogins(ctx context.Context, userID int64, ip string, since time.Time) (int, error) {
	if userID == 0 && ip == "" {
		return 0, nil
	}
	if userID > 0 {
		db, tbl := r.shard(ctx, tblLoginLogs, userID)
		q := db.WithContext(ctx).Table(tbl).Where("status = 0 AND created_at >= ?", since)
		if ip != "" {
			q = q.Where("user_id = ? OR ip = ?", userID, ip)
		} else {
			q = q.Where("user_id = ?", userID)
		}
		var n int64
		err := q.Count(&n).Error
		return int(n), err
	}
	// userID == 0 && ip != "" → fanout
	var total int64
	for _, s := range allShardsForFanout(ctx, r.mgr, r.router, tblLoginLogs) {
		var n int64
		if err := s.DB.WithContext(ctx).Table(s.Table).
			Where("status = 0 AND ip = ? AND created_at >= ?", ip, since).
			Count(&n).Error; err != nil {
			return int(total), err
		}
		total += n
	}
	return int(total), nil
}

// ─── Session（按 user_id 分片）+ session_lookup（meta）──────────────────────

func (r *userRepo) reserveSessionLookup(ctx context.Context, token string, userID int64, expiresAt time.Time) error {
	return r.meta().WithContext(ctx).Exec(
		"INSERT INTO "+r.metaTbl(ctx, tblSessionLookup)+
			" (token, user_id, expires_at) VALUES (?, ?, ?)",
		token, userID, expiresAt,
	).Error
}

func (r *userRepo) deleteSessionLookup(ctx context.Context, token string) {
	_ = r.meta().WithContext(ctx).Exec(
		"DELETE FROM "+r.metaTbl(ctx, tblSessionLookup)+" WHERE token = ?",
		token,
	).Error
}

func (r *userRepo) resolveSessionLookup(ctx context.Context, token string) (int64, bool) {
	type row struct {
		UserID int64 `gorm:"column:user_id"`
	}
	var out row
	err := r.metaRO().WithContext(ctx).Table(r.metaTbl(ctx, tblSessionLookup)).
		Where("token = ?", token).Take(&out).Error
	if err != nil {
		return 0, false
	}
	return out.UserID, true
}

func (r *userRepo) CreateSession(ctx context.Context, s *domain.UserSession) error {
	if err := r.reserveSessionLookup(ctx, s.Token, s.UserID, s.ExpiresAt); err != nil {
		// token 撞 dup 极罕见（token 一般 256-bit random）；当作内部错继续上抛。
		return err
	}
	db, tbl := r.shard(ctx, tblUserSessions, s.UserID)
	if err := db.WithContext(ctx).Table(tbl).Create(s).Error; err != nil {
		r.deleteSessionLookup(ctx, s.Token)
		return err
	}
	return nil
}

func (r *userRepo) GetSession(ctx context.Context, token string) (*domain.UserSession, error) {
	uid, ok := r.resolveSessionLookup(ctx, token)
	if !ok {
		return nil, domain.ErrSessionNotFound
	}
	db, tbl := r.shard(ctx, tblUserSessions, uid)
	var s domain.UserSession
	err := db.WithContext(ctx).Table(tbl).Where("token = ?", token).Take(&s).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrSessionNotFound
	}
	return &s, err
}

func (r *userRepo) DeleteSession(ctx context.Context, token string) error {
	uid, ok := r.resolveSessionLookup(ctx, token)
	if !ok {
		// 没有 lookup 也清掉同名 lookup 当兜底，再返。
		r.deleteSessionLookup(ctx, token)
		return nil
	}
	db, tbl := r.shard(ctx, tblUserSessions, uid)
	if err := db.WithContext(ctx).Table(tbl).
		Where("token = ?", token).Delete(&domain.UserSession{}).Error; err != nil {
		return err
	}
	r.deleteSessionLookup(ctx, token)
	return nil
}

// DeleteAllUserSessions 删 user 全部 session：先扫该 user shard 拿 token 列表，
// 删 shard 行 + 同步 nuke session_lookup。
func (r *userRepo) DeleteAllUserSessions(ctx context.Context, userID int64) error {
	db, tbl := r.shard(ctx, tblUserSessions, userID)
	var tokens []string
	if err := db.WithContext(ctx).Table(tbl).
		Where("user_id = ?", userID).Pluck("token", &tokens).Error; err != nil {
		return err
	}
	if err := db.WithContext(ctx).Table(tbl).
		Where("user_id = ?", userID).Delete(&domain.UserSession{}).Error; err != nil {
		return err
	}
	for _, tk := range tokens {
		r.deleteSessionLookup(ctx, tk)
	}
	return nil
}

// ─── Wallet（user_accounts，按 user_id 分片）────────────────────────────────

func (r *userRepo) AddUserAccount(ctx context.Context, a *domain.UserAccount) error {
	db, tbl := r.shard(ctx, tblUserAccounts, a.UserID)
	return db.WithContext(ctx).Table(tbl).Create(a).Error
}

func (r *userRepo) ListUserAccounts(ctx context.Context, userID int64) ([]*domain.UserAccount, error) {
	db, tbl := r.shard(ctx, tblUserAccounts, userID)
	var out []*domain.UserAccount
	err := db.WithContext(ctx).Table(tbl).Where("user_id = ?", userID).Find(&out).Error
	return out, err
}

func (r *userRepo) GetUserAccount(ctx context.Context, userID int64, currency string) (*domain.UserAccount, error) {
	db, tbl := r.shard(ctx, tblUserAccounts, userID)
	var a domain.UserAccount
	err := db.WithContext(ctx).Table(tbl).
		Where("user_id = ? AND currency = ?", userID, currency).Take(&a).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &a, err
}

// ─── Settings（按 user_id 分片）─────────────────────────────────────────────

func (r *userRepo) UpsertSettings(ctx context.Context, userID int64, settingsJSON string) error {
	s := &domain.UserSettings{UserID: userID, Settings: settingsJSON}
	db, tbl := r.shard(ctx, tblUserSettings, userID)
	return db.WithContext(ctx).Table(tbl).Save(s).Error
}

func (r *userRepo) GetSettings(ctx context.Context, userID int64) (string, error) {
	db, tbl := r.shard(ctx, tblUserSettings, userID)
	var s domain.UserSettings
	err := db.WithContext(ctx).Table(tbl).Where("user_id = ?", userID).Take(&s).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil
	}
	return s.Settings, err
}

// ─── RBAC ───────────────────────────────────────────────────────────────────
//
// user_roles 按 user_id 分片；roles / permissions / role_permissions 留 meta。
// 跨 DB 的 join 拆成两步：先在 shard 拿 role_id 集合，再去 meta 查字典。

func (r *userRepo) AssignRole(ctx context.Context, userID, roleID int64) error {
	db, tbl := r.shard(ctx, tblUserRoles, userID)
	return db.WithContext(ctx).Table(tbl).
		Create(&domain.UserRole{UserID: userID, RoleID: roleID}).Error
}

func (r *userRepo) RemoveRole(ctx context.Context, userID, roleID int64) error {
	db, tbl := r.shard(ctx, tblUserRoles, userID)
	return db.WithContext(ctx).Table(tbl).
		Where("user_id = ? AND role_id = ?", userID, roleID).
		Delete(&domain.UserRole{}).Error
}

// ListRoles shard 取 role_id 集合 → meta 查 roles 详情。
func (r *userRepo) ListRoles(ctx context.Context, userID int64) ([]*domain.Role, error) {
	db, tbl := r.shard(ctx, tblUserRoles, userID)
	var roleIDs []int64
	if err := db.WithContext(ctx).Table(tbl).
		Where("user_id = ?", userID).Pluck("role_id", &roleIDs).Error; err != nil {
		return nil, err
	}
	if len(roleIDs) == 0 {
		return nil, nil
	}
	var roles []*domain.Role
	err := r.metaRO().WithContext(ctx).Table(r.metaTbl(ctx, tblRoles)).
		Where("id IN ?", roleIDs).Find(&roles).Error
	return roles, err
}

// ListPermissions shard 取 user 的 role_id → meta 查 permissions JOIN role_permissions。
func (r *userRepo) ListPermissions(ctx context.Context, userID int64) ([]*domain.Permission, error) {
	db, tbl := r.shard(ctx, tblUserRoles, userID)
	var roleIDs []int64
	if err := db.WithContext(ctx).Table(tbl).
		Where("user_id = ?", userID).Pluck("role_id", &roleIDs).Error; err != nil {
		return nil, err
	}
	if len(roleIDs) == 0 {
		return nil, nil
	}
	var perms []*domain.Permission
	err := r.metaRO().WithContext(ctx).
		Table(r.metaTbl(ctx, tblPermissions)+" AS permissions").
		Joins("INNER JOIN "+r.metaTbl(ctx, tblRolePermissions)+" AS role_permissions ON role_permissions.permission_id = permissions.id").
		Where("role_permissions.role_id IN ?", roleIDs).
		Distinct("permissions.id, permissions.name, permissions.code").
		Find(&perms).Error
	return perms, err
}

// HasPermission 1) meta 查 code 对应的 role_ids；2) shard 看 user 是否有任意一个。
func (r *userRepo) HasPermission(ctx context.Context, userID int64, code string) (bool, error) {
	var roleIDs []int64
	err := r.metaRO().WithContext(ctx).
		Table(r.metaTbl(ctx, tblRolePermissions)+" AS role_permissions").
		Joins("INNER JOIN "+r.metaTbl(ctx, tblPermissions)+" AS permissions ON permissions.id = role_permissions.permission_id").
		Where("permissions.code = ?", code).
		Pluck("role_permissions.role_id", &roleIDs).Error
	if err != nil {
		return false, err
	}
	if len(roleIDs) == 0 {
		return false, nil
	}
	db, tbl := r.shard(ctx, tblUserRoles, userID)
	var n int64
	if err := db.WithContext(ctx).Table(tbl).
		Where("user_id = ? AND role_id IN ?", userID, roleIDs).
		Count(&n).Error; err != nil {
		return false, err
	}
	return n > 0, nil
}

func (r *userRepo) FindRoleByName(ctx context.Context, name string) (*domain.Role, error) {
	var role domain.Role
	err := r.metaRO().WithContext(ctx).Table(r.metaTbl(ctx, tblRoles)).
		Where("name = ?", name).Take(&role).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &role, err
}

// ─── Email codes（meta，by identifier）──────────────────────────────────────

func (r *userRepo) CreateEmailCode(ctx context.Context, c *domain.EmailCode) error {
	return r.meta().WithContext(ctx).Table(r.metaTbl(ctx, tblEmailCodes)).Create(c).Error
}

func (r *userRepo) FindLatestActiveCode(ctx context.Context, identifier, purpose string) (*domain.EmailCode, error) {
	var c domain.EmailCode
	err := r.metaRO().WithContext(ctx).Table(r.metaTbl(ctx, tblEmailCodes)).
		Where("identifier = ? AND purpose = ? AND used_at IS NULL AND expires_at > ?",
			identifier, purpose, time.Now()).
		Order("id DESC").Take(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrEmailCodeExpired
	}
	return &c, err
}

func (r *userRepo) IncrCodeAttempts(ctx context.Context, codeID int64) error {
	return r.meta().WithContext(ctx).Table(r.metaTbl(ctx, tblEmailCodes)).
		Where("id = ?", codeID).
		UpdateColumn("attempts", gorm.Expr("attempts + 1")).Error
}

// MarkCodeUsed CAS 消费 OTP，仅当 used_at IS NULL 时才标记。
// RowsAffected==0 → 该 code 已被并发请求消费过 → 返 ErrEmailCodeUsed。
//
// 攻击场景：OTP 泄漏（嗅探、剪贴板、社工）后，攻击者跟合法用户同时
// VerifyCode → 各自完成各自的操作（双重 reset password / 抢绑手机等）→
// 账号接管。CAS 后只有第一个赢 transition 的请求继续，第二个直接拒。
func (r *userRepo) MarkCodeUsed(ctx context.Context, codeID int64) error {
	now := time.Now()
	res := r.meta().WithContext(ctx).Table(r.metaTbl(ctx, tblEmailCodes)).
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

// ─── helpers ────────────────────────────────────────────────────────────────

// ptrStr nil-safe *string → string 解包。
func ptrStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
