// Package cohort 按运营维度（商户 / 国家 / 渠道）拆分决策 + outcome，
// 算每组的 block_rate / fraud_rate / precision。
//
// 给运营 dashboard 看："X 商户最近一周的 block_rate 是 12%（远高于平台
// 平均 3%）" → 触发人工对账："是商户业务突然多了 high-risk 流量，还是
// 风控规则误伤了？"
//
// 计算 on-demand 跑（Compute），不维护单独存储。数据源 = audit.MemSink +
// feedback.Recorder。生产应该从 ClickHouse 长期数据跑离线 batch。
package cohort

import (
	"sort"
	"strings"
	"time"

	"github.com/xiongwp/risk-manage/internal/audit"
	"github.com/xiongwp/risk-manage/internal/feedback"
)

// GroupBy 决定 cohort key 怎么从 AuditInput 取。
type GroupBy string

const (
	GroupByMerchant      GroupBy = "merchant_id"
	GroupByCountry       GroupBy = "country"
	GroupByPaymentMethod GroupBy = "payment_method"
)

// Stats 单个 cohort 的统计快照。
type Stats struct {
	Key                string  `json:"key"`
	Total              int     `json:"total"`
	Block              int     `json:"block"`             // verdict ∈ {DENY, REVIEW}
	Allow              int     `json:"allow"`
	BlockRate          float64 `json:"block_rate"`        // block / total
	LabeledTotal       int     `json:"labeled_total"`     // 有 outcome 反馈的样本数
	ActualFraud        int     `json:"actual_fraud"`      // 其中 is_fraud=true
	ActualFraudRate    float64 `json:"actual_fraud_rate"` // actual_fraud / labeled_total
	BlockedFraud       int     `json:"blocked_fraud"`     // block AND is_fraud=true (TP)
	BlockedLegit       int     `json:"blocked_legit"`     // block AND is_fraud=false (FP)
	PrecisionAtBlock   float64 `json:"precision_at_block"` // TP / block (labeled subset)
}

