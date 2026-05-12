// helpers.go — sources 包公共工具.

package sources

import (
	"crypto/sha256"
	"encoding/hex"
)

func hash16(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}
