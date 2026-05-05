package replay

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strconv"
)

// Sanitizer 把 raw Event（含真实 PII）转成 shadow Event（脱敏 + 可整形）。
//
// 单 worker 部署：从 raw topic 拉 → 跑 Sanitize → 写 shadow topic。
// 失败的 event 写到 dead-letter topic 让 admin 后续核查。
type Sanitizer struct {
	// HMACKey 用于 deterministic-fake：同一真值永远映射到同一假值，
	// 保留聚合 pattern（同一 user 多次请求在压测中仍是"同一用户"）。
	// 32 字节随机，全平台一份，KMS 管理（不要 hard-code）。
	HMACKey []byte
	// PrefixForFakeUserID shadow 段位起点（默认 9e9 = ShadowFleetUserIDMin）。
	// 同 fake_user_id 落在 shadow 用户段，跟主流量永不撞。
	PrefixForFakeUserID int64
	// FieldRules 字段名 → 脱敏策略
	FieldRules map[string]Strategy
}

// Strategy 脱敏策略
type Strategy int

const (
	// StrategyKeep 保留原值（默认行为，安全字段如金额 / 币种 / 状态）
	StrategyKeep Strategy = iota
	// StrategyDrop 直接删除字段（cvv 等绝不能录入压测库）
	StrategyDrop
	// StrategyDeterministicFake hmac(real) → 截短 → 同字段类型的假值
	// 同一 real 永远同一 fake，跨录回放保持引用一致
	StrategyDeterministicFake
	// StrategyMaskPAN 卡号专用：保留 BIN(前6) + last4，中间打 *
	StrategyMaskPAN
	// StrategyShadowUserID user_id 专用：在 hmac 后加 shadow 段偏移
	StrategyShadowUserID
)

// DefaultFieldRules 默认脱敏规则。业务方可以覆盖 / 追加。
//
// 字段名匹配 protobuf snake_case（protojson 默认 lowerCamelCase，sanitizer 内部
// 会两种 case 都尝试）。
var DefaultFieldRules = map[string]Strategy{
	// 卡数据
	"card_number":     StrategyMaskPAN,
	"pan":             StrategyMaskPAN,
	"cvv":             StrategyDrop,
	"cvv2":            StrategyDrop,
	"expiry":          StrategyKeep, // 月份 / 年份不算 PII（不带 PAN 没法用）
	"track_data":      StrategyDrop,
	// PII
	"email":           StrategyDeterministicFake,
	"phone":           StrategyDeterministicFake,
	"phone_number":    StrategyDeterministicFake,
	"id_card_no":      StrategyDrop,
	"national_id":     StrategyDrop,
	"first_name":      StrategyDeterministicFake,
	"last_name":       StrategyDeterministicFake,
	"full_name":       StrategyDeterministicFake,
	"address":         StrategyDeterministicFake,
	// 业务 ID
	"user_id":         StrategyShadowUserID,
	"customer_id":     StrategyDeterministicFake,
	"merchant_id":     StrategyDeterministicFake,
	// 网络
	"ip_address":      StrategyDeterministicFake,
	"ip":              StrategyDeterministicFake,
	"device_id":       StrategyDeterministicFake,
}

// NewSanitizer 构造，HMACKey 必填（32 字节）；PrefixForFakeUserID=0 时默认 9e9。
func NewSanitizer(hmacKey []byte, rules map[string]Strategy) *Sanitizer {
	if rules == nil {
		// 拷贝一份避免被外部改默认
		rules = make(map[string]Strategy, len(DefaultFieldRules))
		for k, v := range DefaultFieldRules {
			rules[k] = v
		}
	}
	return &Sanitizer{
		HMACKey:             hmacKey,
		PrefixForFakeUserID: 9_000_000_000, // shadow.ShadowFleetUserIDMin
		FieldRules:          rules,
	}
}

// Sanitize 把一条 raw Event 脱敏成 shadow Event。
// 脱敏只动 BodyJSON 字段，Headers 已经在 capture 时白名单过滤过。
func (s *Sanitizer) Sanitize(ev *Event) (*Event, error) {
	var body map[string]any
	if err := json.Unmarshal(ev.BodyJSON, &body); err != nil {
		return nil, err
	}
	s.scrubObject(body)
	scrubbed, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return &Event{
		CapturedAt:  ev.CapturedAt,
		Method:      ev.Method,
		Headers:     ev.Headers,
		BodyJSON:    scrubbed,
		CaptureNode: ev.CaptureNode,
		SampleRate:  ev.SampleRate,
	}, nil
}

