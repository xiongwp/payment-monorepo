// Command replay 拿一条历史 DecisionAudit 重跑当前规则集，对比新旧 verdict。
//
// 用途：
//   - 上线新规则前回归：把上周高 score / 边缘 case 跑一遍，看新规则有没有
//     误杀正常交易、漏掉欺诈
//   - 故障复盘：监管 / 商户投诉时拿 decision_id 还原"为什么这笔 review"
//   - A/B 验证：阈值调整前用历史决策算 expected approval rate
//
// 用法：
//
//	# 单条 decision，从 stdin
//	echo '<DecisionAudit JSON>' | risk-replay --config config/config.yaml
//
//	# 批量，从 admin 端点拉最近 N 条
//	risk-replay --config config/config.yaml \
//	            --url http://risk:9590/admin/audit/decisions?limit=1000 \
//	            --token <admin-bearer>
//
//	# 单条，从文件
//	risk-replay --config config/config.yaml --file audit.json --decision-id <hex>
//
// 输出：每条决策一行，CSV：decision_id, original_verdict, replay_verdict, match, score_diff
//
// **重要限制**：审计存的是 AuditInput（不含 fingerprint / behavior / IP intel
// 富化字段）。依赖这些字段的规则（bot_detection, behavior_anomaly, ip_risk）
// 在 replay 时无法触发，结果 verdict 可能比原始决策更宽松 — 这是预期的，
// 替换审计 schema 让它带全字段属于"未来工作"。
package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"
	"go.uber.org/zap"

	"github.com/xiongwp/risk-manage/internal/audit"
	"github.com/xiongwp/risk-manage/internal/engine"
	"github.com/xiongwp/risk-manage/internal/merchantlist"
	"github.com/xiongwp/risk-manage/internal/rules"
	"github.com/xiongwp/risk-manage/internal/sanction"
	"github.com/xiongwp/risk-manage/internal/store"
)

