// translator.go — Money Flow Graph → accounting CreateTransaction 入参翻译器.
//
// SP-AC-7 重构: multi-leg per rule.
//
// 核心语义:
//   - 一个 graph 触发后 → 由 N 个 edge 组成
//   - edge 按 event_code 分组 → 每组 = 一条 accounting TransactionRule = 一次原子操作
//   - 每组转 1 个 TransactionRequest, 其中 Legs[] 持本组所有 edge 的资金流
//   - engine 调 accounting.CreateTransaction 一组一调, 上游一次原子落账
//
// 数据流:
//   caller.attributes  →  Node.AccountIDAttr/AmountAttr/CurrencyAttr  →  TxnLeg(account_id/amount/currency)
//                                                                           ↓
//                              event_code 分组聚合 → TransactionRequest{Legs}
//                                                                           ↓
//                              engine → accounting.CreateTransaction (一次)

package workflow

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"reconcile-system/packages/split-payment/internal/domain"
)

// TriggerContext 触发事件携带的上下文.
//
// Attributes 是 K-V 字典. 对于图里每个 Node, caller 至少要提供:
//   - attributes[node.AccountIDAttr]               = 账户 ID
//   - attributes[node.AmountAttr]  (or 默认 _amount)   = 金额 (minor units)
//   - attributes[node.CurrencyAttr] (or 默认 _currency) = 币种
//
// 平台固定户 (node.AccountIDAttr 空) 不需要 caller 提供, translator 用 node.ID 兜底.
type TriggerContext struct {
	Event       string
	ChargeID    string
	MerchantID  string
	AmountMinor int64
	Currency    string
	Attributes  map[string]string
	TraceID     string
}

// Translate Graph + 事件上下文 → 一个 RunPlan (含 N 条 multi-leg TransactionRequest).
//
// 错误条件:
//   - guards 任一失败
//   - spec.scenario 空 (= 没绑 product_code, 无法路由到 rule)
//   - 任一 edge 没配 event_code
//   - 任一 edge 配了 AccountIDAttr 但 event.attributes 取不到值
//   - 同一 event_code 组内币种不一致 (accounting 不支持跨币种原子操作)
func Translate(g *domain.Graph, tc TriggerContext) (*domain.RunPlan, error) {
	if g == nil {
		return nil, errors.New("nil graph")
	}
	spec := g.Spec

	// 1) guards
	for _, gd := range spec.Guards {
		if err := evalGuard(gd, tc); err != nil {
			return nil, fmt.Errorf("guard %s: %w", gd.Kind, err)
		}
	}

	if spec.Scenario == "" {
		return nil, errors.New("graph.spec.scenario required (= accounting product_code)")
	}

	plan := &domain.RunPlan{
		GraphID:      g.ID,
		GraphVersion: g.Version,
		TriggerEvent: tc.Event,
		ChargeID:     tc.ChargeID,
		MerchantID:   tc.MerchantID,
		AmountMinor:  tc.AmountMinor,
		Currency:     tc.Currency,
		Attributes:   tc.Attributes,
		Status:       "created",
		TraceID:      tc.TraceID,
	}
	plan.TransferGroup = genTransferGroup(tc.ChargeID)

	// 2) 按 event_code 分组 edges, 同组内 remainder 排最后
	groups, groupOrder, err := groupEdgesByEventCode(spec.Edges)
	if err != nil {
		return nil, err
	}

	// 3) 每组生成一个 TransactionRequest (multi-leg)
	movements := []domain.Movement{}
	transactions := []domain.TransactionRequest{}
	now := time.Now().UTC()

	for _, evCode := range groupOrder {
		edges := groups[evCode]

		legs := []domain.TxnLeg{}
		remaining := tc.AmountMinor
		groupCurrency := ""
		for _, e := range edges {
			fromNode := findNode(spec.Nodes, e.From)
			toNode := findNode(spec.Nodes, e.To)

			fromAccID, err := resolveAccountID(fromNode, tc)
			if err != nil {
				return nil, fmt.Errorf("edge %s→%s from: %w", e.From, e.To, err)
			}
			toAccID, err := resolveAccountID(toNode, tc)
			if err != nil {
				return nil, fmt.Errorf("edge %s→%s to: %w", e.From, e.To, err)
			}

			amount, currency, src, err := resolveAmountCurrency(toNode, fromNode, e, tc, remaining)
			if err != nil {
				return nil, fmt.Errorf("edge %s→%s: %w", e.From, e.To, err)
			}
			if amount <= 0 {
				movements = append(movements, domain.Movement{
					EdgeFromNode: e.From, EdgeToNode: e.To,
					FromAccount: fromAccID, ToAccount: toAccID,
					Status: "skipped", Reason: "amount <= 0",
				})
				continue
			}
			if src == "rule" && amount > remaining {
				return nil, fmt.Errorf("edge %s→%s overflows source: want %d remain %d",
					e.From, e.To, amount, remaining)
			}
			// 同组内币种检查
			if groupCurrency == "" {
				groupCurrency = currency
			} else if groupCurrency != currency {
				return nil, fmt.Errorf("event_code=%q mixes currencies %q vs %q (rule must be single-currency)",
					evCode, groupCurrency, currency)
			}

			legs = append(legs, domain.TxnLeg{
				EdgeFromNode:  e.From,
				EdgeToNode:    e.To,
				FromAccountID: fromAccID,
				ToAccountID:   toAccID,
				Amount:        fmt.Sprintf("%d", amount),
				Currency:      currency,
			})
			movements = append(movements, domain.Movement{
				EdgeFromNode: e.From, EdgeToNode: e.To,
				FromAccount: fromAccID, ToAccount: toAccID,
				AmountMinor: amount, Status: "pending",
			})
			if src == "rule" {
				remaining -= amount
			}
		}

		// 尾差归本组最后一 leg (防 percent rounding 丢钱)
		if remaining > 0 && len(legs) > 0 {
			last := &legs[len(legs)-1]
			var lastAmt int64
			_, _ = fmt.Sscanf(last.Amount, "%d", &lastAmt)
			lastAmt += remaining
			last.Amount = fmt.Sprintf("%d", lastAmt)
			// 同步 movement
			for i := len(movements) - 1; i >= 0; i-- {
				if movements[i].EdgeFromNode == last.EdgeFromNode &&
					movements[i].EdgeToNode == last.EdgeToNode {
					movements[i].AmountMinor = lastAmt
					break
				}
			}
		}

		if len(legs) == 0 {
			continue
		}
		transactions = append(transactions, domain.TransactionRequest{
			OrderNo:      fmt.Sprintf("%s_%s", tc.ChargeID, evCode),
			BusinessNo:   tc.ChargeID,
			BusinessType: spec.Scenario,
			ProductCode:  spec.Scenario,
			EventCode:    evCode,
			Legs:         legs,
			TraceID:      tc.TraceID,
			Status:       "pending",
			CreatedAt:    now,
		})
	}

	plan.Movements = movements
	plan.Transactions = transactions
	return plan, nil
}

