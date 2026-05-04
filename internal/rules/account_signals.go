package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xiongwp/risk-manage/internal/engine"
	"github.com/xiongwp/risk-manage/internal/store"
)

// ─── new_account_high_value ────────────────────────────────────────
//
// "刚注册账号 + 立即做高价值操作（提现 / 大额支付）"= 经典黑产模式。
//
// 配置：
//
//	type: new_account_high_value
//	config:
//	  max_account_age_seconds: 3600   # 账号 < 1h 视为新
//	  min_amount_usd:          50000  # AmountUSD > 阈值
//	  decision: deny

type NewAccountHighValueConfig struct {
	MaxAccountAgeSeconds int64  `json:"max_account_age_seconds"`
	MinAmountUSD         int64  `json:"min_amount_usd"`
	Decision             string `json:"decision"`
}

type newAccountHighValueRule struct {
	id, name string
	enabled  bool
	cfg      NewAccountHighValueConfig
	verdict  engine.Decision
}

func NewAccountHighValueFactory() engine.RuleFactory {
	return func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		var cfg NewAccountHighValueConfig
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return nil, fmt.Errorf("new_account_high_value: %w", err)
			}
		}
		if cfg.MaxAccountAgeSeconds <= 0 {
			cfg.MaxAccountAgeSeconds = 3600
		}
		if cfg.MinAmountUSD <= 0 {
			cfg.MinAmountUSD = 50000
		}
		v := engine.Review
		if strings.EqualFold(cfg.Decision, "deny") {
			v = engine.Deny
		}
		return &newAccountHighValueRule{id: id, name: name, enabled: enabled, cfg: cfg, verdict: v}, nil
	}
}

func (r *newAccountHighValueRule) ID() string    { return r.id }
func (r *newAccountHighValueRule) Name() string  { return r.name }
func (r *newAccountHighValueRule) Type() string  { return "new_account_high_value" }
func (r *newAccountHighValueRule) Enabled() bool { return r.enabled }

func (r *newAccountHighValueRule) Evaluate(_ context.Context, txn *engine.TxnContext) *engine.Hit {
	if txn == nil || txn.AccountAgeSeconds <= 0 {
		return nil
	}
	if txn.AccountAgeSeconds > r.cfg.MaxAccountAgeSeconds {
		return nil
	}
	amount := txn.AmountUSD
	if amount == 0 {
		amount = txn.Amount
	}
	if amount < r.cfg.MinAmountUSD {
		return nil
	}
	return &engine.Hit{
		RuleID: r.id, RuleName: r.name, Decision: r.verdict, Force: true,
		Detail: fmt.Sprintf("new account (%ds old) + high value ($%d) — likely abuse",
			txn.AccountAgeSeconds, amount),
	}
}

// ─── login_anomaly ─────────────────────────────────────────────────
//
// 异地登录 / 失败登录暴增 / 跨多国登录 = 撞库 / 账号被盗信号。
//
// 配置：
//
//	type: login_anomaly
//	config:
//	  max_failed_24h:    5
//	  max_countries:     1
//	  decision:          review

type LoginAnomalyConfig struct {
	MaxFailed24h int    `json:"max_failed_24h"`
	MaxCountries int    `json:"max_countries"`
	Decision     string `json:"decision"`
}

type loginAnomalyRule struct {
	id, name string
	enabled  bool
	cfg      LoginAnomalyConfig
	verdict  engine.Decision
}

func LoginAnomalyFactory() engine.RuleFactory {
	return func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		var cfg LoginAnomalyConfig
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return nil, fmt.Errorf("login_anomaly: %w", err)
			}
		}
		if cfg.MaxFailed24h <= 0 {
			cfg.MaxFailed24h = 5
		}
		if cfg.MaxCountries <= 0 {
			cfg.MaxCountries = 1
		}
		v := engine.Review
		if strings.EqualFold(cfg.Decision, "deny") {
			v = engine.Deny
		}
		return &loginAnomalyRule{id: id, name: name, enabled: enabled, cfg: cfg, verdict: v}, nil
	}
}

