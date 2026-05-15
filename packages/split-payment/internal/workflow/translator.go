// translator.go — Money Flow Graph → accounting AtomicBatch 翻译器。
//
// 输入: Graph + 触发事件 (含 amount/currency/attributes)
// 输出: []Movement (实例化的资金移动), 可直接交给 accounting client 提交。
//
// 算法:
//   1. eval guards: 任何 guard 失败 → 整体拒绝
//   2. 拓扑排序节点: input → 其他
//   3. 渲染节点 account_id (替换 {placeholder})
//   4. 按 edge 顺序 (source 节点先, remainder 边最后) 算金额
//   5. percent: floor(amount * bp / 10000)
//      fixed_minor: 直接
//      remainder: 剩余兜底 (确保 sum == input)
//   6. placeholder 缺失:
//        if_missing=skip   → 跳过此边
//        if_missing=fail   → 报错
//        if_missing=default → 用 DefaultBeneficiary

package workflow

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"reconcile-system/packages/split-payment/internal/domain"
)

// TriggerContext 触发事件携带的上下文。
type TriggerContext struct {
	Event       string
	ChargeID    string
	MerchantID  string
	AmountMinor int64
	Currency    string
	Attributes  map[string]string // event payload merge — 用于 placeholder 替换 + guard eval
	TraceID     string
}

