package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/google/cel-go/cel"

	"github.com/xiongwp/risk-manage/internal/engine"
)

// ─── cel: google/cel-go 表达式规则 ─────────────────────────────────
//
// 跟 DSL 并存：DSL 扁平 AND/OR + 13 算子覆盖 80% 场景，CEL 用于嵌套布尔 /
// 函数 / 列表 comprehension 的剩余 20%。
//
// 配置（admin /admin/rules/update Body.config）：
//
//	type: cel
//	config:
//	  expression: 'amount > 50000 && customer.country == "PH"'
//	  decision:   review        # review / deny；默认 review
//	  weight:     25            # 命中累加 score
//
// 顶层变量（全 DynType，支持嵌套访问）：
//
//	标量：amount, currency, country, method, merchant_id, customer_id,
//	     ip_address, ip_country, ml_score, device_id, user_agent, platform,
//	     language, hardware_concurrency, time_to_checkout_ms, keystroke_count,
//	     event_type, account_age_seconds, fingerprint_hash, risk_session_id,
//	     amount_usd, hour_of_day, day_of_week, is_weekend, is_business_hours
//	对象：customer / ip / device / behavior / card / threeds / email / phone / login
//	映射：metadata (map<string,string>)
//
// 风险兜底：
//   - panic 用 recover wrap（Evaluate 内）
//   - 死循环 cel.CostLimit(celMaxCost) 在 Program 阶段
//   - nil 字段在 fillActivation 时统一填零值；表达式 has() 守护亦可
//   - 表达式拒收：Factory Parse+Check，错误规则不进 engine
//
// 不引 internal/service：TxnContext 在 internal/engine，rules 包已 import 过。

// celMaxCost CEL 表达式单次 Eval 的最大计算成本，防写出 O(N^2) 字段
// comprehension 把 CPU 打满。100 = 大约 50~100 个 op；正常 fraud 规则 < 30。
const celMaxCost uint64 = 100

// CELConfig CEL 规则的配置 schema（admin /admin/rules/update Body.config）。
type CELConfig struct {
	Expression string `json:"expression"`
	Decision   string `json:"decision"`
	Weight     int    `json:"weight"`
}

// celRule 实现 engine.Rule 接口。Program 编译结果在 Factory 阶段一次性
// 构造并缓存到 struct，Evaluate 路径只跑 prg.Eval()。
type celRule struct {
	id, name string
	enabled  bool
	expr     string
	verdict  engine.Decision
	weight   int
	prg      cel.Program

	// activationPool 复用 map[string]any（每次 Eval 分配 ~20 个 key
	// 在 100K 笔/秒 时会触发频繁 GC；池化降到 0 alloc）。
	activationPool sync.Pool
}

// CELFactory 注册到 engine：eng.RegisterFactory("cel", rules.CELFactory())。
func CELFactory() engine.RuleFactory {
	return func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		var cfg CELConfig
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("cel config: %w", err)
		}
		if strings.TrimSpace(cfg.Expression) == "" {
			return nil, fmt.Errorf("cel: expression required")
		}
		prg, err := compileCEL(cfg.Expression)
		if err != nil {
			return nil, fmt.Errorf("cel rule %q: %w", id, err)
		}
		verdict := engine.Review
		if strings.EqualFold(cfg.Decision, "deny") {
			verdict = engine.Deny
		}
		return &celRule{
			id: id, name: name, enabled: enabled,
			expr: cfg.Expression, verdict: verdict, weight: cfg.Weight,
			prg: prg,
		}, nil
	}
}

func (r *celRule) ID() string    { return r.id }
func (r *celRule) Name() string  { return r.name }
func (r *celRule) Type() string  { return "cel" }
func (r *celRule) Enabled() bool { return r.enabled }
func (r *celRule) Weight() int   { return r.weight }

// Evaluate 跑预编译的 CEL Program。任何异常（panic / eval error / 非 bool
// 返回）一律视作"未命中"，绝不把异常扩散到 engine 主循环。
func (r *celRule) Evaluate(_ context.Context, txn *engine.TxnContext) (hit *engine.Hit) {
	if txn == nil || r.prg == nil {
		return nil
	}
	// recover：CEL 内部 reflect / proto 路径理论上不 panic，但用户传 nil
	// slice / map 在某些边缘有 nil deref 风险。统一吞掉。
	defer func() {
		if rec := recover(); rec != nil {
			hit = nil
		}
	}()
	act := r.acquireActivation(txn)
	defer r.releaseActivation(act)
	out, _, err := r.prg.Eval(act)
	if err != nil {
		return nil
	}
	b, ok := out.Value().(bool)
	if !ok || !b {
		return nil
	}
	return &engine.Hit{
		RuleID:   r.id,
		RuleName: r.name,
		Decision: r.verdict,
		Detail:   "cel: " + r.expr,
	}
}

