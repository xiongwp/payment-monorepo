package threeds

import "time"

// AuthRequest 进 3DS 引擎前业务侧塞的上下文.
type AuthRequest struct {
	// 交易基本信息
	AmountCents int64     `json:"amount_cents"`
	Currency    string    `json:"currency"` // ISO 4217
	IsRecurring bool      `json:"is_recurring"`  // MIT=true
	IsFirstMIT  bool      `json:"is_first_mit"`  // 续费第一次需要 SCA 拿凭证, 之后免
	CardScheme  string    `json:"card_scheme"`   // visa / mastercard / amex
	BIN         string    `json:"bin"`           // 决定 issuer 国家 (EEA 才适用 PSD2)
	MerchantID  string    `json:"merchant_id"`
	IssuerCountry string  `json:"issuer_country"` // ISO-2; 关键 — 非 EEA 卡不受 PSD2 约束

	// 风控信号
	RiskScore     int  `json:"risk_score"`      // 0-100, 来自 risk-manage
	IsTrusted     bool `json:"is_trusted"`      // 持卡人加白名单
	IsB2B         bool `json:"is_b2b"`          // 企业卡
	IsHotelNoShow bool `json:"is_hotel_noshow"`

	// 商户 fraud rate (acquirer 算; 我们 OK 按 channel 给 monthly 数)
	AcquirerFraudRate float64 `json:"acquirer_fraud_rate"` // 0.0013 = 0.13%

	// Low-value 计数器 (按卡 30d 滚动; vault / card-center 维护)
	CardLowValueCount  int   `json:"card_low_value_count"`  // 已用免 SCA 次数
	CardLowValueAmount int64 `json:"card_low_value_amount"` // 已累计 (分)
}

// Decision 决策结果
type Decision struct {
	Action         Action     `json:"action"`              // frictionless / challenge / nochallenge_noth_3ds
	ExemptionUsed  Exemption  `json:"exemption_used,omitempty"`
	ECI            string     `json:"eci"`                 // 推荐填给卡组
	Reason         string     `json:"reason"`              // 给 ops 日志 / 拒绝 trace
	LiabilityShift bool       `json:"liability_shift"`     // chargeback 是否能 push to issuer
	DecidedAt      time.Time  `json:"decided_at"`
}

// Action
type Action string

const (
	ActionFrictionless    Action = "frictionless"      // 不弹 challenge, 直接送 cleartext
	ActionChallenge       Action = "challenge"         // 弹 OTP / biometric
	ActionNonThreeDS      Action = "non_3ds"           // 不走 3DS (非 EEA 卡)
)

// Exemption 命中哪个 exemption
type Exemption string

const (
	ExemptionNone        Exemption = ""
	ExemptionTRA         Exemption = "tra"          // Transaction Risk Analysis
	ExemptionLowValue    Exemption = "low_value"    // Article 16
	ExemptionMITRecurring Exemption = "mit"          // Article 14
	ExemptionTrusted     Exemption = "trusted"      // Article 13
	ExemptionB2B         Exemption = "b2b"          // Article 17
	ExemptionNoShow      Exemption = "no_show"
)

// TRA 阈值表 (€) — 跟 PSD2 EBA RTS 一致
//
// 商户 fraud rate 必须 ≤ 阈值 才能用对应金额段 TRA:
//   ≤ €100   要 fraud rate ≤ 0.13%
//   ≤ €250   要 ≤ 0.06%
//   ≤ €500   要 ≤ 0.01%
type TRAThreshold struct {
	MaxAmountCents  int64
	MaxFraudRate    float64
}

var defaultTRAThresholds = []TRAThreshold{
	{100_00, 0.0013},   // €100, 0.13%
	{250_00, 0.0006},   // €250, 0.06%
	{500_00, 0.0001},   // €500, 0.01%
}

// LowValueThresholds — Article 16
const (
	LowValueSingleMaxCents     int64 = 30_00       // 单笔 €30
	LowValueRolling30dMaxCount int   = 5           // 30d 内最多 5 次
	LowValueRolling30dMaxCents int64 = 100_00      // 30d 内累计 €100
)

