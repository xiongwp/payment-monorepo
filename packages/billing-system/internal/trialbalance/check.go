// Package trialbalance — 日切借贷平衡校验 + 资金流向报告。
//
// 每日 23:55 跑（避开 0:00 跨日数据竞争）：
//
//   1. 拉今日所有 fee_event 按 event_type 聚合
//   2. 验证 借方 (charge) == 贷方 (refund + chargeback + adjustment)?
//      允许 settled diff（已结算到 statement 的不算 balance）
//   3. 算 net flow = sum(charge.amount) - sum(refund.amount) - sum(chargeback.amount)
//   4. 跟 statement 表当日 net_payout_minor 总和对比
//   5. 不一致 → 写 reconplatform diff (type=trial_balance_break, severity=P0)
//   6. 报告落 Redis: recon:billing:trial_balance:<date>
//   7. 通知 finance + payments oncall

package trialbalance

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Report 每日报告。
type Report struct {
	Date              string         `json:"date"`
	StartedAt         time.Time      `json:"started_at"`
	FinishedAt        time.Time      `json:"finished_at"`
	ByCurrency        []CurrencyView `json:"by_currency"`
	OverallBalanced   bool           `json:"overall_balanced"`
	BreaksCount       int            `json:"breaks_count"`
}

// CurrencyView 单币种维度。
type CurrencyView struct {
	Currency           string `json:"currency"`
	ChargeMinor        int64  `json:"charge_minor"`         // 借方 = 商户收入
	RefundMinor        int64  `json:"refund_minor"`         // 贷方
	ChargebackMinor    int64  `json:"chargeback_minor"`
	AdjustmentMinor    int64  `json:"adjustment_minor"`     // 正=补，负=扣
	FXSpreadMinor      int64  `json:"fx_spread_minor"`
	NetFlowMinor       int64  `json:"net_flow_minor"`       // = charge - refund - chargeback + adjustment
	StatementNetMinor  int64  `json:"statement_net_minor"`  // 当日 statement.net_payout_minor 总和
	DeltaMinor         int64  `json:"delta_minor"`          // net_flow - statement_net
	Balanced           bool   `json:"balanced"`             // |delta| <= tolerance
	EventCount         int    `json:"event_count"`
}

// Checker 借贷平衡检查器。
type Checker struct {
	db            *sql.DB
	log           *zap.Logger
	toleranceMinor int64           // 容忍差异（默认 0，浮点币种调 1）
	reconURL      string           // reconplatform admin 写 diff
	hc            *http.Client
}

func New(db *sql.DB, reconURL string, log *zap.Logger) *Checker {
	return &Checker{
		db: db, log: log,
		reconURL: reconURL,
		hc: &http.Client{Timeout: 10 * time.Second},
	}
}

// Run 跑某天的 trial balance + fund flow report。
func (c *Checker) Run(ctx context.Context, date time.Time) (*Report, error) {
	t0 := time.Now()
	dateStr := date.Format("2006-01-02")
	from := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 1)

	rep := &Report{Date: dateStr, StartedAt: t0}

	// 1. 拉 fee_event 按 (currency, event_type) 聚合
	const q = `SELECT currency, event_type,
		SUM(gross_amount_minor) AS gross_total,
		SUM(fee_minor) AS fee_total,
		COUNT(*) AS cnt
	FROM fee_event
	WHERE occurred_at >= ? AND occurred_at < ?
	GROUP BY currency, event_type`
	rows, err := c.db.QueryContext(ctx, q, from, to)
	if err != nil {
		return nil, fmt.Errorf("query fee_event: %w", err)
	}
	defer rows.Close()

	views := map[string]*CurrencyView{}
	for rows.Next() {
		var ccy, evtype string
		var gross, fee int64
		var cnt int
		if err := rows.Scan(&ccy, &evtype, &gross, &fee, &cnt); err != nil {
			return nil, err
		}
		v, ok := views[ccy]
		if !ok {
			v = &CurrencyView{Currency: ccy}
			views[ccy] = v
		}
		v.EventCount += cnt
		switch evtype {
		case "charge":
			v.ChargeMinor += gross
		case "refund":
			v.RefundMinor += gross
		case "chargeback":
			v.ChargebackMinor += gross
		case "fx_spread":
			v.FXSpreadMinor += fee
		}
	}

	// 2. adjustment 表
	const qAdj = `SELECT currency, SUM(amount_minor) FROM adjustment
		WHERE created_at >= ? AND created_at < ? GROUP BY currency`
	rowsA, err := c.db.QueryContext(ctx, qAdj, from, to)
	if err == nil {
		for rowsA.Next() {
			var ccy string
			var amt int64
			rowsA.Scan(&ccy, &amt)
			if v, ok := views[ccy]; ok {
				v.AdjustmentMinor = amt
			}
		}
		rowsA.Close()
	}

	// 3. statement 当日 net_payout 总和
	const qStmt = `SELECT currency, SUM(net_payout_minor) FROM statement
		WHERE period_start >= ? AND period_end <= ? GROUP BY currency`
	rowsS, err := c.db.QueryContext(ctx, qStmt, from, to)
	if err == nil {
		for rowsS.Next() {
			var ccy string
			var amt int64
			rowsS.Scan(&ccy, &amt)
			if v, ok := views[ccy]; ok {
				v.StatementNetMinor = amt
			}
		}
		rowsS.Close()
	}

	// 4. 计算 net_flow + delta，判断是否平衡
	for _, v := range views {
		v.NetFlowMinor = v.ChargeMinor - v.RefundMinor - v.ChargebackMinor + v.AdjustmentMinor
		v.DeltaMinor = v.NetFlowMinor - v.StatementNetMinor
		v.Balanced = absInt64(v.DeltaMinor) <= c.toleranceMinor
		if !v.Balanced {
			rep.BreaksCount++
		}
		rep.ByCurrency = append(rep.ByCurrency, *v)
	}
	rep.OverallBalanced = rep.BreaksCount == 0

	// 5. 不平衡 → 写 reconplatform diff
	for _, v := range rep.ByCurrency {
		if !v.Balanced {
			c.writeReconDiff(ctx, dateStr, v)
		}
	}

	rep.FinishedAt = time.Now()
	c.log.Info("trial balance complete",
		zap.String("date", dateStr),
		zap.Int("breaks", rep.BreaksCount),
		zap.Bool("balanced", rep.OverallBalanced),
		zap.Duration("dur", rep.FinishedAt.Sub(t0)))
	return rep, nil
}

// writeReconDiff 把不平衡写到 reconplatform 当 diff 处理。
func (c *Checker) writeReconDiff(ctx context.Context, date string, v CurrencyView) {
	if c.reconURL == "" {
		return
	}
	payload := map[string]any{
		"type":       "trial_balance_break",
		"severity":   "P0",
		"key":        date + ":" + v.Currency,
		"date":       date,
		"currency":   v.Currency,
		"detail":     v,
	}
	body, _ := json.Marshal(payload)
	url := strings.TrimRight(c.reconURL, "/") + "/api/v1/diffs/_external"
	req, _ := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		c.log.Warn("write recon diff failed", zap.Error(err))
		return
	}
	resp.Body.Close()
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