func (r *celRule) acquireActivation(txn *engine.TxnContext) map[string]any {
	if v := r.activationPool.Get(); v != nil {
		m := v.(map[string]any)
		for k := range m {
			delete(m, k)
		}
		fillActivation(m, txn)
		return m
	}
	m := make(map[string]any, 32)
	fillActivation(m, txn)
	return m
}

func (r *celRule) releaseActivation(m map[string]any) { r.activationPool.Put(m) }

// ── CEL 环境 + 编译 ───────────────────────────────────────────────

// celEnv 全局共享环境。所有 cel 规则的变量声明一致 → Env 复用 0 alloc。
var (
	celEnv     *cel.Env
	celEnvOnce sync.Once
	celEnvErr  error
)

// celVars 表达式可访问的顶层变量名。全部声明成 DynType，让用户写
// `customer.country` / `metadata.email_hash` 等嵌套访问无需逐字段 schema。
var celVars = []string{
	"amount", "currency", "country", "method", "merchant_id", "customer_id",
	"ip_address", "ip_country", "ml_score", "device_id", "user_agent",
	"platform", "language", "hardware_concurrency", "time_to_checkout_ms",
	"keystroke_count", "event_type", "account_age_seconds", "fingerprint_hash",
	"risk_session_id", "amount_usd", "hour_of_day", "day_of_week",
	"is_weekend", "is_business_hours",
	"customer", "ip", "device", "behavior", "card", "threeds",
	"email", "phone", "login", "metadata",
}

func buildCELEnv() (*cel.Env, error) {
	opts := make([]cel.EnvOption, 0, len(celVars))
	for _, v := range celVars {
		opts = append(opts, cel.Variable(v, cel.DynType))
	}
	return cel.NewEnv(opts...)
}

func getCELEnv() (*cel.Env, error) {
	celEnvOnce.Do(func() { celEnv, celEnvErr = buildCELEnv() })
	return celEnv, celEnvErr
}

// compileCEL Parse + Check + Program。Factory 阶段调用一次，缓存结果。
func compileCEL(expr string) (cel.Program, error) {
	env, err := getCELEnv()
	if err != nil {
		return nil, fmt.Errorf("cel env: %w", err)
	}
	ast, iss := env.Parse(expr)
	if iss != nil && iss.Err() != nil {
		return nil, fmt.Errorf("parse: %w", iss.Err())
	}
	checked, iss := env.Check(ast)
	if iss != nil && iss.Err() != nil {
		return nil, fmt.Errorf("check: %w", iss.Err())
	}
	// 验证返回类型必须是 bool；返 string / int 的表达式拒收（运营要写命中条件）。
	if !checked.OutputType().IsExactType(cel.BoolType) {
		return nil, fmt.Errorf("expression must return bool, got %s", checked.OutputType().String())
	}
	prg, err := env.Program(checked,
		cel.CostLimit(celMaxCost),
		cel.EvalOptions(cel.OptOptimize),
	)
	if err != nil {
		return nil, fmt.Errorf("program: %w", err)
	}
	return prg, nil
}

// ── TxnContext → CEL activation map ───────────────────────────────

