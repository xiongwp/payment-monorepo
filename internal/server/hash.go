package server

import (
	"crypto/sha256"
	"encoding/hex"
)

// sha256Hex: 跟 service.hashKey 一致的算法，copy 在 server 层是为了让 resolver
// 热路径不引用 service 层私有函数（保持单向依赖 server → service）。
func sha256Hex(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}
