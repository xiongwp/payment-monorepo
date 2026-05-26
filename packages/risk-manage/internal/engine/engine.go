// Package engine 是风控规则引擎。接收一笔交易的特征，依次跑所有 enabled 规则，
// 返回最严厉的 decision（DENY > REVIEW > ALLOW）。
//
// 规则类型（type 字段）对应 rules/ 包里的具体实现：
//   amount_limit   — 单笔 / 日 / 月累计限额
//   velocity       — N 笔 / T 分钟 速率限制
//   blacklist      — 商户 / 用户 / IP / 设备 黑名单
//   country_block  — 国家 / 币种白名单/黑名单
//   amount_pattern — 异常金额模式（如连续整数金额）
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/xiongwp/risk-manage/internal/metrics"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// Decision 三态
type Decision int

const (
	Allow  Decision = 1
	Deny   Decision = 2
	Review Decision = 3
)

func (d Decision) String() string {
	switch d {
	case Allow:
		return "ALLOW"
	case Deny:
		return "DENY"
	case Review:
		return "REVIEW"
	}
	return "UNKNOWN"
}

// Worse 返回两个 decision 中更严厉的那个
func Worse(a, b Decision) Decision {
	// Deny > Review > Allow
	if a == Deny || b == Deny {
		return Deny
	}
	if a == Review || b == Review {
		return Review
	}
	return Allow
}