// fillActivation 把 TxnContext 摊平成 map[string]any 喂给 CEL。所有缺字段
// 填零值（不留 nil），让 has() 守护和直接访问都安全。
func fillActivation(m map[string]any, t *engine.TxnContext) {
	// 标量
	m["amount"], m["currency"], m["country"] = t.Amount, t.Currency, t.Country
	m["method"], m["merchant_id"], m["customer_id"] = t.PaymentMethod, t.MerchantID, t.CustomerID
	m["ip_address"], m["ip_country"], m["ml_score"] = t.IPAddress, t.IPCountry, t.MLScore
	m["device_id"], m["user_agent"], m["platform"] = t.DeviceID, t.UserAgent, t.Platform
	m["language"], m["hardware_concurrency"] = t.Language, t.HardwareConcurrency
	m["time_to_checkout_ms"], m["keystroke_count"] = t.TimeToCheckoutMs, t.KeystrokeCount
	m["event_type"], m["account_age_seconds"] = t.EventType, t.AccountAgeSeconds
	m["fingerprint_hash"], m["risk_session_id"] = t.FingerprintHash, t.RiskSessionID
	m["amount_usd"], m["hour_of_day"], m["day_of_week"] = t.AmountUSD, t.HourOfDay, t.DayOfWeek
	m["is_weekend"], m["is_business_hours"] = t.IsWeekend, t.IsBusinessHours
	// 分组对象
	m["customer"] = map[string]any{
		"country":                t.Country,
		"paid_count_90d":         t.CustomerPaidCount90d,
		"chargeback_count_90d":   t.CustomerChargebackCount90d,
		"avg_amount":             t.CustomerAvgAmount,
		"distinct_merchants_90d": t.CustomerDistinctMerchants90d,
		"distinct_devices_90d":   t.CustomerDistinctDevices90d,
		"distinct_ips_90d":       t.CustomerDistinctIPs90d,
		"account_age_seconds":    t.AccountAgeSeconds,
		"is_new_device":          t.IsNewDevice, "is_new_ip": t.IsNewIP,
		"login_failed_24h":       t.LoginFailedCount24h,
		"login_countries_recent": t.LoginCountriesRecent,
	}
	m["ip"] = map[string]any{"country": t.IPCountry, "proxy": t.IPProxy,
		"vpn": t.IPVPN, "data_center": t.IPDataCenter, "asn": t.IPASN}
	m["device"] = map[string]any{"id": t.DeviceID, "fingerprint_hash": t.FingerprintHash,
		"canvas": t.CanvasFingerprint, "webgl": t.WebGLRenderer,
		"audio_hash": t.AudioContextHash, "rooted": t.DeviceRooted,
		"battery_present": t.BatteryPresent, "font_hash": t.FontHash,
		"screen": t.ScreenWxH, "ua_hash": t.UAHash,
		// web-sdk v0.2 扩展信号
		"plugins_hash":         t.PluginsHash,
		"device_memory":        t.DeviceMemory,
		"pixel_ratio":          t.PixelRatio,
		"color_depth":          t.ColorDepth,
		"touch_support":        t.TouchSupport,
		"codec_hash":           t.CodecHash,
		"connection_type":      t.ConnectionType,
		"cookie_enabled":       t.CookieEnabled,
		"do_not_track":         t.DoNotTrack,
		"webdriver":            t.Webdriver,
		"cdc_globals":          stringSlice(t.CDCGlobals),
		"chrome_runtime":       t.ChromeRuntime,
		"permissions_mismatch": t.PermissionsMismatch,
		"webrtc_local_ips":     stringSlice(t.WebRTCLocalIPs),
		"signal_coverage":      t.SignalCoverageRatio,
		// mobile v0.3 attestation + 反 hook（iOS/Android SDK 上报）
		"jailbroken":           t.IsJailbroken,
		"emulator":             t.IsEmulator,
		"frida_detected":       t.FridaDetected,
		"xposed_detected":      t.XposedDetected,
		"debugger_attached":    t.DebuggerAttached,
		"attestation_verified": t.AttestationVerified,
		"attestation_kind":     t.AttestationKind,
	}
	m["behavior"] = map[string]any{"time_to_checkout_ms": t.TimeToCheckoutMs,
		"mouse_entropy": t.MouseMovementEntropy, "click_interval_ms": t.ClickIntervalMs,
		"scroll_speed": t.ScrollSpeedPxPerSec, "typing_cv": t.TypingRhythmCV,
		"keystroke_count": t.KeystrokeCount, "pasted_fields": stringSlice(t.PastedFields),
		// v0.2 mouse trajectory 派生
		"mouse_avg_speed":            t.MouseAvgSpeedPxPerMs,
		"mouse_speed_variance":       t.MouseSpeedVariance,
		"mouse_acceleration_kurt":    t.MouseAccelerationKurtosis,
		"mouse_straightness":         t.MouseStraightnessRatio,
		"mouse_pause_count":          t.MousePauseCount,
		// v0.2 keystroke biometrics
		"keystroke_dwell_mean":   t.KeystrokeDwellMean,
		"keystroke_dwell_cv":     t.KeystrokeDwellCV,
		"keystroke_flight_mean":  t.KeystrokeFlightMean,
		"keystroke_flight_cv":    t.KeystrokeFlightCV,
	}
	m["card"] = map[string]any{"brand": t.CardBrand, "funding": t.CardFunding,
		"country": t.CardCountry, "commercial": t.CardCommercial}
	m["threeds"] = map[string]any{"authenticated": t.ThreeDSAuthenticated, "result": t.ThreeDSResult}
	m["email"] = map[string]any{"hash": t.EmailHash, "domain": t.EmailDomain}
	m["phone"] = map[string]any{"hash": t.PhoneHash, "country_prefix": t.PhoneCountryPrefix, "carrier": t.PhoneCarrier}
	m["login"] = map[string]any{"city": t.LoginCity, "city_prev": t.LoginCityPrev,
		"seconds_since_last": t.SecondsSinceLastLogin, "cookie_resets_24h": t.CookieResets24h}
	m["metadata"] = metadataMap(t.Metadata)
}

func stringSlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func metadataMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