// Compute 按 groupBy 聚合 audit + feedback 数据，返回所有 cohort 的统计列表。
//
// 排序：默认按 Total 降序（流量大的商户在前）；caller 想换排序自己 sort。
//
// minTotal：cohort 总样本数 < minTotal 直接丢掉，避免长尾噪音和单条样本
// 算出 precision=1.0/0.0 这种误导值。0 = 不丢。
func Compute(audits []*audit.DecisionAudit, fbRec feedback.Recorder, groupBy GroupBy, minTotal int) []Stats {
	bucket := make(map[string]*Stats)

	for _, a := range audits {
		if a == nil {
			continue
		}
		key := keyOf(a, groupBy)
		if key == "" {
			continue
		}
		s, ok := bucket[key]
		if !ok {
			s = &Stats{Key: key}
			bucket[key] = s
		}
		s.Total++
		blocked := a.Verdict == "DENY" || a.Verdict == "REVIEW"
		if blocked {
			s.Block++
		} else {
			s.Allow++
		}
		// 反查 outcome（若有）
		if fbRec == nil {
			continue
		}
		outs := fbRec.Get(a.DecisionID)
		if len(outs) == 0 {
			continue
		}
		s.LabeledTotal++
		// 多条 outcome：任一 is_fraud=true 就算 fraud（多源反馈通常一致）
		isFraud := false
		for _, o := range outs {
			if o.IsFraud {
				isFraud = true
				break
			}
		}
		if isFraud {
			s.ActualFraud++
			if blocked {
				s.BlockedFraud++
			}
		} else if blocked {
			s.BlockedLegit++
		}
	}

	out := make([]Stats, 0, len(bucket))
	for _, s := range bucket {
		if s.Total < minTotal {
			continue
		}
		if s.Total > 0 {
			s.BlockRate = float64(s.Block) / float64(s.Total)
		}
		if s.LabeledTotal > 0 {
			s.ActualFraudRate = float64(s.ActualFraud) / float64(s.LabeledTotal)
		}
		// precision @ block 仅在 block 样本里有 outcome 反馈的子集 >= 5 时计算
		blockLabeled := s.BlockedFraud + s.BlockedLegit
		if blockLabeled >= 5 {
			s.PrecisionAtBlock = float64(s.BlockedFraud) / float64(blockLabeled)
		}
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Total != out[j].Total {
			return out[i].Total > out[j].Total
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Bucket 时间桶。
type Bucket string

const (
	BucketHour Bucket = "hour"
	BucketDay  Bucket = "day"
	BucketWeek Bucket = "week"
)

// TimeBucket 单个时间桶的统计。
type TimeBucket struct {
	BucketStart        time.Time `json:"bucket_start"`
	Total              int       `json:"total"`
	Block              int       `json:"block"`
	BlockRate          float64   `json:"block_rate"`
	LabeledTotal       int       `json:"labeled_total"`
	ActualFraud        int       `json:"actual_fraud"`
	ActualFraudRate    float64   `json:"actual_fraud_rate"`
}

// TimeSeries 跑时间序列 cohort：按 bucket 分桶 audit + outcome，给 dashboard
// 画 block_rate / fraud_rate 趋势图。
//
// optionalKey != "" 时只算该 cohort 的子集（如 "merchant_id=m_x" 的近 30 天
// 趋势）；空时 = 全平台。
//
// 时间桶按 OccurredAt truncate；空 bucket 不输出（前端补 0）。结果按
// bucket_start 升序。
func TimeSeries(audits []*audit.DecisionAudit, fbRec feedback.Recorder, gb GroupBy, optionalKey string, bucket Bucket) []TimeBucket {
	bm := make(map[time.Time]*TimeBucket)
	for _, a := range audits {
		if a == nil || a.OccurredAt.IsZero() {
			continue
		}
		if optionalKey != "" && keyOf(a, gb) != optionalKey {
			continue
		}
		bs := truncateToBucket(a.OccurredAt, bucket)
		tb, ok := bm[bs]
		if !ok {
			tb = &TimeBucket{BucketStart: bs}
			bm[bs] = tb
		}
		tb.Total++
		blocked := a.Verdict == "DENY" || a.Verdict == "REVIEW"
		if blocked {
			tb.Block++
		}
		if fbRec == nil {
			continue
		}
		outs := fbRec.Get(a.DecisionID)
		if len(outs) == 0 {
			continue
		}
		tb.LabeledTotal++
		for _, o := range outs {
			if o.IsFraud {
				tb.ActualFraud++
				break
			}
		}
	}
	out := make([]TimeBucket, 0, len(bm))
	for _, tb := range bm {
		if tb.Total > 0 {
			tb.BlockRate = float64(tb.Block) / float64(tb.Total)
		}
		if tb.LabeledTotal > 0 {
			tb.ActualFraudRate = float64(tb.ActualFraud) / float64(tb.LabeledTotal)
		}
		out = append(out, *tb)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BucketStart.Before(out[j].BucketStart) })
	return out
}

func truncateToBucket(t time.Time, b Bucket) time.Time {
	t = t.UTC()
	switch b {
	case BucketHour:
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, time.UTC)
	case BucketWeek:
		// 周一为周首；先 truncate 到天，再回退到当周周一。
		day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		offset := int(day.Weekday()) - int(time.Monday)
		if offset < 0 {
			offset += 7
		}
		return day.AddDate(0, 0, -offset)
	default: // day
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	}
}

func keyOf(a *audit.DecisionAudit, gb GroupBy) string {
	switch gb {
	case GroupByMerchant:
		return a.Input.MerchantID
	case GroupByCountry:
		return strings.ToUpper(a.Input.Country)
	case GroupByPaymentMethod:
		return a.Input.PaymentMethod
	default:
		return a.Input.MerchantID // 缺省按商户分组
	}
}
