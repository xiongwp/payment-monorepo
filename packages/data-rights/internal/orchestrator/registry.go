// registry.go — 默认 service registry. 真生产从 config-center 读.

package orchestrator

import "reconcile-system/packages/data-rights/internal/domain"

// DefaultRegistry 列默认 service 列表 (dev). 生产用 config-center.GetJSON 读.
func DefaultRegistry() domain.ServiceRegistry {
	return domain.ServiceRegistry{
		Services: []domain.ServiceEntry{
			{
				Name:            "user-merchant-core",
				BaseURL:         "http://user-merchant-core:9191",
				SupportsAccess:  true,
				SupportsErasure: true,
				SubjectTypes:    []domain.SubjectType{domain.SubjectMerchant, domain.SubjectCustomer},
			},
			{
				Name:            "order-core",
				BaseURL:         "http://order-core:9091",
				SupportsAccess:  true,
				SupportsErasure: true, // partial: AML 7y hold
				SubjectTypes:    []domain.SubjectType{domain.SubjectCustomer, domain.SubjectMerchant},
			},
			{
				Name:            "payment-core",
				BaseURL:         "http://payment-core:9090",
				SupportsAccess:  true,
				SupportsErasure: true, // partial
				SubjectTypes:    []domain.SubjectType{domain.SubjectCustomer, domain.SubjectMerchant},
			},
			{
				Name:            "billing-system",
				BaseURL:         "http://billing-system:8081",
				SupportsAccess:  true,
				SupportsErasure: true, // partial: 7y tax retention
				SubjectTypes:    []domain.SubjectType{domain.SubjectMerchant},
			},
			{
				Name:            "kyc-service",
				BaseURL:         "http://kyc-service:8084",
				SupportsAccess:  true,
				SupportsErasure: false, // 不能删 — AML / 反欺诈黑名单永久
				SubjectTypes:    []domain.SubjectType{domain.SubjectMerchant, domain.SubjectCustomer},
			},
			{
				Name:            "tokenization-vault",
				BaseURL:         "http://tokenization-vault:8089",
				SupportsAccess:  true,  // 返 last4 / brand (PAN 不返)
				SupportsErasure: true,  // 卡 token 完全可删 (issuer 自己处理 PAN)
				SubjectTypes:    []domain.SubjectType{domain.SubjectCustomer},
			},
			{
				Name:            "merchant-webhook",
				BaseURL:         "http://merchant-webhook:8080",
				SupportsAccess:  true,
				SupportsErasure: true,
				SubjectTypes:    []domain.SubjectType{domain.SubjectMerchant},
			},
			{
				Name:            "subscription",
				BaseURL:         "http://subscription:8085",
				SupportsAccess:  true,
				SupportsErasure: true,
				SubjectTypes:    []domain.SubjectType{domain.SubjectMerchant, domain.SubjectCustomer},
			},
			{
				Name:            "wallet-service",
				BaseURL:         "http://wallet-service:8086",
				SupportsAccess:  true,
				SupportsErasure: false, // 余额未结清不能删, ops review
				SubjectTypes:    []domain.SubjectType{domain.SubjectCustomer, domain.SubjectMerchant},
			},
			{
				Name:            "audit-log",
				BaseURL:         "http://audit-log:8087",
				SupportsAccess:  true,
				SupportsErasure: false, // 审计 hash 链不可删, 改为脱敏字段 (tombstone)
				SubjectTypes:    []domain.SubjectType{domain.SubjectMerchant, domain.SubjectCustomer, domain.SubjectStaff},
			},
		},
	}
}
