package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/xiongwp/risk-manage/internal/engine"
)

// ─── dsl: 商户运营自助规则 (无需写 Go + 部署) ────────────────────────
//
// 设计取舍：不上完整表达式语言 (CEL / expr / lua)：
//   - 满足 80% 运营需求：fact-based 一组条件 ANDed
//   - 零外部依赖 + < 1µs/condition 评估
//   - 配置直观，admin UI 直接生成 / 解析 JSON 不需要写 parser
//
// 配置：
//
//	type: dsl
//	config:
//	  decision:  review              # review / deny；默认 review
//	  weight:    25                  # 命中累加 score
//	  match_any: false               # true = OR；默认 false = AND
//	  conditions:
//	    - { field: amount,        op: gt,        value: 100000 }
//	    - { field: ip_country,    op: ne_field,  other: country }
//	    - { field: payment_method, op: in,       values: ["CARD","BANK_TRANSFER"] }
//	    - { field: ml_score,      op: gte,       value: 0.8 }
//
// 字段白名单 (Resolver 暴露)：
//   amount(int) currency country payment_method merchant_id customer_id
//   ip_address ip_country ip_proxy(bool) ip_vpn(bool) ip_data_center(bool)
//   ml_score(float) device_id user_agent platform language hardware_concurrency(int)
//   time_to_checkout_ms(int) keystroke_count(int)
//   metadata.*  (访问 metadata map：field="metadata.email_hash")
//
// 操作符：
//   eq | ne                                  ：两值相等
//   gt | gte | lt | lte                      ：数值 / 字符串字典序比较
//   in | not_in                              ：value 在 values 列表里
//   contains | not_contains                  ：字符串子串
//   prefix | suffix                          ：字符串前后缀
//   ne_field | eq_field                      ：跨字段比较（field vs other）
//   present | absent                         ：字段存在 / 缺失（空字符串视为缺失）

type DSLCondition struct {
	Field  string   `json:"field"`
	Op     string   `json:"op"`
	Value  any      `json:"value,omitempty"`  // 标量
	Values []any    `json:"values,omitempty"` // 列表 (in / not_in)
	Other  string   `json:"other,omitempty"`  // 跨字段比较的右侧字段名
}

type DSLConfig struct {
	Conditions []DSLCondition `json:"conditions"`
	MatchAny   bool           `json:"match_any"` // 默认 AND
	Decision   string         `json:"decision"`
	Weight     int            `json:"weight"`
}

type dslRule struct {
	id, name string
	enabled  bool
	cfg      DSLConfig
	verdict  engine.Decision
	weight   int
}

func DSLFactory() engine.RuleFactory {
	return func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		var cfg DSLConfig
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("dsl config: %w", err)
		}
		if len(cfg.Conditions) == 0 {
			return nil, fmt.Errorf("dsl: at least 1 condition required")
		}
		// 启动期校验所有 condition 的 field + op，避免运行时才发现 typo
		for i, c := range cfg.Conditions {
			if !validOp(c.Op) {
				return nil, fmt.Errorf("dsl condition %d: unknown op %q", i, c.Op)
			}
			if c.Field == "" {
				return nil, fmt.Errorf("dsl condition %d: field required", i)
			}
		}
		verdict := engine.Review
		if strings.EqualFold(cfg.Decision, "deny") {
			verdict = engine.Deny
		}
		return &dslRule{
			id: id, name: name, enabled: enabled, cfg: cfg,
			verdict: verdict, weight: cfg.Weight,
		}, nil
	}
}

func (r *dslRule) ID() string    { return r.id }
func (r *dslRule) Name() string  { return r.name }
func (r *dslRule) Type() string  { return "dsl" }
func (r *dslRule) Enabled() bool { return r.enabled }
func (r *dslRule) Weight() int   { return r.weight }

func (r *dslRule) Evaluate(_ context.Context, txn *engine.TxnContext) *engine.Hit {
	if txn == nil {
		return nil
	}
	hits := 0
	matched := make([]string, 0, len(r.cfg.Conditions))
	for _, c := range r.cfg.Conditions {
		ok := evalCondition(txn, c)
		if ok {
			hits++
			matched = append(matched, fmt.Sprintf("%s %s %v", c.Field, c.Op, condRHS(c)))
			if r.cfg.MatchAny {
				break
			}
		} else if !r.cfg.MatchAny {
			return nil // AND 模式，任一不满足 → 不命中
		}
	}
	// AND 模式到这里说明所有条件都过；OR 模式 hits>0 即命中
	if !r.cfg.MatchAny && hits != len(r.cfg.Conditions) {
		return nil
	}
	if r.cfg.MatchAny && hits == 0 {
		return nil
	}
	return &engine.Hit{
		RuleID:   r.id,
		RuleName: r.name,
		Decision: r.verdict,
		Detail:   "dsl: " + strings.Join(matched, "; "),
	}
}

