package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xiongwp/risk-manage/internal/engine"
)

// ─── bot_detection: 复合 bot 信号 ────────────────────────────────────
//
// 跟 behavior_anomaly 区别：
//   - behavior_anomaly 是行为面（鼠标 / 打字 / 时间），任一阈值过线即命中
//   - bot_detection 是设备面 × 行为面联合：单一信号容易 false positive，
//     联合判断更稳
//
// 逻辑：score = sum(命中权重)；score >= min_signals → 命中（默认 2）。
//
// 信号（每个对应一条公开的 bot fingerprint 特征）：
//
//	signal               weight  说明
//	─────────────────────────────────────────────────────────────────────
//	headless_webgl        2      WebGLRenderer 含 SwiftShader / llvmpipe / Mesa
//	low_concurrency       1      HardwareConcurrency in [0, 1]（headless / 弱 VM）
//	missing_screen        1      ScreenWxH 缺失或 < "640x480"
//	zero_mouse_entropy    2      MouseMovementEntropy == 0（无鼠标轨迹）
//	rapid_checkout        2      TimeToCheckoutMs > 0 && < min_time（默认 1000）
//	no_keystrokes         1      KeystrokeCount == 0 且 PastedFields 不为空
//	bot_user_agent        2      UserAgent 含 HeadlessChrome / PhantomJS / etc
//
// 触发示例：headless_webgl(2) + zero_mouse_entropy(2) ≥ 2 → 命中（典型 puppeteer
// 脚本指纹）。
//
// 配置：
//
//	type: bot_detection
//	config:
//	  min_signals:         2
//	  min_time_ms:         1000
//	  decision:            deny    # 默认 review
//	  weight:              50      # 命中规则的全局 score 权重（不是单信号权重）

type BotDetectionConfig struct {
	MinSignals int    `json:"min_signals"`
	MinTimeMs  int64  `json:"min_time_ms"`
	Decision   string `json:"decision"` // deny / review；默认 review
}

type botDetectionRule struct {
	id, name string
	enabled  bool
	cfg      BotDetectionConfig
	verdict  engine.Decision
}

func BotDetectionFactory() engine.RuleFactory {
	return func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		var cfg BotDetectionConfig
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("bot_detection config: %w", err)
		}
		if cfg.MinSignals <= 0 {
			cfg.MinSignals = 2
		}
		if cfg.MinTimeMs <= 0 {
			cfg.MinTimeMs = 1000
		}
		verdict := engine.Review
		if strings.EqualFold(strings.TrimSpace(cfg.Decision), "deny") {
			verdict = engine.Deny
		}
		return &botDetectionRule{id: id, name: name, enabled: enabled, cfg: cfg, verdict: verdict}, nil
	}
}

func (r *botDetectionRule) ID() string    { return r.id }
func (r *botDetectionRule) Name() string  { return r.name }
func (r *botDetectionRule) Type() string  { return "bot_detection" }
func (r *botDetectionRule) Enabled() bool { return r.enabled }

func (r *botDetectionRule) Evaluate(_ context.Context, txn *engine.TxnContext) *engine.Hit {
	if txn == nil {
		return nil
	}
	score := 0
	var signals []string

	// headless WebGL / 软件渲染器
	wg := strings.ToLower(txn.WebGLRenderer)
	if wg != "" && (strings.Contains(wg, "swiftshader") ||
		strings.Contains(wg, "llvmpipe") ||
		strings.Contains(wg, "mesa offscreen") ||
		strings.Contains(wg, "google swiftshader")) {
		score += 2
		signals = append(signals, "headless_webgl="+txn.WebGLRenderer)
	}
	// hardware concurrency 极低
	if txn.HardwareConcurrency > 0 && txn.HardwareConcurrency <= 1 {
		score++
		signals = append(signals, fmt.Sprintf("low_concurrency=%d", txn.HardwareConcurrency))
	}
	// 屏幕分辨率缺失
	if txn.ScreenWxH == "" || txn.ScreenWxH == "0x0" {
		score++
		signals = append(signals, "missing_screen")
	}
	// 鼠标轨迹熵 0（fingerprint 上报但 0 ≠ 未上报，需 bool 区分；当前 0 视为未上报跳过）
	// 改用：明确上报 0 鼠标移动 + 有键盘事件 → bot 信号
	if txn.MouseMovementEntropy == 0 && (txn.KeystrokeCount > 0 || len(txn.PastedFields) > 0) {
		score += 2
		signals = append(signals, "zero_mouse_entropy_with_input")
	}
	// 极速 checkout
	if txn.TimeToCheckoutMs > 0 && txn.TimeToCheckoutMs < r.cfg.MinTimeMs {
		score += 2
		signals = append(signals, fmt.Sprintf("rapid_checkout=%dms", txn.TimeToCheckoutMs))
	}
	// 没键入但有粘贴（自动填）
	if txn.KeystrokeCount == 0 && len(txn.PastedFields) > 0 {
		score++
		signals = append(signals, "no_keystrokes_with_paste")
	}
	// User-Agent 显式 bot 字符串
	ua := strings.ToLower(txn.UserAgent)
	if ua != "" && (strings.Contains(ua, "headlesschrome") ||
		strings.Contains(ua, "phantomjs") ||
		strings.Contains(ua, "puppeteer") ||
		strings.Contains(ua, "playwright") ||
		strings.Contains(ua, "selenium")) {
		score += 2
		signals = append(signals, "bot_user_agent")
	}
	// Android emulator / VM 检测：UA / GPU 特征
	if ua != "" && (strings.Contains(ua, "genymotion") ||
		strings.Contains(ua, "bluestacks") ||
		strings.Contains(ua, "noxplayer") ||
		strings.Contains(ua, "memuplayer") ||
		strings.Contains(ua, "andy emulator") ||
		strings.Contains(ua, "androidx86") ||
		strings.Contains(ua, "vmware")) {
		score += 2
		signals = append(signals, "emulator_user_agent")
	}
	// 模拟器常见 GPU 字串
	if wg != "" && (strings.Contains(wg, "vmware svga") ||
		strings.Contains(wg, "virtualbox") ||
		strings.Contains(wg, "qemu") ||
		strings.Contains(wg, "parallels") ||
		strings.Contains(wg, "android emulator") ||
		strings.Contains(wg, "google android emulator")) {
		score += 2
		signals = append(signals, "vm_renderer")
	}

	if score < r.cfg.MinSignals {
		return nil
	}
	return &engine.Hit{
		RuleID:   r.id,
		RuleName: r.name,
		Decision: r.verdict,
		Detail:   fmt.Sprintf("bot signals (score=%d): %s", score, strings.Join(signals, "; ")),
	}
}
