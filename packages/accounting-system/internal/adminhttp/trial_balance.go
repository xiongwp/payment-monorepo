package adminhttp

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"sort"
	"strconv"

	"github.com/xiongwp/accounting-system/internal/service"
)

// trial_balance.go 提供试算平衡的 3 个只读 admin HTTP 端点：
//
//	GET /admin/trial-balance/live       实时试算（不依赖 day-cut snapshot）
//	GET /admin/trial-balance/drilldown  第 4 层下钻到具体 account_no
//	GET /admin/trial-balance/export     CSV 导出（金额从 minor×100 转成可读）
//
// 走 admin HTTP，不动 gRPC proto / kitex_gen。

// handleTrialBalanceSnapshot GET /admin/trial-balance/snapshot?currency=PHP&snapshot_date=YYYY-MM-DD&run_id=N
//
// 走 admin HTTP 而非 gRPC 是为了让 CategorySummary.BusinessType / Level 这些 proto
// 里没定义的字段直接 JSON 序列化回前端 —— 分类明细需要按业务类型摊开。
func (s *Server) handleTrialBalanceSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.trialBalanceSvc == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "trial balance service not wired"})
		return
	}
	q := r.URL.Query()
	currency := q.Get("currency")
	if currency == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "currency is required"})
		return
	}
	snapshotDate := q.Get("snapshot_date")
	if snapshotDate == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "snapshot_date is required"})
		return
	}
	runID := atoiDefault(q.Get("run_id"), 0)

	result, err := s.trialBalanceSvc.RunTrialBalanceByCurrency(r.Context(), snapshotDate, currency, runID)
	if err != nil {
		// 部分分片失败时 result 仍带部分数据;返回 200 + warning 字段更利于运维查看。
		if result != nil {
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"currency": currency,
				"result":   result,
				"warning":  err.Error(),
			})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"currency": currency,
		"result":   result,
	})
}

// handleTrialBalanceDates GET /admin/trial-balance/dates
// 返回 {dates: ["2026-05-28", ...]}, 按 DESC 排。
func (s *Server) handleTrialBalanceDates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.trialBalanceSvc == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "trial balance service not wired"})
		return
	}
	dates, err := s.trialBalanceSvc.ListSnapshotDates(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"dates": dates})
}

// handleTrialBalanceLive GET /admin/trial-balance/live?currency=PHP
func (s *Server) handleTrialBalanceLive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.trialBalanceSvc == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "trial balance service not wired"})
		return
	}
	currency := r.URL.Query().Get("currency")
	if currency == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "currency is required"})
		return
	}
	result, err := s.trialBalanceSvc.RunLiveTrialBalance(r.Context(), currency)
	if err != nil {
		// 部分分片失败时 result 仍带部分数据；返回 200 + warning 字段更利于运维查看。
		if result != nil {
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"currency": currency,
				"result":   result,
				"warning":  err.Error(),
			})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"currency": currency,
		"result":   result,
	})
}

// handleTrialBalanceDrilldown GET /admin/trial-balance/drilldown
//
//	?currency=PHP&category=ASSET&account_type=1&business_type=1&snapshot_date=&run_id=0
//
// snapshot_date 空 → 查 live；非空 → 查该日 snapshot。category/account_type/business_type
// 任意组合过滤（缺省 / 0 表示不过滤）。返回 account 级明细，已按 |balance| 降序。
func (s *Server) handleTrialBalanceDrilldown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.trialBalanceSvc == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "trial balance service not wired"})
		return
	}
	q := r.URL.Query()
	currency := q.Get("currency")
	if currency == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "currency is required"})
		return
	}
	category := q.Get("category")
	accountType := atoiDefault(q.Get("account_type"), 0)
	businessType := atoiDefault(q.Get("business_type"), 0)
	snapshotDate := q.Get("snapshot_date")
	runID := atoiDefault(q.Get("run_id"), 0)

	details, err := s.trialBalanceSvc.Drilldown(r.Context(), currency, category, accountType, businessType, snapshotDate, runID)
	if err != nil && len(details) == 0 {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// 该层小计（对返回明细求和）。
	var subtotalBalance, subtotalEnding int64
	for _, d := range details {
		subtotalBalance += d.Balance
		subtotalEnding += d.Ending
	}

	resp := map[string]interface{}{
		"currency":         currency,
		"category":         category,
		"account_type":     accountType,
		"business_type":    businessType,
		"snapshot_date":    snapshotDate,
		"run_id":           runID,
		"count":            len(details),
		"subtotal_balance": subtotalBalance,
		"subtotal_ending":  subtotalEnding,
		"accounts":         details,
	}
	if err != nil {
		resp["warning"] = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleTrialBalanceExport GET /admin/trial-balance/export?currency=PHP&snapshot_date=2026-05-28&run_id=N
//
// CSV 列：科目类别,账户类型,业务类型,账户数,期初余额,本期借方,本期贷方,期末余额
// 金额从 minor×100 转成可读（除 100，2 位小数）。
// snapshot_date 空 → 导出 live 试算（无期初/借贷流水，相应列为 0.00）。
func (s *Server) handleTrialBalanceExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.trialBalanceSvc == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "trial balance service not wired"})
		return
	}
	q := r.URL.Query()
	currency := q.Get("currency")
	if currency == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "currency is required"})
		return
	}
	snapshotDate := q.Get("snapshot_date")
	runID := atoiDefault(q.Get("run_id"), 0)

	rows, filename, err := s.buildTrialBalanceCSVRows(r, currency, snapshotDate, runID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.WriteHeader(http.StatusOK)

	// UTF-8 BOM：Excel 默认按 GBK 打开会乱码，BOM 让其识别 UTF-8。
	_, _ = w.Write([]byte{0xEF, 0xBB, 0xBF})

	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"科目类别", "账户类型", "业务类型", "账户数", "期初余额", "本期借方", "本期贷方", "期末余额"})
	for _, row := range rows {
		_ = cw.Write(row)
	}
	cw.Flush()
}

