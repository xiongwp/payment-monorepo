package domain

import (
	"errors"
	"time"
)

// 完整 C 端用户身份模型，对齐 11 张表设计：
//
//   users           主表（username / email / phone / password_hash / status）
//   user_profiles   资料（昵称 / 头像 / 性别 / 生日 / bio）
//   user_auths      多渠道认证（password / google / wechat / apple / phone）
//   login_logs      登录日志（成功 / 失败 + IP + UA）
//   user_sessions   会话（JWT token + 过期时间；登出 / 风控冻结即时撤销用）
//   roles           角色
//   user_roles      用户-角色多对多
//   permissions     权限
//   role_permissions角色-权限多对多
//   user_wallets    钱包（minor unit int64 不用 DECIMAL）
//   user_settings   用户设置（JSON）

// ─── 错误 ───────────────────────────────────────────────────────────────────

var (
	ErrUserNotFound       = errors.New("user not found")
	ErrUsernameExists     = errors.New("username already exists")
	ErrEmailExists        = errors.New("email already exists")
	ErrPhoneExists        = errors.New("phone already exists")
	ErrAuthExists         = errors.New("auth identity already bound")
	ErrPasswordWrong      = errors.New("password wrong")
	ErrUserDisabled       = errors.New("user disabled")
	ErrUserLocked         = errors.New("user locked")
	ErrEmailNotVerified   = errors.New("email not verified")
	ErrOTPRequired        = errors.New("otp required")
	ErrOTPWrong           = errors.New("otp code wrong")
	ErrEmailCodeExpired   = errors.New("email verification code expired or invalid")
	// ErrEmailCodeUsed OTP 已被消费过，再次 VerifyCode 应拒绝（防 race / 重放）。
	ErrEmailCodeUsed      = errors.New("email verification code already used")
	ErrAuthNotFound       = errors.New("auth identity not found")
	ErrPermissionDenied   = errors.New("permission denied")
	ErrSessionNotFound    = errors.New("session not found")
)

// ─── status enum（users.status TINYINT）─────────────────────────────────────

const (
	UserStatusActive   int8 = 1
	UserStatusDisabled int8 = 0
	UserStatusLocked   int8 = 2 // 风控冻结（撞库 / fraud）
	UserStatusDeleted  int8 = 3 // GDPR 删除（PII 清空 row 保留）
)

// ─── identity_type / auth_type 常量 ────────────────────────────────────────

const (
	AuthTypePassword = "password" // identifier=username/email/phone, credential=bcrypt(hash)
	AuthTypeEmail    = "email"
	AuthTypePhone    = "phone"
	AuthTypeGoogle   = "google"
	AuthTypeWechat   = "wechat"
	AuthTypeGithub   = "github"
	AuthTypeApple    = "apple"
)

// ─── tables ─────────────────────────────────────────────────────────────────

// User 主表。username / email / phone 都唯一可空（用 NULL 代替空串避免 unique 冲突）。
// password_hash 是主密码 bcrypt，登录走 username/email/phone + password。
// status 1=active, 0=disabled, 2=locked, 3=deleted。
//
// id 不用 auto_increment：由 idgen.BizTagUser 分配（保留段 [1e8, 9e8)，跟
// accounting-system 的 user 段保持一致）。CreateUser 时调用方传入。
type User struct {
	ID            int64     `gorm:"column:id;primaryKey"`
	Username      *string   `gorm:"column:username;type:varchar(50);uniqueIndex"`
	Email         *string   `gorm:"column:email;type:varchar(100);uniqueIndex"`
	Phone         *string   `gorm:"column:phone;type:varchar(20);uniqueIndex"`
	PasswordHash  string    `gorm:"column:password_hash;type:varchar(255);not null"`
	Status        int8      `gorm:"column:status;default:1"`
	TOTPSecret    string    `gorm:"column:totp_secret;type:varchar(64);default:''"` // base32 TOTP
	TOTPEnabled   bool      `gorm:"column:totp_enabled;default:false"`
	EmailVerified bool      `gorm:"column:email_verified;default:false"`
	PhoneVerified bool      `gorm:"column:phone_verified;default:false"`
	CreatedAt     time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt     time.Time `gorm:"column:updated_at;autoUpdateTime"`
	// 失败 / 锁定相关
	FailedLoginCount int        `gorm:"column:failed_login_count;default:0"`
	LockedUntil      *time.Time `gorm:"column:locked_until"`
	LastLoginAt      *time.Time `gorm:"column:last_login_at"`
	// 最后一次密码改动时间（含 reset / 改密）。给风控 ATO 规则
	// minutes_since_password_change 用。CreatedAt 兜底（注册即"刚改过密码"）。
	PasswordChangedAt *time.Time `gorm:"column:password_changed_at"`
}

func (User) TableName() string { return "users" }

// UserProfile 资料表（1:1 user）。可选字段全 nullable。
type UserProfile struct {
	UserID   int64      `gorm:"column:user_id;primaryKey"`
	Nickname *string    `gorm:"column:nickname;type:varchar(50)"`
	Avatar   *string    `gorm:"column:avatar;type:varchar(255)"`
	Gender   int8       `gorm:"column:gender;default:0"` // 0未知 1男 2女
	Birthday *time.Time `gorm:"column:birthday;type:date"`
	Bio      *string    `gorm:"column:bio;type:text"`
}

func (UserProfile) TableName() string { return "user_profiles" }