func main() {
	var (
		cfgPath    = flag.String("config", "config/config.yaml", "engine config (YAML)")
		filePath   = flag.String("file", "", "audit JSON file (one record or array)")
		url        = flag.String("url", "", "GET endpoint returning DecisionAudit array (e.g. /admin/audit/decisions?limit=N)")
		token      = flag.String("token", "", "Bearer token for admin endpoint")
		decisionID = flag.String("decision-id", "", "filter to a single decision_id from the source")
		// 回测：候选规则 / 阈值在历史决策上的影响。
		// --candidate-rule 接受多次：每条 JSON RuleDef 字符串 / 文件路径，e.g.:
		//   --candidate-rule '{"id":"rA","name":"big amt review","type":"amount_limit",
		//                       "enabled":true,"weight":25,"config":"{\"max_per_txn\":1000000}"}'
		// 与 base config 的规则合并：同 ID 覆盖；新 ID 追加。
		candidateRules multiFlag
		candidateThresholds = flag.String("candidate-thresholds", "",
			"override score thresholds 'review_min,deny_min' (e.g. 15,40)")
		summaryOnly = flag.Bool("summary", false, "skip per-row CSV; just print verdict diff aggregates")
		// A/B 模式：用整套独立 config 当 challenger，跟 base config 同流量
		// side-by-side 跑，输出两边 verdict 差异。比 candidate-rule 单条覆盖更
		// 干净 — 用来评估"完全替换规则集"的影响（如 v2 → v3 整套迁移）。
		challengerCfg = flag.String("challenger-config", "",
			"side-by-side challenger engine config (full YAML); A/B mode")
	)
	flag.Var(&candidateRules, "candidate-rule",
		"candidate rule definition (JSON RuleDef or @path-to-json); repeatable")
	flag.Parse()

	logger, _ := zap.NewDevelopment()
	defer logger.Sync()

	eng, err := buildEngine(*cfgPath, logger, candidateRules, *candidateThresholds)
	if err != nil {
		fmt.Fprintln(os.Stderr, "engine load:", err)
		os.Exit(1)
	}
	// A/B 模式：构第二套 engine。共用同一组 candidate-rule / thresholds 不应用
	// 到 challenger（A/B 看完整规则集对比，不再叠 candidate）。
	var challengerEng *engine.Engine
	if *challengerCfg != "" {
		challengerEng, err = buildEngine(*challengerCfg, logger, nil, "")
		if err != nil {
			fmt.Fprintln(os.Stderr, "challenger engine load:", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "A/B mode: champion=%s challenger=%s\n", *cfgPath, *challengerCfg)
	}

	records, err := loadRecords(*filePath, *url, *token)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load:", err)
		os.Exit(1)
	}
	if len(records) == 0 {
		fmt.Fprintln(os.Stderr, "no audit records found")
		os.Exit(1)
	}

	var w *csv.Writer
	if !*summaryOnly {
		w = csv.NewWriter(os.Stdout)
		defer w.Flush()
		header := []string{
			"decision_id", "occurred_at",
			"original_verdict", "original_score",
			"replay_verdict", "replay_score",
			"match", "score_diff", "replay_hits",
		}
		if challengerEng != nil {
			header = append(header, "challenger_verdict", "challenger_score", "challenger_hits")
		}
		_ = w.Write(header)
	}

	matched, total := 0, 0
	// 决策-到-决策 confusion matrix：原 verdict × replay verdict
	confusion := map[string]int{}
	// A/B 模式额外的 confusion：champion verdict × challenger verdict
	abConfusion := map[string]int{}
	for _, a := range records {
		if *decisionID != "" && a.DecisionID != *decisionID {
			continue
		}
		total++
		txn := txnFromAudit(a)
		res := eng.Evaluate(context.Background(), txn)

		hits := make([]string, 0, len(res.Hits))
		for _, h := range res.Hits {
			hits = append(hits, h.RuleID)
		}
		match := res.Decision.String() == a.Verdict
		if match {
			matched++
		}
		confusion[a.Verdict+"→"+res.Decision.String()]++

		// A/B 模式：跑 challenger，记录 champion vs challenger confusion
		var challengerRes *engine.Result
		if challengerEng != nil {
			// 用 fresh txn copy，不让 engine 之间互相污染 enrichment
			cTxn := txnFromAudit(a)
			challengerRes = challengerEng.Evaluate(context.Background(), cTxn)
			abConfusion[res.Decision.String()+"→"+challengerRes.Decision.String()]++
		}

		if w != nil {
			scoreDiff := res.RiskScore - a.RiskScore
			row := []string{
				a.DecisionID, a.OccurredAt.UTC().Format(time.RFC3339),
				a.Verdict, fmt.Sprintf("%d", a.RiskScore),
				res.Decision.String(), fmt.Sprintf("%d", res.RiskScore),
				fmt.Sprintf("%t", match), fmt.Sprintf("%+d", scoreDiff),
				strings.Join(hits, "|"),
			}
			if challengerRes != nil {
				cHits := make([]string, 0, len(challengerRes.Hits))
				for _, h := range challengerRes.Hits {
					cHits = append(cHits, h.RuleID)
				}
				row = append(row,
					challengerRes.Decision.String(),
					fmt.Sprintf("%d", challengerRes.RiskScore),
					strings.Join(cHits, "|"),
				)
			}
			_ = w.Write(row)
		}
	}
	fmt.Fprintf(os.Stderr, "\nreplay: %d/%d match (%.1f%%)\n",
		matched, total, percent(matched, total))
	// 突出运营关心的几格：
	//   原 ALLOW → 现 DENY  ：候选规则会拦下原本通过的（误杀风险）
	//   原 ALLOW → 现 REVIEW：候选规则会让原本通过的进人审（运营负担↑）
	//   原 DENY  → 现 ALLOW ：候选规则会放行原本拦的（漏抓风险）
	//   原 REVIEW→ 现 ALLOW ：候选规则会跳过原本人审的
	for _, k := range []string{"ALLOW→DENY", "ALLOW→REVIEW", "DENY→ALLOW", "DENY→REVIEW", "REVIEW→ALLOW", "REVIEW→DENY"} {
		if v := confusion[k]; v > 0 {
			fmt.Fprintf(os.Stderr, "  %-15s %d\n", k, v)
		}
	}
	if challengerEng != nil {
		fmt.Fprintln(os.Stderr, "\n=== A/B confusion matrix (champion → challenger) ===")
		// 同 ID 决策不变 + 改的差异都展示
		for _, k := range []string{
			"ALLOW→ALLOW", "ALLOW→REVIEW", "ALLOW→DENY",
			"REVIEW→ALLOW", "REVIEW→REVIEW", "REVIEW→DENY",
			"DENY→ALLOW", "DENY→REVIEW", "DENY→DENY",
		} {
			if v := abConfusion[k]; v > 0 {
				fmt.Fprintf(os.Stderr, "  %-18s %d\n", k, v)
			}
		}
		// 关键运营指标：
		//   ALLOW→DENY 多 = challenger 收紧（拦更多）
		//   DENY→ALLOW 多 = challenger 放宽
		//   净拦截变化 = (ALLOW→DENY) - (DENY→ALLOW)
		netBlock := abConfusion["ALLOW→DENY"] - abConfusion["DENY→ALLOW"]
		fmt.Fprintf(os.Stderr, "  net_extra_blocks: %+d\n", netBlock)
	}
}