// EEACountries — PSD2 仅约束 EEA + Iceland/Liechtenstein/Norway/Switzerland-ish.
// 卡 issuer 在 EEA + 商户 acquirer 在 EEA → "two-leg" 强制 SCA.
var eeaCountries = map[string]bool{
	"AT": true, "BE": true, "BG": true, "HR": true, "CY": true, "CZ": true,
	"DK": true, "EE": true, "FI": true, "FR": true, "DE": true, "GR": true,
	"HU": true, "IE": true, "IT": true, "LV": true, "LT": true, "LU": true,
	"MT": true, "NL": true, "PL": true, "PT": true, "RO": true, "SK": true,
	"SI": true, "ES": true, "SE": true,
	"IS": true, "LI": true, "NO": true,
}

// Decide 主判定. 命中任一 exemption → frictionless; 否则 challenge.
//
// 排序 (业内通行):
//   1. non-EEA → 不走 3DS (但商户可主动要求 3DS for liability shift)
//   2. Trusted beneficiary
//   3. MIT (recurring, 非第一次)
//   4. B2B
//   5. Low value
//   6. TRA (最常见 frictionless path)
//   → 都不命中 → challenge
func Decide(req AuthRequest) Decision {
	now := time.Now().UTC()

	// 1) 卡 issuer 不在 EEA → 不强制 3DS (无 PSD2 约束)
	if !eeaCountries[req.IssuerCountry] {
		return Decision{
			Action:         ActionNonThreeDS,
			ECI:            nonThreeDSECI(req.CardScheme),
			Reason:         "issuer_country_outside_EEA",
			LiabilityShift: false,
			DecidedAt:      now,
		}
	}

	// 2) 持卡人加白
	if req.IsTrusted {
		return decision(ExemptionTrusted, "cardholder_whitelisted", true, req.CardScheme, now)
	}

	// 3) MIT (subsequent recurring) — 第一次必须 SCA
	if req.IsRecurring && !req.IsFirstMIT {
		return decision(ExemptionMITRecurring, "subsequent_mit_with_stored_cred", true, req.CardScheme, now)
	}

	// 4) B2B
	if req.IsB2B {
		return decision(ExemptionB2B, "corporate_secure_process", false, req.CardScheme, now)
	}

	// 5) no-show
	if req.IsHotelNoShow {
		return decision(ExemptionNoShow, "hotel_or_rental_noshow", false, req.CardScheme, now)
	}

	// 6) low value
	if req.AmountCents <= LowValueSingleMaxCents &&
		req.CardLowValueCount < LowValueRolling30dMaxCount &&
		req.CardLowValueAmount+req.AmountCents <= LowValueRolling30dMaxCents {
		return decision(ExemptionLowValue, "amount_below_30_eur", false, req.CardScheme, now)
	}

	// 7) TRA — 看金额段 + acquirer fraud rate
	for _, th := range defaultTRAThresholds {
		if req.AmountCents > th.MaxAmountCents {
			continue
		}
		if req.AcquirerFraudRate > th.MaxFraudRate {
			continue
		}
		// risk-manage 给的 score 必须低于 cutoff (i.e. low risk)
		// 不严格 EBA 要求, 但工业实践 — score>50 一律 challenge
		if req.RiskScore > 50 {
			continue
		}
		return decision(ExemptionTRA, "tra_passed", false, req.CardScheme, now)
	}

	// → 默认 challenge
	return Decision{
		Action:         ActionChallenge,
		ECI:            "",  // challenge 完了由 ACS 返回 ECI
		Reason:         "no_exemption_matched",
		LiabilityShift: true,  // 走完 challenge 一定有 shift
		DecidedAt:      now,
	}
}

func decision(ex Exemption, reason string, shift bool, scheme string, t time.Time) Decision {
	return Decision{
		Action:         ActionFrictionless,
		ExemptionUsed:  ex,
		ECI:            frictionlessECI(scheme),
		Reason:         reason,
		LiabilityShift: shift,
		DecidedAt:      t,
	}
}

func frictionlessECI(scheme string) string {
	switch scheme {
	case "visa", "amex":
		return "06" // attempted / frictionless
	case "mastercard":
		return "01"
	}
	return "06"
}

func nonThreeDSECI(scheme string) string {
	switch scheme {
	case "visa", "amex":
		return "07"
	case "mastercard":
		return "00"
	}
	return "07"
}