// UserAuth 多渠道认证。auth_type+identifier 全平台唯一。
//
// 关系：
//   - auth_type=password：identifier 通常是 username/email/phone（冗余于 users
//     主表字段；存在让"用 X 登录 → user_auths 反查 user_id"成为唯一查询路径）
//   - auth_type=google/wechat/...：identifier 是 OpenID，credential 存 oauth
//     access_token（建议 KMS 加密）
type UserAuth struct {
	ID         int64     `gorm:"column:id;primaryKey;autoIncrement"`
	UserID     int64     `gorm:"column:user_id;index"`
	AuthType   string    `gorm:"column:auth_type;type:varchar(20);uniqueIndex:uk_auth,priority:1"`
	Identifier string    `gorm:"column:identifier;type:varchar(100);uniqueIndex:uk_auth,priority:2"`
	Credential string    `gorm:"column:credential;type:varchar(255)"`
	Verified   bool      `gorm:"column:verified;default:false"`
	CreatedAt  time.Time `gorm:"column:created_at;autoCreateTime"`
}

func (UserAuth) TableName() string { return "user_auths" }

// LoginLog 登录日志（成功 / 失败）。给风控 + 安全审计用。
type LoginLog struct {
	ID        int64     `gorm:"column:id;primaryKey;autoIncrement"`
	UserID    *int64    `gorm:"column:user_id;index"` // 失败时可为空（用户名不存在）
	IP        string    `gorm:"column:ip;type:varchar(45)"`
	UserAgent string    `gorm:"column:user_agent;type:varchar(255)"`
	AuthType  string    `gorm:"column:auth_type;type:varchar(20)"`
	Status    int8      `gorm:"column:status"` // 1=success 0=failed
	Reason    string    `gorm:"column:reason;type:varchar(64)"`
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
}

func (LoginLog) TableName() string { return "login_logs" }

// UserSession 会话表。token 是 JWT（或 opaque session id）；登出 / 风控冻结
// 时把 row 删除即可即时撤销（service 层登录态校验同时验 JWT + 这里的有效性）。
type UserSession struct {
	ID        int64     `gorm:"column:id;primaryKey;autoIncrement"`
	UserID    int64     `gorm:"column:user_id;index"`
	Token     string    `gorm:"column:token;type:varchar(255);uniqueIndex"`
	IP        string    `gorm:"column:ip;type:varchar(45)"`
	UserAgent string    `gorm:"column:user_agent;type:varchar(255)"`
	ExpiresAt time.Time `gorm:"column:expires_at;index"`
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
}

func (UserSession) TableName() string { return "user_sessions" }

// Role / UserRole / Permission / RolePermission — RBAC。
type Role struct {
	ID          int64  `gorm:"column:id;primaryKey;autoIncrement"`
	Name        string `gorm:"column:name;type:varchar(50);uniqueIndex"`
	Description string `gorm:"column:description;type:varchar(255)"`
}

func (Role) TableName() string { return "roles" }

type UserRole struct {
	UserID int64 `gorm:"column:user_id;primaryKey"`
	RoleID int64 `gorm:"column:role_id;primaryKey"`
}

func (UserRole) TableName() string { return "user_roles" }

type Permission struct {
	ID   int64  `gorm:"column:id;primaryKey;autoIncrement"`
	Name string `gorm:"column:name;type:varchar(100)"`
	Code string `gorm:"column:code;type:varchar(100);uniqueIndex"`
}

func (Permission) TableName() string { return "permissions" }

type RolePermission struct {
	RoleID       int64 `gorm:"column:role_id;primaryKey"`
	PermissionID int64 `gorm:"column:permission_id;primaryKey"`
}

func (RolePermission) TableName() string { return "role_permissions" }

// UserAccount 用户在 accounting-system 开设的账户引用。
//
// 一个用户可以开多币种账户（如 USD + PHP）；本表只存 (user_id, currency,
// account_id) 关联映射，真实余额 + 借贷 + 流水都在 accounting-system 里。
//
// account_id 由 accounting-system 在 CreateAccount 时生成；首次注册或商户首
// 次接入新币种时调 accounting.CreateAccount → 拿到 account_id → 写本表。
type UserAccount struct {
	ID        int64     `gorm:"column:id;primaryKey;autoIncrement"`
	UserID    int64     `gorm:"column:user_id;uniqueIndex:uk_user_currency,priority:1"`
	Currency  string    `gorm:"column:currency;type:varchar(8);uniqueIndex:uk_user_currency,priority:2"`
	AccountID string    `gorm:"column:account_id;type:varchar(64);index"` // accounting-system 主键
	Status    string    `gorm:"column:status;type:varchar(16);default:'active'"`
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
}

func (UserAccount) TableName() string { return "user_accounts" }

// UserSettings JSON。前端 UI 偏好 / 通知开关 / 隐私 等。
type UserSettings struct {
	UserID   int64  `gorm:"column:user_id;primaryKey"`
	Settings string `gorm:"column:settings;type:json"`
}

func (UserSettings) TableName() string { return "user_settings" }

// EmailCode 邮件 / 短信 6 位验证码（sha256 不存明文）+ purpose 防跨场景复用。
type EmailCode struct {
	ID         int64      `gorm:"column:id;primaryKey;autoIncrement"`
	Identifier string     `gorm:"column:identifier;type:varchar(254);index"`
	CodeHash   string     `gorm:"column:code_hash;type:varchar(64)"`
	Purpose    string     `gorm:"column:purpose;type:varchar(32)"`
	ExpiresAt  time.Time  `gorm:"column:expires_at;index"`
	UsedAt     *time.Time `gorm:"column:used_at"`
	Attempts   int        `gorm:"column:attempts;default:0"`
	CreatedAt  time.Time  `gorm:"column:created_at;autoCreateTime"`
}

func (EmailCode) TableName() string { return "email_codes" }
