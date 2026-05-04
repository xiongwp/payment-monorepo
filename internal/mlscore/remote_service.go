// remote_service.go: 远程模型服务客户端 (XGBoost / GBDT / TF Serving / Triton)。
//
// 当 logistic regression 不够时，常见做法是把 XGBoost / 神经网络等模型
// 部署在独立的 model serving (TF Serving / TorchServe / Triton / 自建
// gRPC service)，risk-manage 走 HTTP / gRPC 调用。
//
// 本实现：HTTP JSON 客户端。schema：
//
//   POST <base>/score
//   Body: {"features": {<feature_name>: <value>, ...}}
//   Resp: {"score": 0.78, "model_ver": "xgboost-v3.1", "reasons": [...]}
//
// 主路径 Screen 不直接调（5-50ms 延时打到 SLA）；推荐用法：
//   1. 包在 EnsembleService 里跟 LogisticService 一起跑（fail-open，
//      远程挂了就退到 logistic）
//   2. 或者放 ChampionChallengerService 的 challenger 槽位（不影响主路径）
//
// 性能：每条 Screen 一个 HTTP RPC 太重。生产建议改 gRPC + 长连接 + batch
// 接口（payment-core 把 N 笔交易 batch 一次调）。本实现是 JSON HTTP 单笔
// stub，方便接入测试；切 gRPC 时换 transport 不影响 Service interface。
package mlscore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// RemoteModelService HTTP client 实现 mlscore.Service 接口。
type RemoteModelService struct {
	BaseURL  string        // e.g. "http://xgboost-server:8000"
	ModelTag string        // 给 Result.ModelVer 的兜底（远程返了非空 model_ver 优先）
	Client   *http.Client  // 可注入自定义 transport / 超时
	Timeout  time.Duration // 缺省 100ms
}

// NewRemoteModelService 默认 100ms 超时 (XGBoost CPU 推理 < 50ms 一般够)。
func NewRemoteModelService(baseURL, modelTag string) *RemoteModelService {
	t := 100 * time.Millisecond
	return &RemoteModelService{
		BaseURL:  baseURL,
		ModelTag: modelTag,
		Client:   &http.Client{Timeout: t},
		Timeout:  t,
	}
}

// remoteScoreReq / Resp HTTP wire schema。
type remoteScoreReq struct {
	Features map[string]any `json:"features"`
}

type remoteScoreResp struct {
	Score    float64  `json:"score"`
	ModelVer string   `json:"model_ver"`
	Reasons  []string `json:"reasons"`
}

// Score 把 Features 序列化成 JSON map → POST → 解 score。
func (s *RemoteModelService) Score(ctx context.Context, f Features) (Result, error) {
	if s == nil || s.BaseURL == "" {
		return Result{}, errEmptyBaseURL
	}
	body, _ := json.Marshal(remoteScoreReq{Features: featuresToMap(f)})
	url := strings.TrimRight(s.BaseURL, "/") + "/score"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.Client.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return Result{}, fmt.Errorf("remote model HTTP %d: %s", resp.StatusCode, string(raw))
	}
	var r remoteScoreResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return Result{}, err
	}
	if r.ModelVer == "" {
		r.ModelVer = s.ModelTag
	}
	return Result{
		Score:    clamp01(r.Score),
		ModelVer: r.ModelVer,
	}, nil
}

// featuresToMap 把 mlscore.Features struct 转成 wire JSON map。命名跟
// 训练 pipeline (cmd/retrain) 输出对齐，让远程 model 的 feature_name 对
// 得上 — 改名时两边同步。
func featuresToMap(f Features) map[string]any {
	return map[string]any{
		"amount":               f.Amount,
		"ip_proxy":             f.IPProxy,
		"ip_vpn":               f.IPVPN,
		"ip_data_center":       f.IPDataCenter,
		"ip_country":           f.IPCountry,
		"country":              f.Country,
		"fingerprint_hash":     f.FingerprintHash,
		"webgl_renderer":       f.WebGLRenderer,
		"hardware_concurrency": f.HardwareConcurrency,
		"time_to_checkout_ms":  f.TimeToCheckoutMs,
		"mouse_entropy":        f.MouseMovementEntropy,
		"typing_rhythm_cv":     f.TypingRhythmCV,
		"keystroke_count":      f.KeystrokeCount,
	}
}

var errEmptyBaseURL = errEnsembleStr("RemoteModelService: BaseURL not set")