// Translate Graph + 事件上下文 → 一个 RunPlan (含 N 条 Movement)。
//
// 不真调 accounting; 那一步由调用方 (engine) 拿到 RunPlan 后调 AccountingClient.PostBatch。
// 这样 translator 100% 是纯函数, 单测覆盖容易。
func Translate(g *domain.Graph, tc TriggerContext) (*domain.RunPlan, error) {
	if g == nil {
		return nil, errors.New("nil graph")
	}
	spec := g.Spec

	// 1) 检查 guards
	for _, gd := range spec.Guards {
		if err := evalGuard(gd, tc); err != nil {
			return nil, fmt.Errorf("guard %s: %w", gd.Kind, err)
		}
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

	// 2) 索引节点 — 渲染 account_id (替换 placeholder)
	nodeAccount := map[string]string{} // node.id → rendered account_id
	skippedNodes := map[string]bool{}
	for _, n := range spec.Nodes {
		acc, skipped, err := renderAccount(n, tc.Attributes, tc.MerchantID)
		if err != nil {
			return nil, fmt.Errorf("node %s: %w", n.ID, err)
		}
		if skipped {
			skippedNodes[n.ID] = true
			continue
		}
		nodeAccount[n.ID] = acc
	}

	// 3) 拓扑排序 edges (remainder 永远最后, 否则按 input → output)
	edges := make([]domain.Edge, 0, len(spec.Edges))
	for _, e := range spec.Edges {
		edges = append(edges, e)
	}
	sort.SliceStable(edges, func(i, j int) bool {
		// remainder 排最后
		ri := edges[i].Rule.Type == "remainder"
		rj := edges[j].Rule.Type == "remainder"
		if ri != rj {
			return !ri // 非 remainder 在前
		}
		// 同类型按 from / to 字典序保稳定
		return edges[i].From+"->"+edges[i].To < edges[j].From+"->"+edges[j].To
	})

	// SP-3: 自动生成 transfer_group, 关联同一 charge 衍生的所有资金对象.
	plan.TransferGroup = genTransferGroup(tc.ChargeID)

	// 4) 按 edge 算金额 + 同时产出 typed Transfer/AppFee/Payout 对象 (SP-3).
	remaining := tc.AmountMinor
	movements := []domain.Movement{}
	transfers := []domain.Transfer{}
	appFees := []domain.ApplicationFee{}
	payouts := []domain.Payout{}
	now := time.Now().UTC()
	for _, e := range edges {
		// from / to 节点是否存在 (skipped 节点上的 edge 处理)
		fromAcc, fromOK := nodeAccount[e.From]
		toAcc, toOK := nodeAccount[e.To]
		if !fromOK || !toOK {
			// 任一端节点跳过 (placeholder 缺失)
			handle := e.Rule.IfMissing
			switch handle {
			case "skip", "":
				movements = append(movements, domain.Movement{
					EdgeFromNode: e.From, EdgeToNode: e.To,
					Status: "skipped", Reason: "node skipped (placeholder missing)",
				})
				continue
			case "fail":
				return nil, fmt.Errorf("edge %s→%s skipped node, rule=fail", e.From, e.To)
			case "default":
				if e.Rule.DefaultBeneficiary == "" {
					return nil, fmt.Errorf("edge %s→%s if_missing=default but no DefaultBeneficiary", e.From, e.To)
				}
				toAcc = e.Rule.DefaultBeneficiary
				if !fromOK {
					return nil, fmt.Errorf("edge %s→%s default only supports missing TO node", e.From, e.To)
				}
			}
		}

		// 算金额
		amount, err := computeAmount(e.Rule, tc.AmountMinor, remaining)
		if err != nil {
			return nil, fmt.Errorf("edge %s→%s: %w", e.From, e.To, err)
		}
		if amount <= 0 {
			movements = append(movements, domain.Movement{
				EdgeFromNode: e.From, EdgeToNode: e.To,
				FromAccount: fromAcc, ToAccount: toAcc,
				Status: "skipped", Reason: "amount <= 0",
			})
			continue
		}
		if amount > remaining {
			return nil, fmt.Errorf("edge %s→%s overflows source: want %d remain %d",
				e.From, e.To, amount, remaining)
		}
		movements = append(movements, domain.Movement{
			EdgeFromNode: e.From, EdgeToNode: e.To,
			FromAccount: fromAcc, ToAccount: toAcc,
			AmountMinor: amount, Status: "pending",
		})

		// SP-3: 按 edge.kind 产出 typed 对象, 跟 movement 一对一对应.
		switch e.ResolvedKind() {
		case domain.EdgeKindApplicationFee:
			appFees = append(appFees, domain.ApplicationFee{
				ID:             genID("fee"),
				Charge:         tc.ChargeID,
				Account:        toAcc, // 抽哪个商户的 fee
				AmountMinor:    amount,
				Currency:       tc.Currency,
				Status:         domain.AppFeeStatusPending,
				IdempotencyKey: genIdempotency(tc.ChargeID, e.From, e.To),
				CreatedAt:      now,
			})
		case domain.EdgeKindPayout:
			payouts = append(payouts, domain.Payout{
				ID:             genID("po"),
				Account:        fromAcc,
				AmountMinor:    amount,
				Currency:       tc.Currency,
				Method:         domain.PayoutMethodStandard,
				Status:         domain.PayoutStatusPending,
				IdempotencyKey: genIdempotency(tc.ChargeID, e.From, e.To),
				CreatedAt:      now,
			})
		default: // EdgeKindTransfer (含老 graph 没填 kind 的)
			// SP-3C: 跨币种支持 — 若 edge 指定 dest_currency 且不同于 tc.Currency,
			// 调 FX 换算 (caller 通过 ConvertAmount 注入 FXClient, translator 是纯函数
			// 不直接调 FX; 这里只透传字段, 让 engine 在执行前调换汇).
			finalAmount := amount
			finalCurrency := tc.Currency
			meta := map[string]string{}
			if e.DestCurrency != "" && e.DestCurrency != tc.Currency {
				// 不在 translator 里直接调 FX (保持纯函数), 留个标记给 engine 处理.
				finalCurrency = e.DestCurrency
				meta["fx_pending"] = tc.Currency + "->" + e.DestCurrency
				meta["fx_source_amount_minor"] = fmt.Sprintf("%d", amount)
			}
			transfers = append(transfers, domain.Transfer{
				ID:                 genID("tr"),
				TransferGroup:      plan.TransferGroup,
				SourceAccount:      fromAcc,
				DestinationAccount: toAcc,
				AmountMinor:        finalAmount,
				Currency:           finalCurrency,
				SourceTransaction:  tc.ChargeID,
				Status:             domain.TransferStatusCreated,
				IdempotencyKey:     genIdempotency(tc.ChargeID, e.From, e.To),
				Metadata:           meta,
				CreatedAt:          now,
			})
		}
		remaining -= amount
	}

	// 尾差归最后一条 pending movement
	if remaining > 0 {
		for i := len(movements) - 1; i >= 0; i-- {
			if movements[i].Status == "pending" {
				movements[i].AmountMinor += remaining
				break
			}
		}
	}

	plan.Movements = movements
	plan.Transfers = transfers
	plan.ApplicationFees = appFees
	plan.Payouts = payouts
	return plan, nil
}

// genTransferGroup tg_<chargeID 截断>_<rand4>; 同 charge 不同 run 拿不同 group.
//
// chargeID 空 (非 charge 事件触发, e.g. hold.expired) 时只用 rand 部分.
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

// genID 通用 ID 生成: <prefix>_<8 hex>.
func genID(prefix string) string {
	rb := make([]byte, 8)
	_, _ = rand.Read(rb)
	return prefix + "_" + hex.EncodeToString(rb)
}

// genIdempotency 同一 (charge, edge.from, edge.to) 复跑只产生一条记录.
//
// translator 是纯函数,每次调相同 charge+edge 产生相同 idempotency key,
// 给下游 repo 做 ON DUPLICATE KEY UPDATE 防重保护.
func genIdempotency(chargeID, from, to string) string {
	return chargeID + "::" + from + "->" + to
}

// ─── helpers ──────────────────────────────────────────────────────────

// renderAccount 把 node.AccountTemplate 里的 {xxx} 替换成 Attributes[xxx]; 缺失且 Optional → 跳过。
func renderAccount(n domain.Node, attrs map[string]string, merchantID string) (acc string, skipped bool, err error) {
	tmpl := n.AccountTemplate
	// 内置 {merchant_id} 自动注入
	tmpl = strings.ReplaceAll(tmpl, "{merchant_id}", merchantID)
	// 把所有剩余 {xxx} 都从 attrs 替换
	start := 0
	for {
		i := strings.Index(tmpl[start:], "{")
		if i < 0 {
			break
		}
		i += start
		j := strings.Index(tmpl[i:], "}")
		if j < 0 {
			break
		}
		j += i
		key := tmpl[i+1 : j]
		v, ok := attrs[key]
		if !ok || v == "" {
			if n.Optional {
				return "", true, nil
			}
			return "", false, fmt.Errorf("missing attribute %q for node %s", key, n.ID)
		}
		tmpl = tmpl[:i] + v + tmpl[j+1:]
		start = i + len(v)
	}
	return tmpl, false, nil
}

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

// evalGuard 简易 guard 评估 (Starlark 复杂表达式见 evalStarlarkGuard, 这里不强依赖)。
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
		// 真实生产应该调 user-merchant-core 查 — 这里假设 attribute 带了 merchant_status
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