// multiFlag 给 flag.Var 用：支持 --candidate-rule 重复出现。
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, "; ") }
func (m *multiFlag) Set(s string) error { *m = append(*m, s); return nil }

func percent(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return 100 * float64(n) / float64(d)
}

func buildEngine(cfgPath string, logger *zap.Logger, candidates multiFlag, thresholdsOverride string) (*engine.Engine, error) {
	v := viper.New()
	v.SetConfigFile(cfgPath)
	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("read config %s: %w", cfgPath, err)
	}
	// replay 走 Mem 实现：不需要外部依赖，规则若依赖 store 数据（velocity /
	// link_fanout）就会落空，输出空 hits — 这是 replay 的预期行为，提示用户
	// 这些规则需要"实时数据"才有意义。
	counter := store.NewMemCounter()
	bl := store.NewMemBlacklist()
	links := store.NewMemLinkStore()
	mlSvc := merchantlist.NewMemService()
	ivt := store.NewMemIntervalTracker()

	eng := engine.New(logger)
	eng.RegisterFactory("amount_limit", rules.AmountLimitFactory(counter))
	eng.RegisterFactory("velocity", rules.VelocityFactory(counter))
	eng.RegisterFactory("blacklist", rules.BlacklistFactory(bl))
	eng.RegisterFactory("country_block", rules.CountryBlockFactory())
	eng.RegisterFactory("link_fanout", rules.LinkFanoutFactory(links))
	eng.RegisterFactory("link_fanout_multihop", rules.LinkFanoutMultihopFactory(links))
	eng.RegisterFactory("graph_reputation", rules.GraphReputationFactory(links))
	eng.RegisterFactory("cross_merchant_link", rules.CrossMerchantLinkFactory(links))
	eng.RegisterFactory("card_testing", rules.CardTestingFactory(links))
	eng.RegisterFactory("sanction_screening", rules.SanctionScreeningFactory(sanction.NewMemService()))
	eng.RegisterFactory("dsl", rules.DSLFactory())
	eng.RegisterFactory("cel", rules.CELFactory())
	eng.RegisterFactory("impossible_travel", rules.ImpossibleTravelFactory())
	eng.RegisterFactory("returning_customer", rules.ReturningCustomerFactory())
	eng.RegisterFactory("avs_check", rules.AVSCheckFactory())
	eng.RegisterFactory("bin_country", rules.BINCountryFactory())
	eng.RegisterFactory("email_validation", rules.EmailValidationFactory())
	eng.RegisterFactory("velocity_amount", rules.VelocityAmountFactory(counter))
	eng.RegisterFactory("rolling_amount", rules.RollingAmountFactory(counter))
	eng.RegisterFactory("ip_risk", rules.IPRiskFactory())
	eng.RegisterFactory("behavior_anomaly", rules.BehaviorAnomalyFactory())
	eng.RegisterFactory("bot_detection", rules.BotDetectionFactory())
	eng.RegisterFactory("ml_threshold", rules.MLThresholdFactory())
	eng.RegisterFactory("merchant_allowlist", rules.MerchantAllowlistFactory(mlSvc))
	eng.RegisterFactory("merchant_blocklist", rules.MerchantBlocklistFactory(mlSvc))
	eng.RegisterFactory("new_account_high_value", rules.NewAccountHighValueFactory())
	eng.RegisterFactory("login_anomaly", rules.LoginAnomalyFactory())
	eng.RegisterFactory("email_pattern", rules.EmailPatternFactory())
	eng.RegisterFactory("register_velocity", rules.RegisterVelocityFactory(links))
	eng.RegisterFactory("client_tampering", rules.ClientTamperingFactory())
	eng.RegisterFactory("register_interval", rules.RegisterIntervalFactory(ivt))
	eng.RegisterFactory("username_pattern", rules.UsernamePatternFactory())
	eng.RegisterFactory("fingerprint_multi_account", rules.FingerprintMultiAccountFactory(links))
	eng.RegisterFactory("ua_batch_register", rules.UABatchRegisterFactory(links))

	var defs []struct {
		ID      string `mapstructure:"id"`
		Name    string `mapstructure:"name"`
		Type    string `mapstructure:"type"`
		Enabled bool   `mapstructure:"enabled"`
		Config  string `mapstructure:"config"`
	}
	if err := v.UnmarshalKey("rules", &defs); err != nil {
		return nil, err
	}
	engineDefs := make([]engine.RuleDef, 0, len(defs))
	byID := map[string]int{}
	for _, d := range defs {
		engineDefs = append(engineDefs, engine.RuleDef{
			ID: d.ID, Name: d.Name, Type: d.Type, Enabled: d.Enabled,
			ConfigJSON: json.RawMessage(d.Config),
		})
		byID[d.ID] = len(engineDefs) - 1
	}

	// 候选规则：JSON 文本 / @file 路径，覆盖 base 同 ID 或追加。
	for _, raw := range candidates {
		body, err := loadCandidateBody(raw)
		if err != nil {
			return nil, fmt.Errorf("--candidate-rule %q: %w", raw, err)
		}
		var cd struct {
			ID       string          `json:"id"`
			Name     string          `json:"name"`
			Type     string          `json:"type"`
			Decision string          `json:"decision"`
			Enabled  bool            `json:"enabled"`
			Mode     string          `json:"mode"`
			Weight   int             `json:"weight"`
			Config   json.RawMessage `json:"config"`
		}
		if err := json.Unmarshal(body, &cd); err != nil {
			return nil, fmt.Errorf("candidate parse: %w", err)
		}
		def := engine.RuleDef{
			ID: cd.ID, Name: cd.Name, Type: cd.Type, Decision: cd.Decision,
			Enabled: cd.Enabled, Mode: cd.Mode, Weight: cd.Weight,
			ConfigJSON: cd.Config,
		}
		if idx, ok := byID[cd.ID]; ok {
			engineDefs[idx] = def
			fmt.Fprintf(os.Stderr, "candidate: replace base rule %q\n", cd.ID)
		} else {
			engineDefs = append(engineDefs, def)
			byID[cd.ID] = len(engineDefs) - 1
			fmt.Fprintf(os.Stderr, "candidate: add new rule %q\n", cd.ID)
		}
	}

	if err := eng.LoadRules(engineDefs); err != nil {
		return nil, err
	}

	// 阈值覆盖："review_min,deny_min"
	if thresholdsOverride != "" {
		var rmin, dmin int
		if _, err := fmt.Sscanf(thresholdsOverride, "%d,%d", &rmin, &dmin); err != nil {
			return nil, fmt.Errorf("--candidate-thresholds %q: %w (want 'review_min,deny_min')", thresholdsOverride, err)
		}
		eng.SetScoreThresholds(rmin, dmin)
		fmt.Fprintf(os.Stderr, "candidate thresholds: review_min=%d deny_min=%d\n", rmin, dmin)
	}
	return eng, nil
}

