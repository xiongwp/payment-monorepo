//go:build !attest

package attestation

import (
	"context"

	"go.uber.org/zap"
)

// AppleClient / GoogleClient 在默认 build 是空壳类型；NewVerifier 直接走 stub 路径。
// 真实现见 apple_devicecheck.go / apple_appattest.go / google_playintegrity.go
// （`go build -tags attest` 才会启用）。

// AppleClient stub。
type AppleClient struct{}

// GoogleClient stub。
type GoogleClient struct{}

// NewAppleClient stub 构造器：永远返回 nil，让 NewVerifierWithClients 退回 stub 路径。
func NewAppleClient(_ *zap.Logger, _ AppleConfig) (*AppleClient, error) { return nil, nil }

// NewGoogleClient stub 构造器：同上。
func NewGoogleClient(_ *zap.Logger, _ GoogleConfig) (*GoogleClient, error) { return nil, nil }

// VerifyDeviceCheck stub：直接 trust。
func (c *AppleClient) VerifyDeviceCheck(_ context.Context, req Request) Result {
	return Result{Verified: true, Kind: req.Kind, Reason: "stub-trust"}
}

// VerifyAppAttest stub。
func (c *AppleClient) VerifyAppAttest(_ context.Context, req Request) Result {
	return Result{Verified: true, Kind: req.Kind, Reason: "stub-trust"}
}

// VerifyIntegrityToken stub。
func (c *GoogleClient) VerifyIntegrityToken(_ context.Context, req Request) Result {
	return Result{Verified: true, Kind: req.Kind, Reason: "stub-trust"}
}

// AppleConfig Apple Developer credentials；生产 build 才用到。
type AppleConfig struct {
	TeamID         string
	KeyID          string
	PrivateKeyPath string // .p8 文件路径
	BundleID       string // app bundle id, App Attest cross-check 用
	UseProduction  bool   // false = devicecheck.apple.com 走 development 端点
}

// GoogleConfig Play Integrity 凭证。
type GoogleConfig struct {
	ServiceAccountJSONPath string // GCP service account 私钥（json）
	PackageName            string // Android app package name
}