// scrubObject 递归走 map / array，对字段名命中 FieldRules 的应用策略。
// in-place 修改。
func (s *Sanitizer) scrubObject(v any) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			rule, hit := s.matchRule(k)
			if hit {
				x[k] = s.apply(rule, val)
				continue
			}
			s.scrubObject(val) // 没命中规则继续往下递归
		}
	case []any:
		for _, item := range x {
			s.scrubObject(item)
		}
	}
}

// matchRule 字段名匹配（snake_case + lowerCamelCase 都试）。
func (s *Sanitizer) matchRule(field string) (Strategy, bool) {
	if r, ok := s.FieldRules[field]; ok {
		return r, true
	}
	// camelCase → snake_case 兜底（protojson 默认 lowerCamelCase）。
	snake := camelToSnake(field)
	if r, ok := s.FieldRules[snake]; ok {
		return r, true
	}
	return 0, false
}

// apply 按策略改写字段值。
func (s *Sanitizer) apply(strat Strategy, val any) any {
	str, ok := val.(string)
	if !ok {
		// 数字 user_id 等
		if num, ok := val.(float64); ok && strat == StrategyShadowUserID {
			return s.shadowUserID(int64(num))
		}
		return val
	}
	switch strat {
	case StrategyKeep:
		return str
	case StrategyDrop:
		return nil // map 里 nil 会被 protojson 当作字段未设置
	case StrategyMaskPAN:
		return maskPAN(str)
	case StrategyDeterministicFake:
		return s.deterministicFake(str)
	case StrategyShadowUserID:
		if n, err := strconv.ParseInt(str, 10, 64); err == nil {
			return strconv.FormatInt(s.shadowUserID(n), 10)
		}
		return s.deterministicFake(str)
	}
	return str
}

// deterministicFake hmac(key, real) → hex 截短前 16 位。同 real → 同 fake。
func (s *Sanitizer) deterministicFake(real string) string {
	mac := hmac.New(sha256.New, s.HMACKey)
	mac.Write([]byte(real))
	return hex.EncodeToString(mac.Sum(nil))[:16]
}

// shadowUserID 真 user_id → 影子段假 user_id：hmac 取低 7 位 → + 9e9 段位起点。
// 落在 [9e9, 9e9 + 1e7) 区间，跟主流量 user_id 段（< 9e8）不撞。
func (s *Sanitizer) shadowUserID(real int64) int64 {
	mac := hmac.New(sha256.New, s.HMACKey)
	mac.Write([]byte(strconv.FormatInt(real, 10)))
	sum := mac.Sum(nil)
	hash := int64(sum[0])<<48 | int64(sum[1])<<40 | int64(sum[2])<<32 |
		int64(sum[3])<<24 | int64(sum[4])<<16 | int64(sum[5])<<8 | int64(sum[6])
	if hash < 0 {
		hash = -hash
	}
	return s.PrefixForFakeUserID + (hash % 10_000_000)
}

// maskPAN 保留 BIN(前 6) + last 4，中间长度对应位数 '*'。
//
//	"4111111111111111" → "411111******1111"
//	"345678901234567"  → "345678*****4567"
//	"1234"             → "1234"（太短不动）
func maskPAN(pan string) string {
	if len(pan) < 12 {
		return pan
	}
	mid := len(pan) - 10
	masked := make([]byte, len(pan))
	copy(masked, pan[:6])
	for i := 0; i < mid; i++ {
		masked[6+i] = '*'
	}
	copy(masked[6+mid:], pan[len(pan)-4:])
	return string(masked)
}

// camelToSnake "merchantId" → "merchant_id"
func camelToSnake(s string) string {
	var out []byte
	for i, c := range s {
		if i > 0 && c >= 'A' && c <= 'Z' {
			out = append(out, '_')
		}
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out = append(out, byte(c))
	}
	return string(out)
}

// panRegex 抓 12-19 位连续数字（PAN 长度范围）。
// 用来在自由文本字段（如 description / metadata）里找漏掉的 PAN。
var panRegex = regexp.MustCompile(`\b\d{12,19}\b`)

// ScrubFreeTextPAN 用正则在自由文本里找 PAN 并 mask。
// 对 description / note / merchant_metadata 之类 schema-less 字段兜底用。
func ScrubFreeTextPAN(s string) string {
	return panRegex.ReplaceAllStringFunc(s, func(m string) string {
		if !luhnValid(m) {
			return m
		}
		return maskPAN(m)
	})
}

// luhnValid 简单 Luhn 校验，避免把 12 位订单号也当 PAN mask 掉。
func luhnValid(s string) bool {
	if len(s) < 12 {
		return false
	}
	sum := 0
	odd := len(s) % 2
	for i, c := range s {
		if c < '0' || c > '9' {
			return false
		}
		d := int(c - '0')
		if i%2 == odd {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return sum%10 == 0
}