// TxnContext 一笔交易的风控入参。字段分组（按数据来源）：
//
//	业务侧：PaymentIntentID / MerchantID / CustomerID / Amount / Currency /
//	        PaymentMethod / Country
//
//	端上 SDK 上报（fingerprint / behavior）：FingerprintHash / Platform /
//	        ScreenWxH / Timezone / UserAgent / TimeToCheckoutMs /
//	        KeystrokeCount / MouseMoves / PastedFields
//
//	IP 富化（IPIntel 服务在 Screen 入口填充）：IPCountry / IPProxy /
//	        IPVPN / IPDataCenter / IPASN
//
//	额外业务标记：Metadata
type TxnContext struct {
	// ── 业务 ────────────────────────────────────────
	PaymentIntentID string
	MerchantID      string
	CustomerID      string
	Amount          int64
	Currency        string
	PaymentMethod   string
	Country         string
	IPAddress       string
	DeviceID        string
	UserAgent       string

	// ── Fingerprint Engine（端 SDK 上报）─────────────
	// FingerprintHash 端 SDK 综合 hash（canvas + webgl + audio + ...），
	// 用于稳定识别同一设备 / 浏览器实例。同一设备多次支付应保持不变。
	FingerprintHash string
	// CanvasFingerprint canvas 渲染指纹（同 GPU + 字体 渲染相同字符串
	// 的像素 hash）。每个 GPU/驱动组合产生独一无二的输出。
	CanvasFingerprint string
	// WebGLRenderer 显卡渲染器名（"ANGLE (Intel UHD Graphics 630)" 等）；
	// 模拟器 / headless 浏览器有特征值（"SwiftShader"），可识别 bot。
	WebGLRenderer string
	// AudioContextHash 音频指纹（OfflineAudioContext 渲染输出 hash）。
	AudioContextHash string
	// ScreenWxH 屏幕分辨率 "1920x1080"。
	ScreenWxH string
	// Timezone IANA 时区名（"Asia/Manila"）。和 IPCountry 时区强对比 →
	// 时区欺骗信号（VPN 切国家但本地时区没变）。
	Timezone string
	// Language 浏览器首选语言（"zh-CN" / "en-US"）。和 Country 不匹配可疑。
	Language string
	// HardwareConcurrency 报告的 CPU 逻辑核心数；普通设备 4-16，
	// headless 浏览器经常报 0 / 1，移动端 4-8。
	HardwareConcurrency int
	// Platform "ios" / "android" / "web" / "weChat-miniprogram" 等。
	Platform string

	// ── Behavior Engine（端 SDK 上报）────────────────
	// TimeToCheckoutMs 从打开 checkout 到点击提交的耗时；过短（< 3000ms）
	// 强烈暗示自动化脚本。
	TimeToCheckoutMs int64
	// MouseMovementEntropy 鼠标轨迹的 Shannon 熵。0 = 无轨迹（bot），
	// 真实用户随机性高（>3.0 nat），过低 = 直线 bot；过高罕见但合法。
	MouseMovementEntropy float64
	// ClickIntervalMs 点击事件之间的平均间隔。bot 通常 < 50ms 或固定值
	// （如 100/200/500ms）；真实用户分布在几百到几千 ms。
	ClickIntervalMs int
	// ScrollSpeedPxPerSec 滚动速度（像素/秒）。0 = 没滚动；极高 = 自动脚本。
	ScrollSpeedPxPerSec float64
	// TypingRhythmCV 打字间隔的 coefficient of variation（std/mean）。
	// 真实用户 0.5-1.5；bot 接近 0（固定间隔）或极大（混乱）。
	TypingRhythmCV float64
	// KeystrokeCount 卡号 / 姓名等输入框的按键事件总数。0 = 全部粘贴/自动填。
	KeystrokeCount int
	// PastedFields 粘贴而非键入的字段名（["card_number", "cvc"]）。卡号 +
	// CVC 同时被粘贴 → 极强的卡号库 / 拖库信号。
	PastedFields []string

	// ── IP Intelligence（service Lookup 在 Screen 入口填充）────
	// IPCountry IP 解析的国家（ISO-2）。与 Country（订单声明国家）不一致 → 风险信号。
	IPCountry string
	// IPProxy 已知代理 IP（公开代理名单 / WHOIS 标记）。
	IPProxy bool
	// IPVPN 已知 VPN 商业服务节点。
	IPVPN bool
	// IPDataCenter IP 属于数据中心 ASN（云厂商 / 主机商）。普通用户极少
	// 直接从数据中心发卡支付。
	IPDataCenter bool
	// IPASN ASN 编号（监控用，不直接做规则）。
	IPASN string

	// ── ML scoring（service.Screen 入口调 mlscore.Service.Score 后填充）────
	// MLScore 区间 [0, 1]，越高越可疑。0 = 无模型 / 服务不可达（fail-open）。
	MLScore float64
	// MLModelVer 推理服务返回的模型版本，写到 audit 让决策可重现。
	MLModelVer string

	// RiskSessionID 端 SDK 创建的 session id（POST /v1/risk/session 返回）。
	// 业务侧把它带在支付请求里，Screen 入口用 it 查 SessionStore 富化
	// fingerprint + behavior 字段。**必须由业务侧主动传** — sdk 是公网
	// 无 auth 端点，后端不该信任客户端冒充 session_id 关联到任意账户。
	RiskSessionID string

	// IdempotencyKey 业务侧传入的幂等键（如 paymentIntent 的 client_token /
	// 商户的 reference_id）。同一 key 在 idempotency window 内复用上一次
	// Screen 的 Result，避免重试导致 audit / review queue 重复落条。
	// 空字符串 = 不走幂等路径（每次都重新评估）。
	IdempotencyKey string

	// ── 时间特征（FeatureExtractor 在 Screen 入口填充）────
	// HourOfDay 0-23，按 IP 推断时区；缺时区 → UTC。商业风控用：
	// 半夜 + 大额 + 新设备 = 强 fraud 信号
	HourOfDay int
	// DayOfWeek 0=Sunday..6=Saturday
	DayOfWeek int
	// IsWeekend day_of_week ∈ {0,6}
	IsWeekend bool
	// IsBusinessHours 商户业务时段（默认 9:00-21:00 local）；外的支付加权
	IsBusinessHours bool

	// ── 币种特征（CurrencyConvertExtractor 填充）─────────
	// AmountUSD 跨币种比较 baseline；汇率服务失败时 = Amount（不换算）
	AmountUSD int64

	// ── 卡特征（CardFeatureExtractor 从 metadata.card_bin 拿）─
	// CardBrand visa / mastercard / amex / discover / unionpay / jcb / unknown
	CardBrand string
	// CardFunding credit / debit / prepaid / unknown — prepaid 卡 fraud 率高 4x
	CardFunding string
	// CardCountry BIN 国家（同 metadata.bin_country 但作为 typed 字段方便规则）
	CardCountry string
	// CardCommercial 是否商务卡（true = b2b 卡，fraud 率低）
	CardCommercial bool

	// ── 客户历史聚合（CustomerHistoryExtractor 从 LinkStore + Counter 算）────
	// CustomerPaidCount90d 过去 90 天成功支付笔数
	CustomerPaidCount90d int
	// CustomerChargebackCount90d 过去 90 天 chargeback 笔数
	CustomerChargebackCount90d int
	// CustomerAvgAmount 过去 90 天平均成功支付金额（minor unit）
	CustomerAvgAmount int64
	// CustomerDistinctMerchants90d 过去 90 天接触过的不同商户数
	CustomerDistinctMerchants90d int
	// CustomerDistinctDevices90d 过去 90 天用过的不同设备数（>3 可疑）
	CustomerDistinctDevices90d int
	// CustomerDistinctIPs90d 过去 90 天用过的不同 IP 数（>10 可疑）
	CustomerDistinctIPs90d int

	// ── 3DS / SCA 认证上下文 ─────────────────────────
	// ThreeDSAuthenticated 本笔已通过 3DS（liability shift 已发生 → 商户责任移交银行）
	ThreeDSAuthenticated bool
	// ThreeDSResult: success / attempted / failed / not_required
	ThreeDSResult string

	// ── 上一笔状态 ───────────────────────────────────
	// PrevPaymentStatus 同 customer 上一笔状态：succeeded / failed / refunded / chargeback
	PrevPaymentStatus string
	// SecondsSinceLastPayment 距上一笔成功支付的秒数；0 = 第一笔
	SecondsSinceLastPayment int64

	// ── Event 类型扩展（非纯 payment 场景）────────────
	// EventType 支持平台风控全场景：
	//   payment | register | login | password_change | bind_card | withdraw | refund
	// 默认空 = "payment"（兼容老调用）。
	EventType string
	// AccountAgeSeconds 账号注册到现在的秒数；< 阈值 + 高价值操作 = 强信号
	AccountAgeSeconds int64
	// LoginFailedCount24h 同 customer 过去 24h 失败登录次数
	LoginFailedCount24h int
	// LoginCountriesRecent 过去 N 小时登录过的不同国家数（>1 = 异地登录嫌疑）
	LoginCountriesRecent int
	// IsNewDevice 本次设备指纹之前没见过
	IsNewDevice bool
	// IsNewIP 本次 IP 之前没见过
	IsNewIP bool

	// ── 联系方式（PII：建议商户传 hash 而非原文）────────
	// EmailHash sha256(email)；用于图谱节点（email→customer 边）+ pattern 规则
	EmailHash string
	// PhoneHash sha256(E.164 phone)；同上
	PhoneHash string
	// EmailDomain 邮箱域名（明文：gmail.com / yopmail.com 等），用于
	// disposable / batch 检测，不算 PII
	EmailDomain string
	// PhoneCountryPrefix E.164 国家前缀（"+86", "+63"），便于 virtual-carrier 检测
	PhoneCountryPrefix string
	// PhoneCarrier 运营商类型 hint：mobile / virtual / voip / landline / unknown
	// 由商户在调 Screen 前从外部 carrier-lookup 服务（Twilio Lookup / Numlookup）填
	PhoneCarrier string

	// ── 客户端真实性 / 反篡改信号（前端 SDK 上报）────────
	// JSDisabled 浏览器禁了 JS（true = SDK 没在跑，请求来自非正常客户端）
	JSDisabled bool
	// BeaconBlocked 上报埋点的 navigator.sendBeacon 被拦截（adblock / 脚本环境）
	BeaconBlocked bool
	// HeaderAnomaly request headers 异常（缺 user-agent / accept-language / referer mismatch）
	HeaderAnomaly bool
	// DirectAPICall 没经过前端 SDK 直接调 API（risk_session_id 缺 / 设备指纹空）
	DirectAPICall bool
	// RequestBodyHash 请求体 sha256，IdempotencyKey 不同但 body 相同 → replay 信号
	RequestBodyHash string

	// ── 深度设备特征（Anti-emulator + 设备真实性）──────────
	// BatteryPresent 设备报告了电池 API（桌面浏览器无；移动 native true）
	// 假值 = 桌面浏览器；true = 移动；rule 配合 Platform 判模拟器
	BatteryPresent bool
	// DeviceRooted iOS 越狱 / Android root 标记（SDK 检测）
	DeviceRooted bool
	// IsJailbroken iOS 越狱信号；语义上是 DeviceRooted 的 iOS 别名，但客户端
	// SDK 也单独上报便于规则按平台分别 weight（iOS 越狱率 < Android root）。
	// 端侧字段直传，DeviceRooted = IsJailbroken || IsAndroidRooted。
	IsJailbroken bool
	// IsEmulator 模拟器 / virtual device 标记（iOS Simulator / Android Genymotion
	// / Bluestacks / Nox / LDPlayer 等）。规则常配：emulator + 大额支付 = REVIEW。
	IsEmulator bool
	// FridaDetected 端 SDK 检测到 Frida / FridaGadget 注入（dyld / proc/self/maps
	// 扫到 frida-agent / re.frida.server）。strong bot / 自动化 / 篡改信号。
	FridaDetected bool
	// XposedDetected Android Xposed / LSPosed 注入。同 FridaDetected 强信号。
	XposedDetected bool
	// DebuggerAttached SDK 启动时检测到调试器附加（iOS sysctl P_TRACED；
	// Android /proc/self/status TracerPid != 0）。生产 app 不该被调试。
	DebuggerAttached bool
	// AppAttestToken iOS App Attest attestation object（base64）。由后端
	// internal/attestation/apple_appattest.go 验 X.509 cert chain + CBOR。
	// 仅在 iOS 14+ 且首次启动时上报，后续启动用 AppAttestAssertion。
	AppAttestToken string
	// AppAttestKeyID App Attest 生成的 keyId（base64）。后端验签需要。
	AppAttestKeyID string
	// DeviceCheckToken iOS DeviceCheck token（base64）。后端调 Apple Server-to-Server
	// API（POST /v1/validate_device_token）验签；轻量级 attestation，iOS 11+。
	DeviceCheckToken string
	// PlayIntegrityToken Android Play Integrity API integrityToken（JWE）。
	// 后端用 service account 调 Google Play Developer API decode，得到
	// deviceIntegrity / appIntegrity / accountDetails 三项判定。
	PlayIntegrityToken string
	// AttestationVerified 后端验签通过 = true；空 token / 验签失败 = false。
	// fail-soft 模式下不直接拒，规则可加 weight；RISK_ATTESTATION_REQUIRED=true
	// 时 false → 直接 DENY（service.Screen 入口短路）。
	AttestationVerified bool
	// AttestationKind 实际生效的 attestation 类型：
	//   "device_check" / "app_attest" / "play_integrity" / "" (未上报)
	// 用于 audit + 规则按 kind 区分权重。
	AttestationKind string
	// FontHash 字体列表 sha256（指纹维度之一；批量设备字体一致）
	FontHash string
	// WebRTCLocalIPs WebRTC 暴露的真实 LAN IP 列表；跟 IPAddress 不一致 → VPN/代理
	WebRTCLocalIPs []string
	// SecondsSinceLastLogin 距上一次登录的秒数（cross-city 用）；0 = 第一次
	SecondsSinceLastLogin int64
	// LoginCity 本次登录推断的城市（GeoIP）；和上次登录跨城市 + 时间短 → impossible-travel
	LoginCity string
	// LoginCityPrev 上一次登录的城市（service 在 Report 上记录）
	LoginCityPrev string
	// CookieResets24h 24h 内同 customer 清 cookie / 换 cookie 次数（abnormal > 3）
	CookieResets24h int
	// TimezoneIPMismatch 设备时区跟 IP 国家时区不一致（DSL: 比较 Timezone vs ip_country 推断时区）
	TimezoneIPMismatch bool
	// UAHash sha256(UserAgent) — 用于 ua_batch_register link_fanout pivot
	UAHash string

	// ── 端 SDK v0.2+ 扩展信号（fillSessionFields 从 Snapshot 透传）────
	// PluginsHash navigator.plugins 列表 hash（headless 通常为空）
	PluginsHash string
	// DeviceMemory navigator.deviceMemory（GB）；0 = 未上报
	DeviceMemory float64
	// PixelRatio devicePixelRatio；headless 经常恰好 = 1（真机移动 2/3）
	PixelRatio float64
	// ColorDepth screen.colorDepth；headless 经常 24，真机也 24，主要用于聚合
	ColorDepth int
	// TouchSupport navigator.maxTouchPoints；> 0 = 触屏设备
	TouchSupport int
	// CodecHash MediaSource.isTypeSupported 探测集 hash
	CodecHash string
	// ConnectionType navigator.connection.effectiveType（4g / wifi / ...）
	ConnectionType string
	// CookieEnabled 浏览器报告允许 cookie；false = 隐身模式 / 拦截
	CookieEnabled bool
	// DoNotTrack '1' / '0' / 'unspecified'
	DoNotTrack string

	// 反自动化（强信号）
	// Webdriver navigator.webdriver === true（Selenium / Puppeteer 默认 true）
	Webdriver bool
	// CDCGlobals chromedriver / selenium 注入的全局变量名列表（非空 = 强 bot 信号）
	CDCGlobals []string
	// ChromeRuntime typeof chrome !== 'undefined' && chrome.runtime（扩展环境）
	ChromeRuntime bool
	// PermissionsMismatch headless Chrome 经典裂痕：Permissions API 报 prompt
	// 但 Notification.permission 报 denied
	PermissionsMismatch bool

	// 行为：鼠标轨迹派生指标
	MouseAvgSpeedPxPerMs      float64
	MouseSpeedVariance        float64
	MouseAccelerationKurtosis float64
	MouseStraightnessRatio    float64
	MousePauseCount           int

	// 行为：keystroke biometrics
	KeystrokeDwellMean  float64
	KeystrokeDwellCV    float64
	KeystrokeFlightMean float64
	KeystrokeFlightCV   float64

	// SDK 自身采集质量
	// SignalCoverageRatio = 成功信号数 / 总信号数，∈ [0, 1]。低 → 客户端
	// 环境受限（adblock / headless / iframe sandbox），可作为弱信号叠加。
	SignalCoverageRatio float64
	// SignalStatus 每信号采集状态（'ok' / 'fail' / 'timeout' / 'unsupported'），
	// 给规则做单点判断（如 canvas=fail → 真实浏览器极少；audio=timeout → 异常）
	SignalStatus map[string]string

	// ── 业务自定义 ─────────────────────────────────
	Metadata map[string]string
}

