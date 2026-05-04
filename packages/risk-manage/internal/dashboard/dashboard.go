// Package dashboard —— admin /admin/dashboard/summary 聚合端点。
//
// 把运营常用的几个数字一次性返给前端，避免 admin-web 跑 5 个并行 RPC
// 自己拼装。每个数据源都用 callback 注入，本包不直接依赖 review / engine /
// mlscore（避免循环 import + 让 main 可以传任意实现）。
//
// 字段：
//   - rule_count            engine 当前规则数（含 shadow）
//   - queue                 case 队列深度（pending / in_review / escalated / overdue）
//   - models                champion + challengers 名字
//   - decisions_by_verdict  最近 N 笔的 ALLOW / REVIEW / DENY 计数
//
// 一次 RPC 拿全套，admin Dashboard 页面直接渲染。
package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/risk-manage/internal/audit"
)

// QueueCounts 由 caller 实现（review.Store 可包装一层）。
type QueueCounts struct {
	Pending   int `json:"pending"`
	InReview  int `json:"in_review"`
	Escalated int `json:"escalated"`
	Approved  int `json:"approved"`
	Rejected  int `json:"rejected"`
	Overdue   int `json:"overdue"`
}

// RecentAuditFn 复用 audit.MemSink.Recent 风格的 callback。
type RecentAuditFn func(limit int) []*audit.DecisionAudit

// Sources 数据源 callback 集合。任何 nil → 该字段在 summary 输出空 / 0。
type Sources struct {
	RuleCount         func() int
	GetQueueCounts    func() QueueCounts
	ChampionModel     func() string
	ChallengerModels  func() []string
	RecentAudits      RecentAuditFn
	// GetRecallStats 给 /admin/dashboard/recall 用：拉最近 N 天 featurestore
	// snapshot + outcome label 算 P/R/FPR。nil → endpoint 返空。
	GetRecallStats    func() RecallStats
}

// RecallStats 系统级风控召回 / 准确率指标（最近 N 天）。
//
// 概念：把每一笔交易看做模型对"是否 fraud"的二分类预测。verdict ∈
// {DENY, REVIEW} 视作"系统判 fraud"，ALLOW 视作"判 legit"。outcome_label
// 是 ground truth（chargeback / dispute 来的真实标签）。
//
//   - precision_at_block = 系统 BLOCK（DENY+REVIEW）里实际是 fraud 的比例 →
//     越高 = 误伤越少
//   - recall_at_block    = 实际 fraud 里被系统 BLOCK 的比例 → 越高 = 漏抓越少
//   - false_positive_rate = legit 里被错杀的比例 → 越低 = 客户体验越好
type RecallStats struct {
	WindowDays      int     `json:"window_days"`
	LabeledSamples  int     `json:"labeled_samples"`
	ActualFraud     int     `json:"actual_fraud"`
	ActualLegit     int     `json:"actual_legit"`
	TruePositives   int     `json:"true_positives"`
	FalsePositives  int     `json:"false_positives"`
	TrueNegatives   int     `json:"true_negatives"`
	FalseNegatives  int     `json:"false_negatives"`
	PrecisionAtBlock float64 `json:"precision_at_block"`
	RecallAtBlock    float64 `json:"recall_at_block"`
	FalsePositiveRate float64 `json:"false_positive_rate"`
	F1Score         float64 `json:"f1_score"`
}

// Summary admin 拿到的 JSON。
type Summary struct {
	Now             time.Time          `json:"now"`
	RuleCount       int                `json:"rule_count"`
	Queue           QueueCounts        `json:"queue"`
	ChampionModel   string             `json:"champion_model,omitempty"`
	ChallengerModels []string          `json:"challenger_models,omitempty"`
	DecisionsByVerdict map[string]int  `json:"decisions_by_verdict"`
	DecisionsSampleSize int            `json:"decisions_sample_size"`
}

// RegisterHandler 把端点挂到 mux：GET /admin/dashboard/summary 和
// GET /admin/dashboard/recall。
func RegisterHandler(mux *http.ServeMux, src Sources, logger *zap.Logger) {
	mux.HandleFunc("/admin/dashboard/summary", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		s := buildSummary(r.Context(), src)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(s)
	})
	mux.HandleFunc("/admin/dashboard/recall", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		stats := RecallStats{}
		if src.GetRecallStats != nil {
			stats = src.GetRecallStats()
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(stats)
	})
}

func buildSummary(ctx context.Context, src Sources) Summary {
	out := Summary{Now: time.Now().UTC(), DecisionsByVerdict: map[string]int{}}
	if src.RuleCount != nil {
		out.RuleCount = src.RuleCount()
	}
	if src.GetQueueCounts != nil {
		out.Queue = src.GetQueueCounts()
	}
	if src.ChampionModel != nil {
		out.ChampionModel = src.ChampionModel()
	}
	if src.ChallengerModels != nil {
		out.ChallengerModels = src.ChallengerModels()
	}
	if src.RecentAudits != nil {
		// 取最近 500 条决策做 verdict 分布
		rows := src.RecentAudits(500)
		out.DecisionsSampleSize = len(rows)
		for _, r := range rows {
			out.DecisionsByVerdict[r.Verdict]++
		}
	}
	_ = ctx
	return out
}