func (r *loginAnomalyRule) ID() string    { return r.id }
func (r *loginAnomalyRule) Name() string  { return r.name }
func (r *loginAnomalyRule) Type() string  { return "login_anomaly" }
func (r *loginAnomalyRule) Enabled() bool { return r.enabled }

func (r *loginAnomalyRule) Evaluate(_ context.Context, txn *engine.TxnContext) *engine.Hit {
	if txn == nil || txn.EventType != "login" {
		return nil
	}
	reasons := make([]string, 0, 2)
	if txn.LoginFailedCount24h > r.cfg.MaxFailed24h {
		reasons = append(reasons, fmt.Sprintf("failed_24h=%d > %d (credential stuffing?)",
			txn.LoginFailedCount24h, r.cfg.MaxFailed24h))
	}
	if txn.LoginCountriesRecent > r.cfg.MaxCountries {
		reasons = append(reasons, fmt.Sprintf("countries_recent=%d > %d (account takeover?)",
			txn.LoginCountriesRecent, r.cfg.MaxCountries))
	}
	if len(reasons) == 0 {
		return nil
	}
	return &engine.Hit{
		RuleID: r.id, RuleName: r.name, Decision: r.verdict,
		Detail: "login anomaly: " + strings.Join(reasons, "; "),
	}
}

// ─── email_pattern ─────────────────────────────────────────────────
//
// 批量注册检测：disposable 邮箱域名 + 邮箱前缀模式（test001 / user2025...
// 等数字后缀模式）。
//
// 配置：
//
//	type: email_pattern
//	config:
//	  disposable_domains: ["yopmail.com","mailinator.com","tempmail.com","10minutemail.com","guerrillamail.com"]
//	  numeric_suffix_min_digits: 4   # 邮箱 local-part 末尾连续数字 >= 此长度命中
//	  decision: deny

type EmailPatternConfig struct {
	DisposableDomains      []string `json:"disposable_domains"`
	NumericSuffixMinDigits int      `json:"numeric_suffix_min_digits"`
	Decision               string   `json:"decision"`
}

type emailPatternRule struct {
	id, name      string
	enabled       bool
	disposableSet map[string]struct{}
	minDigits     int
	verdict       engine.Decision
	suffix        string
}

func EmailPatternFactory() engine.RuleFactory {
	return func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		var cfg EmailPatternConfig
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return nil, fmt.Errorf("email_pattern: %w", err)
			}
		}
		if cfg.NumericSuffixMinDigits <= 0 {
			cfg.NumericSuffixMinDigits = 4
		}
		if len(cfg.DisposableDomains) == 0 {
			cfg.DisposableDomains = defaultDisposable
		}
		set := make(map[string]struct{}, len(cfg.DisposableDomains))
		for _, d := range cfg.DisposableDomains {
			set[strings.ToLower(strings.TrimSpace(d))] = struct{}{}
		}
		v := engine.Review
		if strings.EqualFold(cfg.Decision, "deny") {
			v = engine.Deny
		}
		return &emailPatternRule{
			id: id, name: name, enabled: enabled,
			disposableSet: set, minDigits: cfg.NumericSuffixMinDigits, verdict: v,
		}, nil
	}
}

var defaultDisposable = []string{
	"yopmail.com", "mailinator.com", "tempmail.com", "10minutemail.com",
	"guerrillamail.com", "trashmail.com", "throwawaymail.com", "fakeinbox.com",
	"sharklasers.com", "getairmail.com", "maildrop.cc", "mintemail.com",
}

func (r *emailPatternRule) ID() string    { return r.id }
func (r *emailPatternRule) Name() string  { return r.name }
func (r *emailPatternRule) Type() string  { return "email_pattern" }
func (r *emailPatternRule) Enabled() bool { return r.enabled }

