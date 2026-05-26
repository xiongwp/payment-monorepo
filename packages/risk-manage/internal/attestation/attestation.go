// Package attestation 后端验签 iOS / Android 客户端上报的 attestation token。
//
// 三种 attestation 来源：
//
//	iOS DeviceCheck（iOS 11+）       → Apple Server-to-Server API
//	iOS App Attest  （iOS 14+）      → 本地验 X.509 cert chain + CBOR attestation object
//	Android Play Integrity API       → Google Play Developer API decode integrityToken
//
// 默认 build（无 //go:build attest）走 stub：直接 trust = true。生产构建用
// `go build -tags attest`，启用 apple_devicecheck.go / apple_appattest.go /
// google_playintegrity.go 真实现（这三个文件都加了 //go:build attest）。
//
// 调用入口：service.Screen / session.Create 拿到 SDK 上报的 token 后调
// Verifier.VerifyIOS / VerifyAndroid，返回 Result 写到 Snapshot.AttestationVerified +
// engine.TxnContext.AttestationVerified。
package attestation

import (
	"context"
	"errors"
	"os"
	"strings"

	"go.uber.org/zap"
)

// Kind attestation 来源类型；和 SDK 上报的 attestationKind 字段对齐。
type Kind string

const (
	KindNone          Kind = ""
	KindDeviceCheck   Kind = "device_check"
	KindAppAttest     Kind = "app_attest"
	KindPlayIntegrity Kind = "play_integrity"
)

// Request SDK 上报的原始 attestation 数据；caller 从 Snapshot 拼出来。
type Request struct {
	Platform string // "ios" / "android"
	Kind     Kind
	// iOS DeviceCheck token（base64）
	DeviceCheckToken string
	// iOS App Attest
	AppAttestToken     string // 首次 attestation object（base64）
	AppAttestKeyID     string // base64
	AppAttestAssertion string // 后续启动 assertion（base64）
	// Android Play Integrity
	PlayIntegrityToken string // JWE 字符串
	// 共享：服务端发的 challenge / nonce（防重放）
	Nonce string
	// 标识用：bundle id / package name 用来 cross-check attestation 里的 app id
	BundleID    string
	PackageName string
}

// Result 验签结果。
type Result struct {
	Verified bool
	Kind     Kind
	// Reason 失败原因（不阻塞流程时也 log，方便排查）。
	Reason string
	// DeviceIntegrity / AppIntegrity Play Integrity 解码后的字段，audit + 规则可用。
	DeviceIntegrity string // "MEETS_DEVICE_INTEGRITY" / "MEETS_BASIC_INTEGRITY" / "MEETS_STRONG_INTEGRITY"
	AppIntegrity    string // "PLAY_RECOGNIZED" / "UNRECOGNIZED_VERSION" / "UNEVALUATED"
	// AppAttestRPID iOS App Attest 验出的 receipt RP id（bundle id hash）。
	AppAttestRPID string
}

// Verifier 路由到具体 attestation 验签实现。
type Verifier struct {
	logger *zap.Logger
	apple  *AppleClient   // 真实现仅在 //go:build attest 时初始化非 nil
	google *GoogleClient  // 同上
	stub   bool           // 默认 build 为 true（不调外部）
	// Required 是否必须验签通过。配 RISK_ATTESTATION_REQUIRED=true 开启；
	// 此时空 token / 验签失败 → service.Screen 短路拒绝。
	Required bool
}

// NewVerifier 默认 stub 路径（无外部依赖）。生产用 NewVerifierWithClients
// 注入真实 AppleClient / GoogleClient（apple_devicecheck.go 等会在 attest tag
// 下导出这俩 client 类型）。
func NewVerifier(logger *zap.Logger) *Verifier {
	return &Verifier{
		logger:   logger,
		stub:     true,
		Required: parseRequired(),
	}
}

// NewVerifierWithClients 注入真实客户端的版本。stub=false。
// 仅在 //go:build attest 编译时 apple / google 非 nil；调用方应用同样 tag 构建。
func NewVerifierWithClients(logger *zap.Logger, apple *AppleClient, google *GoogleClient) *Verifier {
	return &Verifier{
		logger:   logger,
		apple:    apple,
		google:   google,
		stub:     apple == nil && google == nil,
		Required: parseRequired(),
	}
}

func parseRequired() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("RISK_ATTESTATION_REQUIRED")))
	switch v {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}

// Verify 路由到对应平台 / kind 的验签器。
//
// 行为矩阵：
//
//	stub 模式（默认 build）：
//	  - 有 token → Verified=true（信任 client），Reason="stub-trust"
//	  - 无 token → Verified=false，Reason="missing-token"
//
//	生产模式（//go:build attest）：
//	  - iOS device_check  → AppleClient.VerifyDeviceCheck
//	  - iOS app_attest    → AppleClient.VerifyAppAttest (含 assertion if present)
//	  - android play_int  → GoogleClient.VerifyIntegrityToken
//	  - 任何错误            → Verified=false，写 Reason
func (v *Verifier) Verify(ctx context.Context, req Request) Result {
	if req.Kind == KindNone || (req.DeviceCheckToken == "" && req.AppAttestToken == "" && req.PlayIntegrityToken == "") {
		return Result{Verified: false, Kind: req.Kind, Reason: "missing-token"}
	}
	if v.stub {
		// stub：信任 client；仅在 logger 标一下。生产 build 不会走这条。
		if v.logger != nil {
			v.logger.Debug("attestation stub-trust",
				zap.String("platform", req.Platform),
				zap.String("kind", string(req.Kind)),
			)
		}
		return Result{Verified: true, Kind: req.Kind, Reason: "stub-trust"}
	}
	switch req.Kind {
	case KindDeviceCheck:
		if v.apple == nil {
			return Result{Verified: false, Kind: req.Kind, Reason: "no-apple-client"}
		}
		return v.apple.VerifyDeviceCheck(ctx, req)
	case KindAppAttest:
		if v.apple == nil {
			return Result{Verified: false, Kind: req.Kind, Reason: "no-apple-client"}
		}
		return v.apple.VerifyAppAttest(ctx, req)
	case KindPlayIntegrity:
		if v.google == nil {
			return Result{Verified: false, Kind: req.Kind, Reason: "no-google-client"}
		}
		return v.google.VerifyIntegrityToken(ctx, req)
	}
	return Result{Verified: false, Kind: req.Kind, Reason: "unknown-kind"}
}

// IsAttestationRequired 透出 Required 给 service 层判断 fail-loud / fail-soft。
func (v *Verifier) IsAttestationRequired() bool { return v.Required }

// ErrAttestationRequired service.Screen 短路拒绝时返回；caller 应映射成 DENY decision。
var ErrAttestationRequired = errors.New("attestation required but verification failed")