// Mode 规则的执行模式。
type Mode int

const (
	// ModeEnforce 默认；命中即影响最终 verdict。
	ModeEnforce Mode = 0
	// ModeShadow 评估但不影响 verdict；命中只记 ShadowHits + Prometheus 计数，
	// 用于上线新规则前的影子验证（Stripe Radar 风格）。配置 mode: shadow。
	ModeShadow Mode = 1
)

func (m Mode) String() string {
	if m == ModeShadow {
		return "shadow"
	}
	return "enforce"
}

// Hit 单条规则命中结果
type Hit struct {
	RuleID   string
	RuleName string
	Decision Decision
	Detail   string
	// Mode 当时这条规则的模式。Shadow 命中也填这条，方便 audit / 报告区分。
	Mode Mode
	// Force 短路：命中即终止评估，最终 verdict 直接 = Decision，忽略 score。
	// 用途：商户 allowlist 命中（Decision=Allow + Force=true）→ 立即放行；
	//      商户 blocklist 命中（Decision=Deny + Force=true）→ 立即拒。
	// 跟 Worse() 兜底机制并存，但 Force 优先级最高。
	Force bool
}

// Result 引擎整体判定
type Result struct {
	Decision  Decision
	RiskScore int // 0-100
	RiskLevel string
	// Hits enforce 模式规则命中（影响 Decision / RiskScore）
	Hits []Hit
	// ShadowHits shadow 模式规则命中（仅观察，不影响 Decision / RiskScore）
	ShadowHits []Hit
	// DecisionID service.Screen 生成；audit / review queue / outcome 三处用同一 id 串起。
	// payment-core 把它作为 X-Risk-Decision-ID 透传给商户 webhook，便于反查。
	DecisionID string
	// RecommendedAction payment-core 用来决策 3DS step-up / hold-for-review / block。
	// 当前固定映射：DENY→"block"; REVIEW→"step_up_3ds"; ALLOW→""。
	// 未来扩展：根据商户能力 / 风险类型选不同动作（如 "challenge_otp"）。
	RecommendedAction string
}