func (r *emailPatternRule) Evaluate(_ context.Context, txn *engine.TxnContext) *engine.Hit {
	if txn == nil || txn.Metadata == nil {
		return nil
	}
	domain := strings.ToLower(strings.TrimSpace(txn.EmailDomain))
	if domain == "" {
		domain = strings.ToLower(strings.TrimSpace(txn.Metadata["email_domain"]))
	}
	emailLocal := strings.ToLower(strings.TrimSpace(txn.Metadata["email_local"]))

	reasons := make([]string, 0, 2)
	if domain != "" {
		if _, ok := r.disposableSet[domain]; ok {
			reasons = append(reasons, "disposable email domain="+domain)
		}
	}
	// 末尾连续数字检测：local part 反转扫连续数字
	if emailLocal != "" {
		digits := 0
		for i := len(emailLocal) - 1; i >= 0; i-- {
			c := emailLocal[i]
			if c >= '0' && c <= '9' {
				digits++
			} else {
				break
			}
		}
		if digits >= r.minDigits {
			reasons = append(reasons, fmt.Sprintf("email local has %d trailing digits (batch pattern)", digits))
		}
	}
	if len(reasons) == 0 {
		return nil
	}
	return &engine.Hit{
		RuleID: r.id, RuleName: r.name, Decision: r.verdict, Force: r.verdict == engine.Deny,
		Detail: "email_pattern: " + strings.Join(reasons, "; "),
	}
}

// ─── register_velocity ─────────────────────────────────────────────
//
// 同 IP / device 短时间注册大量账号。
// 实现：靠 LinkStore 已经写过的 (ip → customer / device → customer) 边数。
// EventType=register 时计算关联 customer 数 ≥ 阈值即命中。
//
// 配置：
//
//	type: register_velocity
//	config:
//	  pivot:     ip            # ip / device
//	  threshold: 20            # 1 小时内（LinkStore TTL）关联超 N 个 customer
//	  decision:  deny

type RegisterVelocityConfig struct {
	Pivot     string `json:"pivot"`
	Threshold int    `json:"threshold"`
	Decision  string `json:"decision"`
}

type registerVelocityRule struct {
	id, name string
	enabled  bool
	cfg      RegisterVelocityConfig
	links    store.LinkStore
	verdict  engine.Decision
}

func RegisterVelocityFactory(links store.LinkStore) engine.RuleFactory {
	return func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		var cfg RegisterVelocityConfig
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return nil, fmt.Errorf("register_velocity: %w", err)
			}
		}
		if cfg.Pivot == "" {
			cfg.Pivot = "ip"
		}
		if cfg.Threshold <= 0 {
			cfg.Threshold = 20
		}
		v := engine.Deny
		if strings.EqualFold(cfg.Decision, "review") {
			v = engine.Review
		}
		return &registerVelocityRule{id: id, name: name, enabled: enabled, cfg: cfg, links: links, verdict: v}, nil
	}
}

func (r *registerVelocityRule) ID() string    { return r.id }
func (r *registerVelocityRule) Name() string  { return r.name }
func (r *registerVelocityRule) Type() string  { return "register_velocity" }
func (r *registerVelocityRule) Enabled() bool { return r.enabled }

func (r *registerVelocityRule) Evaluate(ctx context.Context, txn *engine.TxnContext) *engine.Hit {
	if txn == nil || r.links == nil || txn.EventType != "register" {
		return nil
	}
	pivotKey := pivotValue(r.cfg.Pivot, txn)
	if pivotKey == "" {
		return nil
	}
	customers := r.links.Peers(ctx, pivotKey, "customer:")
	if len(customers) < r.cfg.Threshold {
		return nil
	}
	return &engine.Hit{
		RuleID: r.id, RuleName: r.name, Decision: r.verdict,
		Detail: fmt.Sprintf("%s %q linked to %d customers in window (register velocity, threshold %d)",
			r.cfg.Pivot, pivotKey, len(customers), r.cfg.Threshold),
	}
}
