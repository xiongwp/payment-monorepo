package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xiongwp/risk-manage/internal/engine"
)

// ─── client_tampering: 反客户端篡改 / 直调 API / replay 检测 ────────
//
// 信号汇总（任一命中累分；累分 ≥ min_signals 触发）：
//
//	signal             weight  说明
//	─────────────────────────────────────────────
//	js_disabled         2      JS 被禁；正常 web 用户不可能（mobile native 例外）
//	beacon_blocked      1      埋点被拦（adblock / 脚本环境）
//	header_anomaly      1      关键 header 缺 / 不一致
//	direct_api_call     2      没 risk_session_id + 没设备指纹 = 绕过前端 SDK
//	bot_user_agent      2      UA 含 curl / wget / python-requests / okhttp
//
// 移动 App 调用 API 默认不会有 risk_session_id（webview 才有），但应有
// 设备 ID + 指纹 hash + UA 包含 app 版本字符串；本规则用 metadata.platform=mobile
// 跳过 direct_api_call 信号防误杀。

type ClientTamperingConfig struct {
	MinSignals int    `json:"min_signals"` // 默认 2
	Decision   string `json:"decision"`    // 默认 review
}

type clientTamperingRule struct {
	id, name string
	enabled  bool
	cfg      ClientTamperingConfig
	verdict  engine.Decision
}

func ClientTamperingFactory() engine.RuleFactory {
	return func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		var cfg ClientTamperingConfig
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return nil, fmt.Errorf("client_tampering: %w", err)
			}
		}
		if cfg.MinSignals <= 0 {
			cfg.MinSignals = 2
		}
		v := engine.Review
		if strings.EqualFold(cfg.Decision, "deny") {
			v = engine.Deny
		}
		return &clientTamperingRule{id: id, name: name, enabled: enabled, cfg: cfg, verdict: v}, nil
	}
}

func (r *clientTamperingRule) ID() string    { return r.id }
func (r *clientTamperingRule) Name() string  { return r.name }
func (r *clientTamperingRule) Type() string  { return "client_tampering" }
func (r *clientTamperingRule) Enabled() bool { return r.enabled }

func (r *clientTamperingRule) Evaluate(_ context.Context, txn *engine.TxnContext) *engine.Hit {
	if txn == nil {
		return nil
	}
	score := 0
	signals := make([]string, 0, 4)
	if txn.JSDisabled {
		score += 2
		signals = append(signals, "js_disabled")
	}
	if txn.BeaconBlocked {
		score++
		signals = append(signals, "beacon_blocked")
	}
	if txn.HeaderAnomaly {
		score++
		signals = append(signals, "header_anomaly")
	}
	// direct_api_call 仅 web 平台计；移动端豁免
	if txn.DirectAPICall && !strings.EqualFold(txn.Platform, "ios") &&
		!strings.EqualFold(txn.Platform, "android") {
		score += 2
		signals = append(signals, "direct_api_call")
	}
	ua := strings.ToLower(txn.UserAgent)
	if ua != "" && (strings.Contains(ua, "curl/") ||
		strings.Contains(ua, "wget/") ||
		strings.Contains(ua, "python-requests/") ||
		strings.Contains(ua, "okhttp/") ||
		strings.Contains(ua, "go-http-client/") ||
		strings.Contains(ua, "httpx/") ||
		strings.Contains(ua, "axios/")) {
		score += 2
		signals = append(signals, "bot_user_agent="+txn.UserAgent)
	}
	if score < r.cfg.MinSignals {
		return nil
	}
	return &engine.Hit{
		RuleID: r.id, RuleName: r.name, Decision: r.verdict,
		Detail: fmt.Sprintf("client tampering signals (score=%d): %s", score, strings.Join(signals, "; ")),
	}
}