// Rule 每种规则类型要实现的接口
type Rule interface {
	ID() string
	Name() string
	Type() string
	Enabled() bool
	Evaluate(ctx context.Context, txn *TxnContext) *Hit // nil = 没命中（放行）
}

// ModedRule 可选接口：实现它的规则可单独控制 mode（enforce / shadow）。
// 老规则不实现 → 视作 ModeEnforce。
type ModedRule interface {
	Mode() Mode
}

// modeOf 取 rule 的 mode；不实现 ModedRule 则视作 enforce。
func modeOf(r Rule) Mode {
	if mr, ok := r.(ModedRule); ok {
		return mr.Mode()
	}
	return ModeEnforce
}

// RuleFactory 从 JSON config 构造 Rule 实例
type RuleFactory func(id, name string, enabled bool, configJSON json.RawMessage) (Rule, error)

// Engine 规则引擎
type Engine struct {
	mu           sync.RWMutex
	rules        []Rule
	defs         []RuleDef // 缓存最近 LoadRules 的 defs，给 admin /admin/rules/list 用
	factories    map[string]RuleFactory
	thresholds   ScoreThresholds
	policy       PolicyStore      // nil-safe：无 override → 用 baseline thresholds
	versionStore RuleVersionStore // nil-safe：无 store → UpdateRule 不写版本（详见 rule_versions.go）
	logger       *zap.Logger
}

