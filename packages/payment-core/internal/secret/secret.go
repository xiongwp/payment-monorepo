// Package secret 桥接 viper 配置和 kmsclient —— 启动时把 `kms:v1:...` 密文
// 展开成明文，业务代码看到的 tokens / credentials 和历史写法完全一样。
//
// 两种用法：
//
//  1. Resolve(ctx, kms, "svc:paycore:auth", v.GetString("auth.token"))
//     —— 单个字段，手动指定 AAD context
//
//  2. ResolveStringSlice(ctx, kms, "svc:paycore:auth_tokens", v.GetStringSlice("auth.tokens"))
//     —— 一组字段，常见于多 token
//
// AAD context 约定：`svc:<service>:<field>`，举例：
//   - svc:paycore:auth_tokens
//   - svc:paycore:channel_endpoint_cred
//   - svc:paycore:mtls_key
package secret

import (
	"context"
	"fmt"
	"strings"

	"github.com/xiongwp/payment-core/internal/kmsclient"
)

// ciphertextPrefix 和 kms-manage/internal/cryptoenv.Prefix 保持一致。
// 没 import 那个 internal 包是因为 Go 的 internal/ 不允许跨模块引用。
// 两边同时改的风险低（线格式版本只 bump 一次就要动全量），值得一点重复。
const ciphertextPrefix = "kms:v1:"

func isCiphertext(s string) bool { return strings.HasPrefix(s, ciphertextPrefix) }

// Resolve 对单个值透明解密：
//   - 如果是明文（不带 kms:v1: 前缀），原样返回
//   - 否则调用 kms-manage 解密，失败整条链路向上抛
func Resolve(ctx context.Context, kms kmsclient.Client, aadContext, value string) (string, error) {
	if !isCiphertext(value) {
		return value, nil
	}
	if kms == nil {
		return "", fmt.Errorf("secret: value looks encrypted but no kms client wired: %s…", value[:min(20, len(value))])
	}
	plain, err := kms.Decrypt(ctx, value, aadContext)
	if err != nil {
		return "", fmt.Errorf("secret: decrypt %s: %w", aadContext, err)
	}
	return string(plain), nil
}

// ResolveStringSlice 对一组值透明解密（每项独立解密）。
// 空 slice 原样返回。
func ResolveStringSlice(ctx context.Context, kms kmsclient.Client, aadContext string, values []string) ([]string, error) {
	if len(values) == 0 {
		return values, nil
	}
	out := make([]string, len(values))
	for i, v := range values {
		p, err := Resolve(ctx, kms, aadContext, v)
		if err != nil {
			return nil, fmt.Errorf("[%d] %w", i, err)
		}
		out[i] = p
	}
	return out, nil
}

// ResolveBytes 同 Resolve，但返回 []byte；适合 TLS 私钥这种二进制内容。
func ResolveBytes(ctx context.Context, kms kmsclient.Client, aadContext, value string) ([]byte, error) {
	if !isCiphertext(value) {
		return []byte(value), nil
	}
	if kms == nil {
		return nil, fmt.Errorf("secret: value looks encrypted but no kms client wired")
	}
	return kms.Decrypt(ctx, value, aadContext)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
