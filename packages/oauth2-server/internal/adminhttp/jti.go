// jti.go — JWT ID 生成. RFC 7519 §4.1.7 要求 jti 全局唯一防重放.
//
// 实现: 16 字节 crypto/rand → hex 编码 (32 字符).
// 单服务每秒 1M qps 撞 jti 的概率 ~ 2^-65 → 工程零冲突.

package adminhttp

import (
	"crypto/rand"
	"encoding/hex"
)

// randomJTI 返回 32 字符 hex 字符串. crypto/rand 失败 panic — entropy 不足时
// 颁 token 也不安全, fail fast.
func randomJTI() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("randomJTI: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}