func New(logger *zap.Logger) *Engine {
	return &Engine{
		factories:  make(map[string]RuleFactory),
		thresholds: defaultThresholds,
		logger:     logger,
	}
}

// RegisterFactory 注册规则类型工厂。启动期调用。
func (e *Engine) RegisterFactory(ruleType string, f RuleFactory) {
	e.factories[ruleType] = f
}

// LoadRules 从配置加载规则（替换当前全部）
func (e *Engine) LoadRules(defs []RuleDef) error {
	var rules []Rule
	var errs []string
	for _, d := range defs {
		f, ok := e.factories[d.Type]
		if !ok {
			// P2-1: 未知规则类型 = 配置错误，记录但不 skip（上报）
			msg := fmt.Sprintf("unknown rule type %q for rule %q", d.Type, d.ID)
			errs = append(errs, msg)
			if e.logger != nil {
				e.logger.Error("rule load: " + msg)
			}
			continue
		}
		r, err := f(d.ID, d.Name, d.Enabled, d.ConfigJSON)
		if err != nil {
			msg := fmt.Sprintf("rule %q (%s) failed: %v", d.ID, d.Type, err)
			errs = append(errs, msg)
			if e.logger != nil {
				e.logger.Error("rule load: " + msg)
			}
			continue
		}
		// 应用 weight wrapper：因子函数签名不带 weight 参数，wrap 让 Engine
		// 的 weightOf 能拿到 RuleDef.Weight；shadow 同样用 wrapper 链。
		w := d.Weight
		if w == 0 {
			w = inferWeight(d.Decision)
		}
		if w != 0 {
			r = &weightedRule{Rule: r, weight: w}
		}
		if strings.EqualFold(d.Mode, "shadow") {
			r = &shadowWrappedRule{Rule: r}
		}
		// Rollout: 灰度配置 → wrap 成 rolloutRule，evaluate 入口先按桶过滤。
		// EnablePct=0 不会到这里（被前面 EnablePct=0 的快速跳过路径排除），
		// EnablePct=100 / 缺省 → 不 wrap（无运行时开销）。
		if d.Rollout.EnablePct > 0 && d.Rollout.EnablePct < 100 {
			r = &rolloutRule{Rule: r, ruleID: d.ID, rollout: d.Rollout}
		}
		rules = append(rules, r)
	}
	if len(errs) > 0 && e.logger != nil {
		e.logger.Warn("some rules failed to load",
			zap.Int("failed", len(errs)), zap.Int("loaded", len(rules)))
	}
	e.mu.Lock()
	e.rules = rules
	// 拷贝 defs 给 admin /admin/rules/list 用；不是引用，避免 caller mutate。
	e.defs = make([]RuleDef, len(defs))
	copy(e.defs, defs)
	e.mu.Unlock()
	e.logger.Info("rules loaded", zap.Int("count", len(rules)))
	return nil
}