func condRHS(c DSLCondition) any {
	if c.Other != "" {
		return "$" + c.Other
	}
	if len(c.Values) > 0 {
		return c.Values
	}
	return c.Value
}

// ── op evaluation ──────────────────────────────────────────────

func validOp(op string) bool {
	switch strings.ToLower(op) {
	case "eq", "ne", "gt", "gte", "lt", "lte",
		"in", "not_in",
		"contains", "not_contains",
		"prefix", "suffix",
		"eq_field", "ne_field",
		"present", "absent":
		return true
	}
	return false
}

func evalCondition(txn *engine.TxnContext, c DSLCondition) bool {
	left, leftPresent := readField(txn, c.Field)
	op := strings.ToLower(c.Op)

	switch op {
	case "present":
		return leftPresent
	case "absent":
		return !leftPresent
	}

	if !leftPresent {
		return false // 缺字段 → 多数 op 视为不命中
	}

	switch op {
	case "eq":
		return cmpEq(left, c.Value)
	case "ne":
		return !cmpEq(left, c.Value)
	case "gt", "gte", "lt", "lte":
		return cmpNum(left, c.Value, op)
	case "in":
		for _, v := range c.Values {
			if cmpEq(left, v) {
				return true
			}
		}
		return false
	case "not_in":
		for _, v := range c.Values {
			if cmpEq(left, v) {
				return false
			}
		}
		return true
	case "contains":
		return strings.Contains(toStr(left), toStr(c.Value))
	case "not_contains":
		return !strings.Contains(toStr(left), toStr(c.Value))
	case "prefix":
		return strings.HasPrefix(toStr(left), toStr(c.Value))
	case "suffix":
		return strings.HasSuffix(toStr(left), toStr(c.Value))
	case "eq_field":
		right, rOK := readField(txn, c.Other)
		return rOK && cmpEq(left, right)
	case "ne_field":
		right, rOK := readField(txn, c.Other)
		return !rOK || !cmpEq(left, right)
	}
	return false
}

// readField 把 c.Field 解析到 TxnContext 字段。返回 (value, present)。
// 空字符串视为 absent。metadata.* 走 map。
func readField(txn *engine.TxnContext, field string) (any, bool) {
	if strings.HasPrefix(field, "metadata.") {
		k := strings.TrimPrefix(field, "metadata.")
		v, ok := txn.Metadata[k]
		if !ok || v == "" {
			return nil, false
		}
		return v, true
	}
	switch field {
	case "amount":
		return txn.Amount, txn.Amount != 0
	case "currency":
		return txn.Currency, txn.Currency != ""
	case "country":
		return txn.Country, txn.Country != ""
	case "payment_method":
		return txn.PaymentMethod, txn.PaymentMethod != ""
	case "merchant_id":
		return txn.MerchantID, txn.MerchantID != ""
	case "customer_id":
		return txn.CustomerID, txn.CustomerID != ""
	case "ip_address":
		return txn.IPAddress, txn.IPAddress != ""
	case "ip_country":
		return txn.IPCountry, txn.IPCountry != ""
	case "ip_proxy":
		return txn.IPProxy, true
	case "ip_vpn":
		return txn.IPVPN, true
	case "ip_data_center":
		return txn.IPDataCenter, true
	case "ml_score":
		return txn.MLScore, true
	case "device_id":
		return txn.DeviceID, txn.DeviceID != ""
	case "user_agent":
		return txn.UserAgent, txn.UserAgent != ""
	case "platform":
		return txn.Platform, txn.Platform != ""
	case "language":
		return txn.Language, txn.Language != ""
	case "hardware_concurrency":
		return txn.HardwareConcurrency, true
	case "time_to_checkout_ms":
		return txn.TimeToCheckoutMs, true
	case "keystroke_count":
		return txn.KeystrokeCount, true
	}
	return nil, false
}

func cmpEq(a, b any) bool {
	if a == nil || b == nil {
		return a == b
	}
	// 数值统一转 float64 比较
	if af, aok := toFloat(a); aok {
		if bf, bok := toFloat(b); bok {
			return af == bf
		}
	}
	return toStr(a) == toStr(b)
}

func cmpNum(a, b any, op string) bool {
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok {
		switch op {
		case "gt":
			return af > bf
		case "gte":
			return af >= bf
		case "lt":
			return af < bf
		case "lte":
			return af <= bf
		}
		return false
	}
	// 字符串字典序兜底
	as, bs := toStr(a), toStr(b)
	switch op {
	case "gt":
		return as > bs
	case "gte":
		return as >= bs
	case "lt":
		return as < bs
	case "lte":
		return as <= bs
	}
	return false
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case float32:
		return float64(x), true
	case float64:
		return x, true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

func toStr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprintf("%v", x)
	}
}

// var _ = sort.Strings — 占位让 import 不被去掉（如果将来用 sorted helpers）
var _ = sort.Strings