// csvExportRow 聚合到 (category, type, business_type) 三层的一行导出数据。
type csvExportRow struct {
	category     string
	accountType  int
	businessType int
	accountCount int64
	beginning    int64
	debit        int64
	credit       int64
	ending       int64
}

// buildTrialBalanceCSVRows 用 Drilldown（返回 account_no + business_type）跨分片拉明细，
// 在内存按 (category, type, business_type) 三层聚合成导出行。
//
// 这样 CSV 才能带 business_type 这一列 —— 现有 RunTrialBalanceByCurrency 只聚合到
// (category, type) 两层，拿不到 business_type。
func (s *Server) buildTrialBalanceCSVRows(r *http.Request, currency, snapshotDate string, runID int) ([][]string, string, error) {
	// category/type/businessType 全部不过滤 → 拉全量明细。
	details, err := s.trialBalanceSvc.Drilldown(r.Context(), currency, "", 0, 0, snapshotDate, runID)
	if err != nil && len(details) == 0 {
		return nil, "", err
	}

	type aggKey struct {
		typ     int
		bizType int
	}
	// 用 service.CategoryForAccountType 从 account_type 推导 category（权威映射）。
	agg := make(map[aggKey]*csvExportRow)
	for _, d := range details {
		k := aggKey{typ: int(d.AccountType), bizType: d.AccountBusinessType}
		row, ok := agg[k]
		if !ok {
			cat, cerr := service.CategoryForAccountType(d.AccountType)
			catStr := string(cat)
			if cerr != nil {
				catStr = "UNKNOWN"
			}
			row = &csvExportRow{
				category:     catStr,
				accountType:  int(d.AccountType),
				businessType: d.AccountBusinessType,
			}
			agg[k] = row
		}
		row.accountCount++
		row.beginning += d.Beginning
		row.debit += d.Debit
		row.credit += d.Credit
		row.ending += d.Ending
	}

	out := make([]*csvExportRow, 0, len(agg))
	for _, v := range agg {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].category != out[j].category {
			return out[i].category < out[j].category
		}
		if out[i].accountType != out[j].accountType {
			return out[i].accountType < out[j].accountType
		}
		return out[i].businessType < out[j].businessType
	})

	rows := make([][]string, 0, len(out))
	for _, v := range out {
		rows = append(rows, []string{
			v.category,
			strconv.Itoa(v.accountType),
			strconv.Itoa(v.businessType),
			strconv.FormatInt(v.accountCount, 10),
			formatMinorAmount(v.beginning),
			formatMinorAmount(v.debit),
			formatMinorAmount(v.credit),
			formatMinorAmount(v.ending),
		})
	}

	dateLabel := snapshotDate
	if dateLabel == "" {
		dateLabel = "live"
	}
	filename := fmt.Sprintf("trial_balance_%s_%s.csv", currency, dateLabel)
	return rows, filename, nil
}

// formatMinorAmount 把 ISO minor unit × 100 的整数金额转成可读的 2 位小数字符串。
// 金额单位约定：存储值 = 实际金额 × 100。导出时除 100 得到 2 位小数。
// 负数保留符号。
func formatMinorAmount(v int64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	intPart := v / 100
	fracPart := v % 100
	s := fmt.Sprintf("%d.%02d", intPart, fracPart)
	if neg {
		s = "-" + s
	}
	return s
}

// atoiDefault 解析 int，失败返回 def。
func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}