// groupEdgesByEventCode 按 event_code 分组, 同组内 remainder 排到最后.
//
// 返回:
//   groups[event_code] = []edge (排好序)
//   groupOrder         = []event_code (按出现顺序稳定输出, 便于测试断言)
//
// 错: edge 没配 event_code → error.
func groupEdgesByEventCode(edges []domain.Edge) (map[string][]domain.Edge, []string, error) {
	groups := map[string][]domain.Edge{}
	order := []string{}
	for _, e := range edges {
		if e.EventCode == "" {
			return nil, nil, fmt.Errorf("edge %s→%s missing event_code", e.From, e.To)
		}
		if _, ok := groups[e.EventCode]; !ok {
			order = append(order, e.EventCode)
		}
		groups[e.EventCode] = append(groups[e.EventCode], e)
	}
	// 同组内: remainder 永远最后
	for k := range groups {
		es := groups[k]
		sort.SliceStable(es, func(i, j int) bool {
			ri := es[i].Rule.Type == "remainder"
			rj := es[j].Rule.Type == "remainder"
			if ri != rj {
				return !ri
			}
			return es[i].From+"->"+es[i].To < es[j].From+"->"+es[j].To
		})
		groups[k] = es
	}
	return groups, order, nil
}

// resolveAccountID 把 Node 解析成 account_id 字符串.
//
// 规则:
//   - AccountIDAttr 非空 → attributes[AccountIDAttr]
//   - AccountIDAttr 空    → 用 node.ID 兜底 (平台固定户, 全图唯一)
//   - 配了 AccountIDAttr 但 attributes 里没值 → 错 (caller 责任传齐)
func resolveAccountID(n domain.Node, tc TriggerContext) (string, error) {
	if n.AccountIDAttr == "" {
		return n.ID, nil
	}
	v, ok := tc.Attributes[n.AccountIDAttr]
	if !ok || v == "" {
		return "", fmt.Errorf("attributes[%q] missing for node %q", n.AccountIDAttr, n.ID)
	}
	return v, nil
}

