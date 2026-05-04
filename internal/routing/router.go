// Package routing 把 (country, payment_method, merchant_id) 路由到具体的
// payment-channel adapter。无状态，规则来自配置，支持按 merchant 覆盖。
package routing

import (
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
)

// Rule 一条路由规则。
// 匹配顺序：
//  1. Merchant != "" 且匹配当前 merchant → 更高优先级
//  2. Country / PaymentMethod / AmountMin|Max 匹配
//  3. 第一条命中即返回
type Rule struct {
	Priority      int    // 数值越小越优先
	Merchant      string // 商户 ID，为空表示通配
	Country       string // ISO2：PH / SG / ...，为空通配
	PaymentMethod string // GCASH / MAYA / ...，为空通配
	AmountMin     int64  // 0 = 无下限
	AmountMax     int64  // 0 = 无上限
	Adapter       string // 命中后路由到的 adapter 名
}

// compiledRule 是 Rule 的预归一化副本：所有大小写敏感字段在 ReplaceRules 时已
// 经 ToUpper 一次，热路径就不再 EqualFold 导致 allocation。
type compiledRule struct {
	rule          Rule
	merchantUpper string
	countryUpper  string
	methodUpper   string
}

func compile(rs []Rule) []compiledRule {
	out := make([]compiledRule, len(rs))
	for i, r := range rs {
		out[i] = compiledRule{
			rule:          r,
			merchantUpper: strings.ToUpper(r.Merchant),
			countryUpper:  strings.ToUpper(r.Country),
			methodUpper:   strings.ToUpper(r.PaymentMethod),
		}
	}
	return out
}

// MatchInput 路由入参
type MatchInput struct {
	Merchant      string
	Country       string
	PaymentMethod string
	Amount        int64
}

// Router 规则集。可以热加载（ReplaceRules 原子切换）。
//
// 热路径 Route() 用 atomic.Pointer 做 lock-free 读取；大部分 gRPC 请求不涉及
// 规则热更，RWMutex 的 RLock 虽然不阻塞读但仍然有 atomic RMW 成本。这里切到
// atomic.Pointer 之后，Route 只剩一个 Load + 循环比较，可以 inline。
type Router struct {
	rules atomic.Pointer[[]compiledRule]
}

func NewRouter(rules []Rule) *Router {
	r := &Router{}
	r.ReplaceRules(rules)
	return r
}

// ReplaceRules 原子替换规则集，自动排序。
func (r *Router) ReplaceRules(in []Rule) {
	cp := make([]Rule, len(in))
	copy(cp, in)
	// 排序：(merchant 非空优先) > priority asc > 具体度高的在前
	sort.SliceStable(cp, func(i, j int) bool {
		if (cp[i].Merchant != "") != (cp[j].Merchant != "") {
			return cp[i].Merchant != ""
		}
		if cp[i].Priority != cp[j].Priority {
			return cp[i].Priority < cp[j].Priority
		}
		return specificity(cp[i]) > specificity(cp[j])
	})
	compiled := compile(cp)
	r.rules.Store(&compiled)
}

// Route 返回首条命中的规则的 adapter 名；全都不匹配返回 ErrNoMatch。
func (r *Router) Route(in MatchInput) (string, error) {
	// 预归一化输入，避免每条规则比较时再调一次 ToUpper。
	merchantUp := strings.ToUpper(in.Merchant)
	countryUp := strings.ToUpper(in.Country)
	methodUp := strings.ToUpper(in.PaymentMethod)

	rules := r.rules.Load()
	if rules == nil {
		return "", fmt.Errorf("%w: router not initialized", ErrNoMatch)
	}
	for i := range *rules {
		cr := &(*rules)[i]
		if cr.merchantUpper != "" && cr.merchantUpper != merchantUp {
			continue
		}
		if cr.countryUpper != "" && cr.countryUpper != countryUp {
			continue
		}
		if cr.methodUpper != "" && cr.methodUpper != methodUp {
			continue
		}
		if cr.rule.AmountMin > 0 && in.Amount < cr.rule.AmountMin {
			continue
		}
		if cr.rule.AmountMax > 0 && in.Amount > cr.rule.AmountMax {
			continue
		}
		return cr.rule.Adapter, nil
	}
	return "", fmt.Errorf("%w: %+v", ErrNoMatch, in)
}

// Rules 返回当前规则集的副本（用于调试 / admin 接口）。
func (r *Router) Rules() []Rule {
	rules := r.rules.Load()
	if rules == nil {
		return nil
	}
	out := make([]Rule, len(*rules))
	for i := range *rules {
		out[i] = (*rules)[i].rule
	}
	return out
}

func specificity(r Rule) int {
	s := 0
	if r.Merchant != "" {
		s += 4
	}
	if r.Country != "" {
		s++
	}
	if r.PaymentMethod != "" {
		s++
	}
	if r.AmountMin > 0 {
		s++
	}
	if r.AmountMax > 0 {
		s++
	}
	return s
}

// ErrNoMatch 没有规则命中。
var ErrNoMatch = errNoMatch{}

type errNoMatch struct{}

func (errNoMatch) Error() string { return "no routing rule matched" }
