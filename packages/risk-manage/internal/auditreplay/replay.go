// Package auditreplay 把每次 Screen 的原始信号 snapshot 落到对象存储 / 本地，
// 支持事后 replay：拿当前规则集对历史决策重跑 → 决策 diff。
//
// 用途：
//   1. 事故诊断：用户投诉 "为什么这笔被 deny"，运营拿 decision_id 看 raw signals
//   2. 规则回归测试：改规则前先在最近 1k 笔历史决策上跑，看 verdict 翻车率
//   3. 模型 A/B：新模型对历史决策的判断分布 vs 旧模型
//   4. 监管审计：监管要求"重现 2023-08-15 14:32 这笔的决策路径"，原始信号 + 规则版本 + 模型版本三件套调出来
//
// PII 脱敏（落盘前做）：
//   email → sha256 前 16 hex（保留可比对性，不可逆）
//   phone → sha256 前 16 hex
//   IP → /24 截断（保留地理意义，不锁人）
//   keystroke 已是统计量（dwell mean/CV），无内容
//   mouse trajectory 已是统计量，无 (x,y) 序列
//   merchant_id / customer_id 保留（业务必需）
//
// Retention：30d，定时清理。
//
// 不存什么：原始 (x,y,t) 鼠标轨迹（每决策 ~100KB×200 点）、原始 keystroke
// 内容（隐私 + 攻击面）；只存采集出来的统计指标。
package auditreplay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// SignalSnapshot 单次 Screen 的全部输入信号 + 决策上下文。
type SignalSnapshot struct {
	DecisionID   string                 `json:"decision_id"`
	OccurredAt   time.Time              `json:"occurred_at"`
	MerchantID   string                 `json:"merchant_id"`
	CustomerID   string                 `json:"customer_id"`             // 脱敏前；落盘时调 Redact()
	RuleVersion  string                 `json:"rule_version"`            // RuleSetHash hex
	ModelVer     string                 `json:"model_ver"`
	Verdict      string                 `json:"verdict"`
	Score        int                    `json:"score"`
	Signals      map[string]interface{} `json:"signals"`                 // SDK 上来的 28+ 信号
	Hits         []string               `json:"hits"`                    // 命中的 rule_id
	StageLatency map[string]float64     `json:"stage_latency_ms"`        // feature/ipintel/ml/engine/audit
}

// Store 抽象底层存储。MemStore（test）/ LocalStore（dev）/ S3Store（prod）实现。
type Store interface {
	Put(ctx context.Context, snap SignalSnapshot) error
	Get(ctx context.Context, decisionID string) (SignalSnapshot, error)
	// List 用 (date, prefix) 范围扫，分页用 token。监管审计用。
	List(ctx context.Context, date time.Time, limit int) ([]SignalSnapshot, error)
}

// ErrNotFound snapshot 不存在。
type ErrNotFoundType string

func (e ErrNotFoundType) Error() string { return string(e) }

var ErrNotFound ErrNotFoundType = "auditreplay: decision not found"

// Redact 落盘前做 PII 脱敏，原 snapshot 不动（不可变拷贝）。
func (s SignalSnapshot) Redact() SignalSnapshot {
	out := s
	out.CustomerID = hashShort(s.CustomerID)
	// signals 里如果有 email / phone / ip / userId 也脱敏
	if out.Signals != nil {
		redactedSignals := make(map[string]interface{}, len(out.Signals))
		for k, v := range out.Signals {
			redactedSignals[k] = redactValue(k, v)
		}
		out.Signals = redactedSignals
	}
	return out
}

// redactValue 按 key 名识别敏感字段并脱敏。
func redactValue(key string, v interface{}) interface{} {
	low := strings.ToLower(key)
	switch {
	case strings.Contains(low, "email"):
		if s, ok := v.(string); ok {
			return hashShort(s)
		}
	case strings.Contains(low, "phone"), strings.Contains(low, "msisdn"):
		if s, ok := v.(string); ok {
			return hashShort(s)
		}
	case low == "ip" || strings.HasSuffix(low, "_ip"):
		if s, ok := v.(string); ok {
			return ipv4Mask24(s)
		}
	case low == "userid" || low == "user_id":
		if s, ok := v.(string); ok {
			return hashShort(s)
		}
	}
	return v
}

func hashShort(s string) string {
	if s == "" {
		return ""
	}
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:8]) // 16 hex chars
}

// ipv4Mask24 把 1.2.3.4 截到 1.2.3.0；不是 IPv4 原样返。
func ipv4Mask24(ip string) string {
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		// IPv6 简单截 /48；详细逻辑不在本包范围
		if i := strings.Index(ip, "::"); i > 0 {
			return ip[:i] + "::"
		}
		return ip
	}
	return parts[0] + "." + parts[1] + "." + parts[2] + ".0"
}

// MarshalJSON / UnmarshalJSON helper：snapshot 落盘用 gzip+json。
// 但 gzip 在具体 Store 实现里做，这里只暴露 JSON 序列化。
func (s SignalSnapshot) MarshalCompact() ([]byte, error) {
	return json.Marshal(s)
}

// objectKey 生成存储 key：risk/yyyy/mm/dd/{decision_id}.json
func objectKey(prefix string, t time.Time, decisionID string) string {
	return fmt.Sprintf("%s/%04d/%02d/%02d/%s.json", prefix, t.Year(), int(t.Month()), t.Day(), decisionID)
}
