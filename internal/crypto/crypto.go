// Package crypto 字段级 AES-GCM 加解密。与 order-core 的 crypto 一致，
// 用于保护 channel_token 等敏感字段。
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

var ErrCipherInvalid = errors.New("cipher: invalid payload")

type FieldCipher interface {
	Seal(plaintext []byte) (string, error)
	Open(encoded string) ([]byte, error)
}

type NoopCipher struct{}

func (NoopCipher) Seal(p []byte) (string, error) { return string(p), nil }
func (NoopCipher) Open(s string) ([]byte, error) { return []byte(s), nil }

type aesGCMCipher struct {
	gcm cipher.AEAD
}

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

func NewFromHexKey(hexKey string) (FieldCipher, error) {
	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, fmt.Errorf("decode hex key: %w", err)
	}
	return NewAESGCM(key)
}

func (c *aesGCMCipher) Seal(plaintext []byte) (string, error) {
	nonce := make([]byte, c.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := c.gcm.Seal(nonce, nonce, plaintext, nil)
	return "v1:" + base64.StdEncoding.EncodeToString(ct), nil
}

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
