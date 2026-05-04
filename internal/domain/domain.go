// Package domain 定义商户 / KYC / 渠道凭据等核心实体与通用类型。
package domain

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
)

// Metadata 字符串到字符串的 map，序列化为 JSON 列。
type Metadata map[string]string

// Value implements driver.Valuer
func (m Metadata) Value() (driver.Value, error) {
	if m == nil {
		return nil, nil
	}
	return json.Marshal(m)
}

// Scan implements sql.Scanner
func (m *Metadata) Scan(src any) error {
	if src == nil {
		*m = nil
		return nil
	}
	var data []byte
	switch v := src.(type) {
	case []byte:
		data = v
	case string:
		data = []byte(v)
	default:
		return errors.New("domain: unsupported Scan type for Metadata")
	}
	if len(data) == 0 {
		*m = nil
		return nil
	}
	return json.Unmarshal(data, m)
}

// ─── 错误 ─────────────────────────────────────────────────────────────────────

// ErrValidation 业务校验失败
var ErrValidation = errors.New("validation failed")
