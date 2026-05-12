// gorm_models.go — GORM 映射 + ↔ domain 转换.

package domain

import "time"

type InternalTokenGormModel struct {
	Token          string     `gorm:"primaryKey;column:token;type:varchar(64)"`
	MerchantID     string     `gorm:"column:merchant_id;type:varchar(64);uniqueIndex:uk_merchant_pan,priority:1"`
	PANHash        string     `gorm:"column:pan_hash;type:char(64);uniqueIndex:uk_merchant_pan,priority:2"`
	PANLast4       string     `gorm:"column:pan_last4;type:char(4)"`
	BIN            string     `gorm:"column:bin;type:char(6)"`
	Brand          string     `gorm:"column:brand;type:varchar(16)"`
	ExpMonth       int        `gorm:"column:exp_month"`
	ExpYear        int        `gorm:"column:exp_year"`
	CardholderH    string     `gorm:"column:cardholder_h;type:char(32)"`
	Provider       string     `gorm:"column:provider;type:varchar(16);index:idx_provider_ref,priority:1"`
	NetworkToken   string     `gorm:"column:network_token;type:varchar(64)"`
	TokenRefID     string     `gorm:"column:token_ref_id;type:varchar(64);index:idx_provider_ref,priority:2"`
	TokenExpiry    string     `gorm:"column:token_expiry;type:char(4)"`
	ProvisionedAt  *time.Time `gorm:"column:provisioned_at"`
	Status         string     `gorm:"column:status;type:varchar(16);index:idx_status_time,priority:1"`
	CreatedAt      time.Time  `gorm:"column:created_at"`
	UpdatedAt      time.Time  `gorm:"column:updated_at;index:idx_status_time,priority:2"`
}

func (InternalTokenGormModel) TableName() string { return "internal_tokens" }

type EncryptedPANGormModel struct {
	Token      string    `gorm:"primaryKey;column:token;type:varchar(64)"`
	Ciphertext string    `gorm:"column:ciphertext;type:text"`
	CreatedAt  time.Time `gorm:"column:created_at"`
}

func (EncryptedPANGormModel) TableName() string { return "encrypted_pan" }

// ── 双向转换 ──

func FromInternalToken(t InternalToken) InternalTokenGormModel {
	m := InternalTokenGormModel{
		Token: t.Token, MerchantID: t.MerchantID, PANHash: t.PANHash,
		PANLast4: t.PANLast4, BIN: t.BIN, Brand: string(t.Brand),
		ExpMonth: t.ExpMonth, ExpYear: t.ExpYear, CardholderH: t.CardholderH,
		Status: string(t.Status), CreatedAt: t.Created_at, UpdatedAt: t.Updated_at,
	}
	if t.NetworkRef != nil {
		m.Provider = string(t.NetworkRef.Provider)
		m.NetworkToken = t.NetworkRef.NetworkToken
		m.TokenRefID = t.NetworkRef.TokenRefID
		m.TokenExpiry = t.NetworkRef.TokenExpiry
		pt := t.NetworkRef.ProvisionedAt
		m.ProvisionedAt = &pt
	}
	return m
}

func (m InternalTokenGormModel) ToDomain() InternalToken {
	t := InternalToken{
		Token: m.Token, MerchantID: m.MerchantID, PANHash: m.PANHash,
		PANLast4: m.PANLast4, BIN: m.BIN, Brand: CardBrand(m.Brand),
		ExpMonth: m.ExpMonth, ExpYear: m.ExpYear, CardholderH: m.CardholderH,
		Status: TokenStatus(m.Status), Created_at: m.CreatedAt, Updated_at: m.UpdatedAt,
	}
	if m.Provider != "" {
		ref := &NetworkRef{
			Provider:     TokenProvider(m.Provider),
			NetworkToken: m.NetworkToken,
			TokenRefID:   m.TokenRefID,
			TokenExpiry:  m.TokenExpiry,
		}
		if m.ProvisionedAt != nil {
			ref.ProvisionedAt = *m.ProvisionedAt
		}
		t.NetworkRef = ref
	}
	return t
}
