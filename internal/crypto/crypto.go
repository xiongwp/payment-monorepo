// Package crypto 提供敏感字段的加解密（AES-GCM）。
//
// 落库字段（ClientSecret / PayAction.ExpectedSecret / PaymentMethodRef 等）应通过
// FieldCipher.Seal 加密后存库，读取时 Open 解密。
//
// 生产环境：Key 应通过 KMS / Vault 注入，服务重启时从 secret manager 加载；
// 开发环境：可由 env 变量 ORDERCORE_FIELD_KEY 提供 32 字节 hex 字符串。
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
)

// ErrCipherInvalid 密文格式损坏
var ErrCipherInvalid = errors.New("cipher: invalid payload")

// FieldCipher AES-GCM 字段级加密
type FieldCipher interface {
	// Seal 返回 "v1:<base64-nonce-ciphertext>" 格式
	Seal(plaintext []byte) (string, error)
	// Open 解析 "v1:..."
	Open(encoded string) ([]byte, error)
}

// NoopCipher 不加密（本地开发 / 测试用）
type NoopCipher struct{}

// Seal 返回原文
func (NoopCipher) Seal(p []byte) (string, error) { return string(p), nil }

// Open 直接转成 bytes
func (NoopCipher) Open(s string) ([]byte, error) { return []byte(s), nil }

type aesGCMCipher struct {
	gcm cipher.AEAD
}

// NewAESGCM 用 32 字节 key 构造 AES-256-GCM
func NewAESGCM(key []byte) (FieldCipher, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("cipher: key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &aesGCMCipher{gcm: g}, nil
}

// NewFromHexKey "64-char hex" → AES-GCM
func NewFromHexKey(hexKey string) (FieldCipher, error) {
	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, fmt.Errorf("decode hex key: %w", err)
	}
	return NewAESGCM(key)
}

// Seal 加密
func (c *aesGCMCipher) Seal(plaintext []byte) (string, error) {
	nonce := make([]byte, c.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := c.gcm.Seal(nonce, nonce, plaintext, nil)
	return "v1:" + base64.StdEncoding.EncodeToString(ct), nil
}

// Open 解密
func (c *aesGCMCipher) Open(encoded string) ([]byte, error) {
	if len(encoded) < 4 || encoded[:3] != "v1:" {
		return nil, ErrCipherInvalid
	}
	raw, err := base64.StdEncoding.DecodeString(encoded[3:])
	if err != nil {
		return nil, ErrCipherInvalid
	}
	ns := c.gcm.NonceSize()
	if len(raw) < ns {
		return nil, ErrCipherInvalid
	}
	nonce, ct := raw[:ns], raw[ns:]
	return c.gcm.Open(nil, nonce, ct, nil)
}
