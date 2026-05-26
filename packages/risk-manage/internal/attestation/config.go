// config.go — attestation 凭证配置类型。
//
// 不带 build tag，两种 build 都用同一份字段定义。main.go wire 代码可以无
// 视 attest tag 引用这些类型；具体客户端类型 AppleClient / GoogleClient
// 在 clients_stub.go (默认 build) 或 apple_devicecheck.go / google_playintegrity.go
// (//go:build attest) 里独立声明。
package attestation

// AppleConfig Apple Developer 凭证（DeviceCheck + App Attest 共享）。
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