// RuleDef 规则定义（从 YAML config 反序列化）
type RuleDef struct {
	ID         string          `mapstructure:"id"`
	Name     string `mapstructure:"name"`
	Type     string `mapstructure:"type"`
	Decision string `mapstructure:"decision"` // DENY / REVIEW
	Enabled  bool   `mapstructure:"enabled"`
	// Mode "enforce"（默认）/ "shadow"。shadow 命中只观察不阻断流量，
	// 上线新规则前用于影子验证；切到 enforce 之前看 risk_rule_evaluation_total
	// {result="shadow_hit"} 的命中率 + overlap 指标决定是否切换。
	Mode string `mapstructure:"mode"`
	// Weight 命中时累加的风险分数（Radar-style score matrix）。
	//   - 0 时按 Decision 兜底：DENY → 50, REVIEW → 20, 其它 → 0
	//   - 不直接决定 verdict；总分通过 ScoreThresholds 映射成 ALLOW / REVIEW / DENY
	//   - 老规则配 Decision: deny + Weight 不填 → 行为完全等价于上一版本
	Weight     int             `mapstructure:"weight"`
	ConfigJSON json.RawMessage `mapstructure:"config"`
	// Rollout 灰度配置：bucket_field + enable_pct。
	// EnablePct=0 等价 disabled；100 / 缺省 = 全量启用。
	Rollout RolloutConfig `mapstructure:"rollout" json:"rollout,omitempty"`
}

// ScoreThresholds 把累加后的 risk score 映射到 verdict。**>= 比较**：
//
//	score >= DenyMin    → Deny
//	score >= ReviewMin  → Review
//	否则                  → Allow
//
// 通过 Engine.SetScoreThresholds 热更新；admin 改完 system_config 立即生效。
type ScoreThresholds struct {
	ReviewMin int
	DenyMin   int
}

// 默认阈值：和老逻辑一致 — 一条 review (20) → review；一条 deny (50) → deny。
// 运营可通过 admin 调小让更多边缘 case 落 review。
var defaultThresholds = ScoreThresholds{ReviewMin: 20, DenyMin: 50}

// Evaluate 跑所有 enabled 规则，返回最终 decision + 所有 hit。
//
// 决策模型（Radar-style score matrix）：
//   - 每条规则命中累加 weight 到 RiskScore（weight 来自 RuleDef.Weight，缺省时
//     按 Decision 兜底：DENY=50 / REVIEW=20）
//   - 累计分数通过 thresholds 映射成最终 verdict：
//       score >= DenyMin   → Deny
//       score >= ReviewMin → Review
//       否则                  → Allow
//   - **同时**保留 hit-level Worse 兜底：单条 deny 即使 weight=0 也直接走 Deny，
//     防误配；阈值和 hit-level 取较严的那个
//
// shadow 模式规则单独走：命中只进 ShadowHits，不影响 verdict / score。
func (e *Engine) Evaluate(ctx context.Context, txn *TxnContext) *Result {
	tracer := otel.Tracer("risk-manage/engine")
	ctx, evalSpan := tracer.Start(ctx, "engine.Evaluate",
		trace.WithAttributes(
			attribute.String("merchant_id", merchantOf(txn)),
			attribute.String("payment_method", piMethodOf(txn)),
		))
	defer evalSpan.End()

	e.mu.RLock()
	rules := e.rules
	thresholds := e.thresholds
	e.mu.RUnlock()

	// 商户 override：阈值 / 禁用规则 / weight 调整。
	// 任何字段缺省都退到 baseline，确保未配 override 的商户行为不变。
	var disabled map[string]struct{}
	var weightOverrides map[string]int
	if txn != nil {
		if pol := e.effectivePolicy(txn.MerchantID); pol != nil {
			if pol.ReviewMin > 0 {
				thresholds.ReviewMin = pol.ReviewMin
			}
			if pol.DenyMin > 0 {
				thresholds.DenyMin = pol.DenyMin
			}
			if len(pol.DisabledRules) > 0 {
				disabled = make(map[string]struct{}, len(pol.DisabledRules))
				for _, id := range pol.DisabledRules {
					disabled[id] = struct{}{}
				}
			}
			if len(pol.WeightOverrides) > 0 {
				weightOverrides = pol.WeightOverrides
			}
		}
	}

	res := &Result{Decision: Allow, RiskLevel: "low"}
	score := 0
	hitLevel := Allow
	for _, r := range rules {
		if !r.Enabled() {
			continue
		}
		if _, off := disabled[r.ID()]; off {
			metrics.RuleEvalTotal.WithLabelValues(r.ID(), r.Type(), "merchant_disabled").Inc()
			continue
		}
		ruleStart := time.Now()
		ruleCtx, ruleSpan := tracer.Start(ctx, "rule."+r.Type(),
			trace.WithAttributes(
				attribute.String("rule_id", r.ID()),
				attribute.String("rule_type", r.Type()),
			))
		hit := r.Evaluate(ruleCtx, txn)
		evalLat := time.Since(ruleStart).Seconds()
		metrics.RuleEvalDuration.WithLabelValues(r.ID(), r.Type()).Observe(evalLat)
		if hit != nil {
			ruleSpan.SetAttributes(
				attribute.String("decision", hit.Decision.String()),
				attribute.Bool("force", hit.Force),
			)
		}
		ruleSpan.End()
		mode := modeOf(r)
		if hit == nil {
			metrics.RuleEvalTotal.WithLabelValues(r.ID(), r.Type(), "miss").Inc()
			continue
		}
		// shadow 命中：记 shadow_hit + 独立列表，绝不影响 Decision/score
		if mode == ModeShadow {
			hit.Mode = ModeShadow
			res.ShadowHits = append(res.ShadowHits, *hit)
			metrics.RuleEvalTotal.WithLabelValues(r.ID(), r.Type(), "shadow_hit").Inc()
			continue
		}
		hit.Mode = ModeEnforce
		metrics.RuleEvalTotal.WithLabelValues(r.ID(), r.Type(), "hit").Inc()
		res.Hits = append(res.Hits, *hit)
		// Force 短路：商户 allowlist / blocklist 命中 → 直接终止评估。
		// allowlist hit (Decision=Allow + Force) → 整笔 ALLOW，无视后续 deny 规则；
		// blocklist hit (Decision=Deny  + Force) → 整笔 DENY， 不再评估。
		if hit.Force {
			res.Decision = hit.Decision
			res.RiskScore = 0
			if hit.Decision == Deny {
				res.RiskScore = 100
			}
			res.RiskLevel = scoreLevel(res.RiskScore)
			return res
		}
		// 加权累加：商户 weight override > 规则自身 weight > 按 hit.Decision 兜底。
		w := 0
		if v, ok := weightOverrides[r.ID()]; ok {
			w = v
		} else {
			w = weightOf(r)
		}
		if w == 0 {
			w = inferWeightDecision(hit.Decision)
		}
		score += w
		hitLevel = Worse(hitLevel, hit.Decision)
	}
	if score > 100 {
		score = 100
	}
	res.RiskScore = score
	res.Decision = Worse(hitLevel, mapScoreToVerdict(score, thresholds))
	res.RiskLevel = scoreLevel(score)
	evalSpan.SetAttributes(
		attribute.String("verdict", res.Decision.String()),
		attribute.Int("risk_score", res.RiskScore),
		attribute.String("risk_level", res.RiskLevel),
		attribute.Int("hits", len(res.Hits)),
		attribute.Int("shadow_hits", len(res.ShadowHits)),
	)
	return res
}

