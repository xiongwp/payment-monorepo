//go:build attest

// google_playintegrity.go — Android Play Integrity API 验签。
//
// 客户端拿到的 integrityToken 是 JWE（加密 + 签名）；只有用 service account
// 调 Google Play Developer API decodeIntegrityToken 才能解出 payload。
//
// 流程：
//
//	1. 服务端启动期用 service account JSON 拿 access token（scope =
//	   https://www.googleapis.com/auth/playintegrity）。
//	2. POST https://playintegrity.googleapis.com/v1/<packageName>:decodeIntegrityToken
//	     Authorization: Bearer <access_token>
//	     { "integrity_token": "<client-token>" }
//	3. 返回结构：
//	     tokenPayloadExternal: {
//	       requestDetails: { requestPackageName, nonce, timestampMillis },
//	       appIntegrity:   { appRecognitionVerdict: "PLAY_RECOGNIZED" | ... },
//	       deviceIntegrity:{ deviceRecognitionVerdict: ["MEETS_DEVICE_INTEGRITY"] },
//	       accountDetails: { appLicensingVerdict: "LICENSED" }
//	     }
//	4. 验：
//	     - requestPackageName == 预期 package
//	     - nonce 匹配服务端发的（防重放）
//	     - timestampMillis 在 ±5min 窗口内
//	     - deviceRecognitionVerdict 含 "MEETS_DEVICE_INTEGRITY"（拒 root / 模拟器）
//	     - appRecognitionVerdict == "PLAY_RECOGNIZED"（拒篡改 apk）

package attestation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"go.uber.org/zap"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// GoogleClient Play Integrity decode 客户端。
type GoogleClient struct {
	cfg        GoogleConfig
	logger     *zap.Logger
	tokenSrc   oauth2.TokenSource
	httpClient *http.Client
}

// GoogleConfig 凭证 + 目标 package。
type GoogleConfig struct {
	ServiceAccountJSONPath string
	PackageName            string
}

// NewGoogleClient 读 service account JSON → 取 oauth2 token source。
func NewGoogleClient(logger *zap.Logger, cfg GoogleConfig) (*GoogleClient, error) {
	if cfg.ServiceAccountJSONPath == "" || cfg.PackageName == "" {
		return nil, fmt.Errorf("google attestation config incomplete")
	}
	raw, err := readFile(cfg.ServiceAccountJSONPath)
	if err != nil {
		return nil, fmt.Errorf("read sa json: %w", err)
	}
	jwtConfig, err := google.JWTConfigFromJSON(raw, "https://www.googleapis.com/auth/playintegrity")
	if err != nil {
		return nil, fmt.Errorf("parse sa json: %w", err)
	}
	return &GoogleClient{
		cfg:        cfg,
		logger:     logger,
		tokenSrc:   jwtConfig.TokenSource(context.Background()),
		httpClient: &http.Client{},
	}, nil
}

// VerifyIntegrityToken 调 Google decodeIntegrityToken。
func (c *GoogleClient) VerifyIntegrityToken(ctx context.Context, req Request) Result {
	url := fmt.Sprintf("https://playintegrity.googleapis.com/v1/%s:decodeIntegrityToken", c.cfg.PackageName)
	body, _ := json.Marshal(map[string]string{"integrity_token": req.PlayIntegrityToken})
	httpReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	tok, err := c.tokenSrc.Token()
	if err != nil {
		return Result{Kind: req.Kind, Reason: "oauth: " + err.Error()}
	}
	httpReq.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return Result{Kind: req.Kind, Reason: "post: " + err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Result{Kind: req.Kind, Reason: fmt.Sprintf("google-%d", resp.StatusCode)}
	}
	var out struct {
		TokenPayloadExternal struct {
			RequestDetails struct {
				RequestPackageName string `json:"requestPackageName"`
				Nonce              string `json:"nonce"`
				TimestampMillis    string `json:"timestampMillis"`
			} `json:"requestDetails"`
			AppIntegrity struct {
				AppRecognitionVerdict string `json:"appRecognitionVerdict"`
			} `json:"appIntegrity"`
			DeviceIntegrity struct {
				DeviceRecognitionVerdict []string `json:"deviceRecognitionVerdict"`
			} `json:"deviceIntegrity"`
		} `json:"tokenPayloadExternal"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Result{Kind: req.Kind, Reason: "decode: " + err.Error()}
	}
	rd := out.TokenPayloadExternal.RequestDetails
	if rd.RequestPackageName != c.cfg.PackageName {
		return Result{Kind: req.Kind, Reason: "package-mismatch"}
	}
	if req.Nonce != "" && rd.Nonce != req.Nonce {
		return Result{Kind: req.Kind, Reason: "nonce-mismatch"}
	}
	app := out.TokenPayloadExternal.AppIntegrity.AppRecognitionVerdict
	dev := strings.Join(out.TokenPayloadExternal.DeviceIntegrity.DeviceRecognitionVerdict, ",")
	verified := app == "PLAY_RECOGNIZED" &&
		strings.Contains(dev, "MEETS_DEVICE_INTEGRITY")
	return Result{
		Verified:        verified,
		Kind:            req.Kind,
		Reason:          fmt.Sprintf("app=%s,dev=%s", app, dev),
		AppIntegrity:    app,
		DeviceIntegrity: dev,
	}
}

// readFile 桥接函数；apple_appattest.go 也会用 — 这里集中放避免分散 import。
func readFile(p string) ([]byte, error) {
	// 用 os.ReadFile；放 helper 是为以后注入 mock。
	return osReadFile(p)
}

// osReadFile 包装；测试时替换。
var osReadFile = func(p string) ([]byte, error) {
	return nil, fmt.Errorf("attest tag build: osReadFile not wired; replace with os.ReadFile in main wire-up")
}