// loadCandidateBody 支持 plain JSON 字符串或 @path 文件读取。
func loadCandidateBody(arg string) ([]byte, error) {
	if strings.HasPrefix(arg, "@") {
		return os.ReadFile(arg[1:])
	}
	return []byte(arg), nil
}

// loadRecords 从 file 或 url 拉 DecisionAudit 数组（兼容单条 / 数组 JSON）。
func loadRecords(filePath, url, token string) ([]*audit.DecisionAudit, error) {
	var data []byte
	var err error
	switch {
	case filePath != "":
		data, err = os.ReadFile(filePath)
		if err != nil {
			return nil, err
		}
	case url != "":
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		data, err = io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
	default:
		// stdin
		data, err = io.ReadAll(os.Stdin)
		if err != nil {
			return nil, err
		}
	}
	if len(data) == 0 {
		return nil, errors.New("empty input")
	}
	// 先试数组，再试单条；admin 端点用 metrics.AuditRow 做了瘦身投影 — 这里
	// 兼容 metrics.AuditRow + 完整 DecisionAudit 两种格式（前者 hits 是 rule_id
	// 数组而非完整 hit struct，replay 时只用得到 input + verdict）。
	var arr []*audit.DecisionAudit
	if err := json.Unmarshal(data, &arr); err == nil && len(arr) > 0 {
		return arr, nil
	}
	// metrics.AuditRow 数组（admin 端点的实际格式）
	var rows []auditRow
	if err := json.Unmarshal(data, &rows); err == nil && len(rows) > 0 {
		out := make([]*audit.DecisionAudit, 0, len(rows))
		for _, r := range rows {
			t, _ := time.Parse(time.RFC3339Nano, r.OccurredAt)
			out = append(out, &audit.DecisionAudit{
				DecisionID:     r.DecisionID,
				OccurredAt:     t,
				Verdict:        r.Verdict,
				RiskScore:      r.RiskScore,
				RiskLevel:      r.RiskLevel,
				EvalDurationMs: r.EvalDurationMs,
				// AuditRow 不带 Input；replay 输入字段全空 → 多数规则不命中（已知限制）
			})
		}
		return out, nil
	}
	// 单条
	var single audit.DecisionAudit
	if err := json.Unmarshal(data, &single); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return []*audit.DecisionAudit{&single}, nil
}