// inferWeight 从 RuleDef.Decision（DENY/REVIEW）兜底成 weight。LoadRules 用。
func inferWeight(decisionStr string) int {
	switch strings.ToUpper(strings.TrimSpace(decisionStr)) {
	case "DENY":
		return 50
	case "REVIEW":
		return 20
	}
	return 0
}

// inferWeightDecision 从 Hit.Decision 兜底；Evaluate 在 weight 未配时用。
func inferWeightDecision(d Decision) int {
	switch d {
	case Deny:
		return 50
	case Review:
		return 20
	}
	return 0
}

func mapScoreToVerdict(score int, t ScoreThresholds) Decision {
	if score >= t.DenyMin && t.DenyMin > 0 {
		return Deny
	}
	if score >= t.ReviewMin && t.ReviewMin > 0 {
		return Review
	}
	return Allow
}

func scoreLevel(score int) string {
	switch {
	case score >= 80:
		return "critical"
	case score >= 50:
		return "high"
	case score >= 20:
		return "medium"
	default:
		return "low"
	}
}

// SetScoreThresholds 热更新评分阈值。admin 端在 system_config 改完调用一下
// 即生效。reviewMin <= 0 / denyMin <= 0 退到默认值，避免误配把所有交易拒绝。
func (e *Engine) SetScoreThresholds(reviewMin, denyMin int) {
	t := defaultThresholds
	if reviewMin > 0 {
		t.ReviewMin = reviewMin
	}
	if denyMin > 0 {
		t.DenyMin = denyMin
	}
	e.mu.Lock()
	e.thresholds = t
	e.mu.Unlock()
	if e.logger != nil {
		e.logger.Info("score thresholds updated",
			zap.Int("review_min", t.ReviewMin),
			zap.Int("deny_min", t.DenyMin))
	}
}

// ScoreThresholds 当前生效的阈值（admin 查询用）。
func (e *Engine) ScoreThresholds() ScoreThresholds {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.thresholds
}

// Rules 返回当前规则快照（admin 用）
func (e *Engine) Rules() []Rule {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]Rule, len(e.rules))
	copy(out, e.rules)
	return out
}

// RuleDefs 返回最近 LoadRules 的原始 defs（深拷贝；caller 可任意 mutate）。
// admin /admin/rules/list 用：把 yaml-merged 的完整规则集 (id+name+type+
// config_json+mode+weight+rollout) 还原回前端，让运营在 UI 看到当前在跑什么。
func (e *Engine) RuleDefs() []RuleDef {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]RuleDef, len(e.defs))
	copy(out, e.defs)
	return out
}