// resolveAmountCurrency 解析 edge 的 amount + currency.
//
// 优先级:
//   1. event-driven: TO-node 的 AmountAttr / CurrencyAttr (或默认 convention)
//   2. rule-driven: EdgeRule.computeAmount + tc.Currency
//
// 返回 (amount, currency, source). source ∈ {"event", "rule"}.
func resolveAmountCurrency(toNode, fromNode domain.Node, e domain.Edge, tc TriggerContext, remaining int64) (int64, string, string, error) {
	amountKey := toNode.AmountAttr
	if amountKey == "" && toNode.AccountIDAttr != "" {
		amountKey = toNode.AccountIDAttr + "_amount"
	}
	currencyKey := toNode.CurrencyAttr
	if currencyKey == "" && toNode.AccountIDAttr != "" {
		currencyKey = toNode.AccountIDAttr + "_currency"
	}
	if amountKey != "" {
		if v, ok := tc.Attributes[amountKey]; ok && v != "" {
			var amount int64
			if _, err := fmt.Sscanf(v, "%d", &amount); err != nil {
				return 0, "", "", fmt.Errorf("attributes[%q]=%q not int", amountKey, v)
			}
			currency := tc.Currency
			if v2, ok := tc.Attributes[currencyKey]; ok && v2 != "" {
				currency = v2
			}
			return amount, currency, "event", nil
		}
	}
	amount, err := computeAmount(e.Rule, tc.AmountMinor, remaining)
	if err != nil {
		return 0, "", "", err
	}
	return amount, tc.Currency, "rule", nil
}

// findNode 按 ID 找节点 (没找到返空 Node, 由 caller 处理).
func findNode(nodes []domain.Node, id string) domain.Node {
	for _, n := range nodes {
		if n.ID == id {
			return n
		}
	}
	return domain.Node{}
}

// genTransferGroup tg_<chargeID 截断>_<rand4>; 同 charge 不同 run 拿不同 group.
func genTransferGroup(chargeID string) string {
	rb := make([]byte, 4)
	_, _ = rand.Read(rb)
	suf := hex.EncodeToString(rb)
	if chargeID == "" {
		return "tg_" + suf
	}
	prefix := chargeID
	if len(prefix) > 12 {
		prefix = prefix[:12]
	}
	return "tg_" + prefix + "_" + suf
}

// computeAmount 按 rule 算金额.
func computeAmount(r domain.EdgeRule, total, remaining int64) (int64, error) {
	var amount int64
	switch r.Type {
	case "percent":
		amount = total * r.Value / 10000
	case "fixed_minor":
		amount = r.Value
	case "remainder":
		amount = remaining
	default:
		return 0, fmt.Errorf("unknown rule type %q", r.Type)
	}
	if r.MaxAmount > 0 && amount > r.MaxAmount {
		amount = r.MaxAmount
	}
	if amount < r.MinAmount {
		amount = r.MinAmount
	}
	return amount, nil
}

// evalGuard 简易 guard 评估.
func evalGuard(g domain.Guard, tc TriggerContext) error {
	switch g.Kind {
	case "amount_min":
		v, _ := g.Value.(float64)
		if tc.AmountMinor < int64(v) {
			return errors.New(orDefault(g.Msg, "amount below minimum"))
		}
	case "amount_max":
		v, _ := g.Value.(float64)
		if tc.AmountMinor > int64(v) {
			return errors.New(orDefault(g.Msg, "amount exceeds maximum"))
		}
	case "currency_in":
		list, _ := g.Value.([]any)
		ok := false
		for _, x := range list {
			if s, _ := x.(string); s == tc.Currency {
				ok = true
				break
			}
		}
		if !ok {
			return errors.New(orDefault(g.Msg, "currency not allowed"))
		}
	case "merchant_active":
		if v := tc.Attributes["merchant_status"]; v != "" && v != "active" {
			return errors.New(orDefault(g.Msg, "merchant not active"))
		}
	}
	return nil
}

func orDefault(s, d string) string {
	if s != "" {
		return s
	}
	return d
}