// auditRow 镜像 metrics.AuditRow，避免在 replay CLI 反向 import metrics 包。
type auditRow struct {
	DecisionID     string  `json:"decision_id"`
	OccurredAt     string  `json:"occurred_at"`
	Verdict        string  `json:"verdict"`
	RiskScore      int     `json:"risk_score"`
	RiskLevel      string  `json:"risk_level"`
	HitRules       []string `json:"hit_rules"`
	EvalDurationMs float64 `json:"eval_duration_ms"`
}

// txnFromAudit 把 AuditInput 还原成 TxnContext。fingerprint / behavior /
// IP intel 字段在 audit schema 里没保存 → 这里只能空着。replay 用户应该清楚：
// 依赖那些字段的规则（bot_detection 等）replay 时不会命中，输出 score 偏低
// 是符合预期的"replay 完整性局限"。
func txnFromAudit(a *audit.DecisionAudit) *engine.TxnContext {
	return &engine.TxnContext{
		PaymentIntentID: a.Input.PaymentIntentID,
		MerchantID:      a.Input.MerchantID,
		CustomerID:      a.Input.CustomerID,
		Amount:          a.Input.Amount,
		Currency:        a.Input.Currency,
		PaymentMethod:   a.Input.PaymentMethod,
		Country:         a.Input.Country,
		IPAddress:       a.Input.IPAddress,
		DeviceID:        a.Input.DeviceID,
		Metadata:        a.Input.Metadata,
		MLScore:         a.MLScore,
		MLModelVer:      a.MLModelVer,
	}
}