// UpdateRule 单条规则原子热更新（替换 rules + defs 中同 id 的项）。
// 如果 id 不存在 → 视作新增。Schema 校验由 caller 跑（factory build 成功即合法）。
//
// 返回 true=更新；false=新增。
//
// **不**走版本化路径。带审计 / 版本记录的 admin 端口请用 UpdateRuleVersioned，
// 它会先 Record + Activate 到 VersionStore 再调本方法。
func (e *Engine) UpdateRule(d RuleDef, r Rule) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, existing := range e.defs {
		if existing.ID == d.ID {
			e.defs[i] = d
			e.rules[i] = r
			return true
		}
	}
	e.defs = append(e.defs, d)
	e.rules = append(e.rules, r)
	return false
}

// RemoveRule 按 id 删一条规则（admin disable / delete 用）。返回 true=找到并删；false=不存在。
func (e *Engine) RemoveRule(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, d := range e.defs {
		if d.ID != id {
			continue
		}
		e.defs = append(e.defs[:i], e.defs[i+1:]...)
		e.rules = append(e.rules[:i], e.rules[i+1:]...)
		return true
	}
	return false
}

// BuildRule 把一条 RuleDef 通过 factory 编译成 Rule，跑校验链（weighted 包装 +
// shadow 包装 + rollout 包装）。给 admin /admin/rules/update 在写入前预校验用。
// 失败返 error 让 admin 拒绝请求，规则不写进 engine。
func (e *Engine) BuildRule(d RuleDef) (Rule, error) {
	e.mu.RLock()
	f, ok := e.factories[d.Type]
	e.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown rule type %q", d.Type)
	}
	r, err := f(d.ID, d.Name, d.Enabled, d.ConfigJSON)
	if err != nil {
		return nil, fmt.Errorf("rule %q (%s) config invalid: %w", d.ID, d.Type, err)
	}
	w := d.Weight
	if w == 0 {
		w = inferWeight(d.Decision)
	}
	if w != 0 {
		r = &weightedRule{Rule: r, weight: w}
	}
	if strings.EqualFold(d.Mode, "shadow") {
		r = &shadowWrappedRule{Rule: r}
	}
	if d.Rollout.EnablePct > 0 && d.Rollout.EnablePct < 100 {
		r = &rolloutRule{Rule: r, ruleID: d.ID, rollout: d.Rollout}
	}
	return r, nil
}

// RuleCount 当前规则数
func (e *Engine) RuleCount() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.rules)
}

// SetRuleMode 热切换某条规则的 mode（enforce ↔ shadow）。
// 给"规则预测试 → 影子运行 N 天 → 切 enforce 上线"工作流用，不必走 reload 全量重建。
//
// shadow=true → 当前规则被 shadowWrappedRule 包一层（不影响 verdict）
// shadow=false → 解包返回原始规则
//
// 找不到规则 → 返 false。
func (e *Engine) SetRuleMode(ruleID string, shadow bool) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, r := range e.rules {
		if r.ID() != ruleID {
			continue
		}
		// 如果当前已经被 shadowWrappedRule 包，先解包到 inner
		if sw, ok := r.(*shadowWrappedRule); ok {
			r = sw.Rule
		}
		if shadow {
			e.rules[i] = &shadowWrappedRule{Rule: r}
		} else {
			e.rules[i] = r
		}
		return true
	}
	return false
}

// shadowWrappedRule 包装一条 Rule 把 Mode 强制成 ModeShadow。
// 同时透传 inner 的 Weight()（LoadRules 把 weighted 包装在 shadow 之内）。
type shadowWrappedRule struct {
	Rule
}

func (s *shadowWrappedRule) Mode() Mode { return ModeShadow }

func (s *shadowWrappedRule) Weight() int {
	if w, ok := s.Rule.(WeightedRule); ok {
		return w.Weight()
	}
	return 0
}

// WeightedRule 可选接口：实现它的规则可单独控制 weight。LoadRules 用
// weightedRule wrapper 把 RuleDef.Weight 注入；rules/* 包不必修改。
type WeightedRule interface {
	Weight() int
}

type weightedRule struct {
	Rule
	weight int
}

func (w *weightedRule) Weight() int { return w.weight }

// 透传 Mode：weightedRule 也可能被 shadowWrappedRule 包，但顺序是先 weighted
// 再 shadow，这里基本不会触发；实现作为 defensive 措施。
func (w *weightedRule) Mode() Mode {
	if m, ok := w.Rule.(ModedRule); ok {
		return m.Mode()
	}
	return ModeEnforce
}

// weightOf 取 rule 的 weight；不实现 WeightedRule → 0（让 Evaluate 退到
// 按 Decision 兜底）。
func weightOf(r Rule) int {
	if wr, ok := r.(WeightedRule); ok {
		return wr.Weight()
	}
	return 0
}

func merchantOf(txn *TxnContext) string {
	if txn == nil {
		return ""
	}
	return txn.MerchantID
}

func piMethodOf(txn *TxnContext) string {
	if txn == nil {
		return ""
	}
	return txn.PaymentMethod
}
