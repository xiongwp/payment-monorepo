// Package clients 聚合 admin BFF 用到的所有 RPC client.
//
// 当前混合状态: KMS 已切 Kitex (kmsservice.Client); 其它 7 个 service 仍是 gRPC
// (待 KX-3/4/5/... 后逐个换). idl/MIGRATION.md 跟踪推进.
package clients

import (
	orderv1 "github.com/xiongwp/order-core/api/proto/order/v1"
	paymentcorev1 "github.com/xiongwp/payment-core/api/proto/paymentcore/v1"
	usermerchantv1 "github.com/xiongwp/user-merchant-core/api/proto/usermerchant/v1"

	// Kitex 已切的服务用 kitex_gen 路径:
	kmsservice "reconcile-system/packages/kms-manage/kitex_gen/kms/v1/kmsservice"
	riskservice "reconcile-system/packages/risk-manage/kitex_gen/risk/v1/riskservice"
)

// Deps 是传给 handler 的一包 RPC 客户端, 生产时全部已 dial 好.
//
// 注: KMS 字段类型从 kmsv1.KMSServiceClient (gRPC) 切到 kmsservice.Client (Kitex).
// 上游 handler 的调用代码 (c.KMS.Encrypt(ctx, req)) 形态一致, 不用改; 但 grpc.CallOption
// 类型的可变参数没了 — 如果有显式传 grpc.CallOption 的地方需要删.
type Deps struct {
	PI                orderv1.PaymentIntentServiceClient
	Charge            orderv1.ChargeServiceClient
	Refund            orderv1.RefundServiceClient
	Merchant          usermerchantv1.MerchantServiceClient
	Audit             orderv1.AuditServiceClient
	UserMerchantAudit usermerchantv1.AuditServiceClient
	WebhookDelivery   orderv1.WebhookDeliveryServiceClient
	Ledger            orderv1.LedgerServiceClient
	Dispute           orderv1.DisputeServiceClient
	MerchantSecret    usermerchantv1.MerchantSecretServiceClient
	PCore             paymentcorev1.PaymentCoreServiceClient

	KMS  kmsservice.Client  // Kitex
	Risk riskservice.Client // Kitex
}
