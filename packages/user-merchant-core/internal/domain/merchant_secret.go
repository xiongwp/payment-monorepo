package domain

import (
	"errors"
	"time"
)

// ErrMerchantSecretNotFound 商户渠道凭据不存在
var ErrMerchantSecretNotFound = errors.New("merchant secret not found")

// MerchantChannelSecret 商户渠道凭据（密文）。
//
// 写入路径：admin 提供明文 → service 调 KMS Encrypt → 存本行。
// 读取路径：
//   - admin UI 看的是 masked_hint，永不拿原文；
//   - payment-channel adapter init 时通过 internal RPC 拿原文（需单独 bearer）。
//
// 同一 (merchant_id, channel, field_name) 只能一行；update = 新版本覆盖。
type MerchantChannelSecret struct {
	ID         int64     `gorm:"column:id;primaryKey;autoIncrement"    json:"id"`
	MerchantID string    `gorm:"column:merchant_id;type:varchar(32)"   json:"merchant_id"`
	Channel    string    `gorm:"column:channel;type:varchar(32)"       json:"channel"`
	FieldName  string    `gorm:"column:field_name;type:varchar(64)"    json:"field_name"`
	Ciphertext []byte    `gorm:"column:ciphertext;type:varbinary(4096)" json:"-"`
	Context    string    `gorm:"column:context;type:varchar(256)"      json:"-"`
	MaskedHint string    `gorm:"column:masked_hint;type:varchar(64)"   json:"masked_hint,omitempty"`
	Version    int       `gorm:"column:version"                        json:"version"`
	CreatedBy  string    `gorm:"column:created_by;type:varchar(64)"    json:"created_by,omitempty"`
	Created    time.Time `gorm:"column:created;autoCreateTime"         json:"created"`
	Updated    time.Time `gorm:"column:updated;autoUpdateTime"         json:"updated"`
}

// TableName GORM
func (MerchantChannelSecret) TableName() string { return "merchant_channel_secret" }

// MaskSecret produces the UI-visible hint from a plaintext value. First 4
// characters + "***" + last 3 when long enough; fully redacted otherwise.
// Not cryptographic — just a UX convenience so ops can tell at a glance
// whether rotation actually changed the stored key.
func MaskSecret(plaintext string) string {
	if len(plaintext) <= 8 {
		return "***"
	}
	return plaintext[:4] + "***" + plaintext[len(plaintext)-3:]
}
