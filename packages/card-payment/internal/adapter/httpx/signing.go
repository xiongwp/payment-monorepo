// signing.go：5 家卡组织签名共用工具。
//
// 各家差异：
//   Visa Net (CyberSource):    HTTP Signature (RSA-SHA256, message digest in headers)
//   Mastercard MIP/MPGS:       OAuth 1.0a body hash + RSA-SHA256
//   Amex API:                  HMAC-SHA256 (api_secret + path + body + ts)
//   JCB Net:                   HMAC-SHA256（J/Smart 与 Amex 类似）
//   UnionPay:                  RSA-SHA256（form 字段 sorted KV → sign）
//
// 本文件提供原子操作（HMAC / RSA sign / canonical-string 构造），
// 各 adapter 自己拼最终 header。
package httpx

import (
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
)

// HMACSHA256Hex 给 Amex / JCB 用：hex(hmac_sha256(key, message))
func HMACSHA256Hex(key, message []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write(message)
	return hex.EncodeToString(mac.Sum(nil))
}

// HMACSHA256Base64 同上但 base64 输出（部分 Visa headers 要 base64）
func HMACSHA256Base64(key, message []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write(message)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// SHA256Base64 给 Visa / MIP digest header：base64(sha256(body))
// 用于 HTTP Signature 的 Digest: SHA-256=... header。
func SHA256Base64(body []byte) string {
	sum := sha256.Sum256(body)
	return base64.StdEncoding.EncodeToString(sum[:])
}

// LoadRSAPrivateKey PEM file → *rsa.PrivateKey。Visa / MIP / 银联 共用。
func LoadRSAPrivateKey(path string) (*rsa.PrivateKey, error) {
	if path == "" {
		return nil, errors.New("rsa key: path empty")
	}
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("rsa key read: %w", err)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("rsa key: PEM decode failed")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rsaKey, ok := k.(*rsa.PrivateKey); ok {
			return rsaKey, nil
		}
		return nil, errors.New("rsa key: PKCS8 not RSA")
	}
	return nil, errors.New("rsa key: parse failed (PKCS1 + PKCS8)")
}

// RSASignSHA256 RSA-SHA256 签名 → base64。Visa / MIP / 银联 共用。
//
// 这是 PKCS#1 v1.5 padding（SignPKCS1v15）。Mastercard 部分新接口用
// PSS padding，需另外封装；目前主流 MIP/CyberSource 都还是 v1.5。
func RSASignSHA256(key *rsa.PrivateKey, message []byte) (string, error) {
	if key == nil {
		return "", errors.New("rsa sign: nil key")
	}
	hashed := sha256.Sum256(message)
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hashed[:])
	if err != nil {
		return "", fmt.Errorf("rsa sign: %w", err)
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

// SortedFormString 银联签名前置：把 form values 按 key 升序拼成
// "k1=v1&k2=v2"（不 URL-encode value，跟 UPI 协议 spec 一致）。
//
// 跳过 signature / key 自身。空 value 跳过。
func SortedFormString(values url.Values, skip ...string) string {
	skipSet := make(map[string]struct{}, len(skip))
	for _, s := range skip {
		skipSet[s] = struct{}{}
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		if _, sk := skipSet[k]; sk {
			continue
		}
		if values.Get(k) == "" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+values.Get(k))
	}
	return strings.Join(parts, "&")
}

// CanonicalRequestForVisa 给 Visa Net HTTP Signature 拼 (request-target) +
// 各 header 的 canonical 字符串。Visa 文档参考：
// "(request-target): post /pts/v2/payments\nhost: api.visa.com\ndate: ...\ndigest: SHA-256=..."
func CanonicalRequestForVisa(method, path, host, date, digest string) string {
	method = strings.ToLower(method)
	return fmt.Sprintf(
		"(request-target): %s %s\nhost: %s\ndate: %s\ndigest: %s",
		method, path, host, date, digest,
	)
}
