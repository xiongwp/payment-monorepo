// Package crypto — 数据加密 (AES-256-GCM, DEK 每次新随机 nonce).
//
// 用途: vault 内部对 PAN 加密落 DB. DEK 包了一层 KEK (由 kms-manage envelope 解, 这里不直接做).
// 这里只暴露 EncryptPAN / DecryptPAN — 由调用方提供 DEK (32 bytes).

package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
)

// EncryptPAN aes-256-gcm 加密 (含 12 字节随机 nonce 前缀).
// 返回 base64(nonce || ciphertext || tag).
func EncryptPAN(pan string, dek []byte) (string, error) {
	if len(dek) != 32 {
		return "", errors.New("dek must be 32 bytes")
	}
	if pan == "" {
		return "", errors.New("empty pan")
	}
	block, err := aes.NewCipher(dek)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := gcm.Seal(nil, nonce, []byte(pan), nil)
	out := append(nonce, ct...)
	return base64.StdEncoding.EncodeToString(out), nil
}

// DecryptPAN 反向.
func DecryptPAN(enc string, dek []byte) (string, error) {
	if len(dek) != 32 {
		return "", errors.New("dek must be 32 bytes")
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(dek)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	ns := gcm.NonceSize()
	if len(raw) < ns+16 {
		return "", errors.New("ciphertext too short")
	}
	nonce, ct := raw[:ns], raw[ns:]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}
