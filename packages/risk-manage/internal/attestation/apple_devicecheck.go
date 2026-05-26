//go:build attest

// apple_devicecheck.go — iOS DeviceCheck Server-to-Server API 验签。
//
// 流程：
//
//	1. 服务端用 Apple Developer 后台下载的 .p8 私钥（AuthKey_XXXX.p8）+
//	   TeamID + KeyID 签一个 ES256 JWT（aud=apple, iss=TeamID, kid=KeyID）。
//	2. POST https://api.devicecheck.apple.com/v1/validate_device_token
//	     Authorization: Bearer <jwt>
//	     {
//	       "device_token": "<base64-client-token>",
//	       "transaction_id": "<random uuid>",
//	       "timestamp": <unix-ms>
//	     }
//	3. Apple 返 200 = token valid；返 400/401 = token invalid 或 jwt 错。
//	4. 验通过后可选 update_two_bits → 写 2 bit per-device 标记（黑名单 / 已退款 / 已 chargeback）。
//
// 端点：
//   - https://api.devicecheck.apple.com/v1/validate_device_token         （生产）
//   - https://api.development.devicecheck.apple.com/v1/validate_device_token （开发）
//
// 配置文件 .p8 长这样：
//
//	-----BEGIN PRIVATE KEY-----
//	MIGTAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBHkwdwIBAQQg...
//	-----END PRIVATE KEY-----

package attestation

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"go.uber.org/zap"
)

// AppleClient Apple DeviceCheck / App Attest 客户端。
// AppleConfig / GoogleConfig 类型定义在 clients_stub.go（无 build tag 时也存在），
// 这里只重新声明字段化的 struct（attest tag 下 stub 的 struct 被替换为本 struct）。
type AppleClient struct {
	cfg        AppleConfig
	logger     *zap.Logger
	privateKey *ecdsa.PrivateKey // 启动期从 .p8 解析；后续签 JWT 重用
	endpoint   string
	httpClient *http.Client
}

// NewAppleClient 启动期解析 .p8 + 选定端点。失败 → 返 error，service 层应记 warn
// 但不阻塞启动（attestation 默认 fail-soft）。
func NewAppleClient(logger *zap.Logger, cfg AppleConfig) (*AppleClient, error) {
	if cfg.PrivateKeyPath == "" || cfg.TeamID == "" || cfg.KeyID == "" {
		return nil, fmt.Errorf("apple attestation config incomplete")
	}
	raw, err := os.ReadFile(cfg.PrivateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("read p8: %w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("decode p8 pem failed")
	}
	priv, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse p8: %w", err)
	}
	ec, ok := priv.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("p8 not ecdsa key")
	}
	endpoint := "https://api.devicecheck.apple.com/v1/validate_device_token"
	if !cfg.UseProduction {
		endpoint = "https://api.development.devicecheck.apple.com/v1/validate_device_token"
	}
	return &AppleClient{
		cfg:        cfg,
		logger:     logger,
		privateKey: ec,
		endpoint:   endpoint,
		httpClient: &http.Client{Timeout: 8 * time.Second},
	}, nil
}

// signJWT ES256(header={alg:ES256,kid:KeyID}, claims={iss:TeamID, iat:now, aud:apple}).
// Apple JWT 6 个月内有效；为避免每次签都新算，可缓存 + 过期前 5 分钟刷新。
// 此处简化每次签一遍（API 量级一秒几千笔以下没问题）。
func (c *AppleClient) signJWT() (string, error) {
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"iss": c.cfg.TeamID,
		"iat": time.Now().Unix(),
	})
	tok.Header["kid"] = c.cfg.KeyID
	return tok.SignedString(c.privateKey)
}

// VerifyDeviceCheck POST 到 Apple，200 = valid。
func (c *AppleClient) VerifyDeviceCheck(ctx context.Context, req Request) Result {
	jwtStr, err := c.signJWT()
	if err != nil {
		return Result{Kind: req.Kind, Reason: "sign-jwt: " + err.Error()}
	}
	body := map[string]any{
		"device_token":   req.DeviceCheckToken,
		"transaction_id": newTxnID(),
		"timestamp":      time.Now().UnixMilli(),
	}
	raw, _ := json.Marshal(body)
	httpReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(raw))
	httpReq.Header.Set("Authorization", "Bearer "+jwtStr)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return Result{Kind: req.Kind, Reason: "post: " + err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return Result{Verified: true, Kind: req.Kind, Reason: "apple-200"}
	}
	return Result{Kind: req.Kind, Reason: fmt.Sprintf("apple-%d", resp.StatusCode)}
}

// newTxnID 简单的 random uuid 用作 Apple transaction_id；防 Apple 端去重误判。
func newTxnID() string {
	var b [16]byte
	_, _ = randomRead(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}
