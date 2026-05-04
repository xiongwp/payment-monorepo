// Package clients 聚合 admin BFF 用到的所有 gRPC client。
package clients

import (
	kmsv1 "github.com/xiongwp/kms-manage/api/proto/kms/v1"
	orderv1 "github.com/xiongwp/order-core/api/proto/order/v1"
	paymentcorev1 "github.com/xiongwp/payment-core/api/proto/paymentcore/v1"
	riskv1 "github.com/xiongwp/risk-manage/api/proto/risk/v1"
	usermerchantv1 "github.com/xiongwp/user-merchant-core/api/proto/usermerchant/v1"
)

// Deps 是传给 handler 的一包 gRPC 客户端，生产时全部已 dial 好。
type Deps struct {
	PI              orderv1.PaymentIntentServiceClient
	Charge          orderv1.ChargeServiceClient
	Refund          orderv1.RefundServiceClient
	Merchant        usermerchantv1.MerchantServiceClient
	Audit           orderv1.AuditServiceClient
	UserMerchantAudit usermerchantv1.AuditServiceClient
	WebhookDelivery orderv1.WebhookDeliveryServiceClient
	Ledger          orderv1.LedgerServiceClient
	Dispute         orderv1.DisputeServiceClient
	MerchantSecret  usermerchantv1.MerchantSecretServiceClient
	PCore           paymentcorev1.PaymentCoreServiceClient
	KMS             kmsv1.KMSServiceClient
	Risk            riskv1.RiskServiceClient
}
