// Package clients 聚合 admin BFF 用到的所有 Kitex RPC client.
//
// 全 monorepo Kitex 迁移后, 13 个 leaf service 全部走 Kitex. 这里 13 个字段对应:
//   - order-core (8 services): PaymentIntent / Charge / Refund / Webhook /
//     Audit / WebhookDelivery / Ledger / Dispute
//   - user-merchant-core (6 services): Merchant / MerchantSecret / Audit /
//     User / UserCard (UserCardInternal admin BFF 不直接调)
//   - payment-core: PaymentCore
//   - kms-manage: KMS
//   - risk-manage: Risk
//   - split-payment: SplitPaymentAdmin
package clients

import (
	// order-core (8 services in kitex_gen/order/v1/)
	auditservice "github.com/xiongwp/order-core/kitex_gen/order/v1/auditservice"
	chargeservice "github.com/xiongwp/order-core/kitex_gen/order/v1/chargeservice"
	disputeservice "github.com/xiongwp/order-core/kitex_gen/order/v1/disputeservice"
	ledgerservice "github.com/xiongwp/order-core/kitex_gen/order/v1/ledgerservice"
	paymentintentservice "github.com/xiongwp/order-core/kitex_gen/order/v1/paymentintentservice"
	refundservice "github.com/xiongwp/order-core/kitex_gen/order/v1/refundservice"
	webhookdeliveryservice "github.com/xiongwp/order-core/kitex_gen/order/v1/webhookdeliveryservice"
	webhookservice "github.com/xiongwp/order-core/kitex_gen/order/v1/webhookservice"

	// user-merchant-core (5 callable services, UserCardInternal 内部专用不暴露给 admin BFF)
	umAuditservice "github.com/xiongwp/user-merchant-core/kitex_gen/usermerchant/v1/auditservice"
	merchantsecretservice "github.com/xiongwp/user-merchant-core/kitex_gen/usermerchant/v1/merchantsecretservice"
	merchantservice "github.com/xiongwp/user-merchant-core/kitex_gen/usermerchant/v1/merchantservice"
	usercardservice "github.com/xiongwp/user-merchant-core/kitex_gen/usermerchant/v1/usercardservice"
	userservice "github.com/xiongwp/user-merchant-core/kitex_gen/usermerchant/v1/userservice"

	// Other Kitex services (1 service each)
	kmsservice "github.com/xiongwp/kms-manage/kitex_gen/kms/v1/kmsservice"
	paymentcoreservice "github.com/xiongwp/payment-core/kitex_gen/paymentcore/v1/paymentcoreservice"
	riskservice "github.com/xiongwp/risk-manage/kitex_gen/risk/v1/riskservice"
	// splitadminservice 暂时 stub: split-payment 还没生成 kitex_gen (无 proto IDL).
	// SplitPayment 字段类型用 any, handler 取出后必须 type assert + nil check.
	// splitadminservice "github.com/xiongwp/split-payment/kitex_gen/split_payment/v1/adminservice"
)

// Deps 是传给 handler 的一包 Kitex RPC 客户端, 生产时全部已 dial 好.
//
// 调用方式跟老 gRPC 形态一致 (c.PI.CreatePaymentIntent(ctx, req)), 不需要改 handler;
// 但 grpc.CallOption 可变参数没了 — 如果有显式传 grpc.CallOption 的地方需要删.
type Deps struct {
	// order-core 8 services
	PI              paymentintentservice.Client
	Charge          chargeservice.Client
	Refund          refundservice.Client
	WebhookDelivery webhookdeliveryservice.Client
	Audit           auditservice.Client
	Ledger          ledgerservice.Client
	Dispute         disputeservice.Client
	Webhook         webhookservice.Client

	// user-merchant-core 5 services
	Merchant          merchantservice.Client
	MerchantSecret    merchantsecretservice.Client
	UserMerchantAudit umAuditservice.Client
	User              userservice.Client
	UserCard          usercardservice.Client

	// 单 service
	PCore         paymentcoreservice.Client
	KMS           kmsservice.Client
	Risk          riskservice.Client
	// SplitPayment 是 split-payment.AdminService Kitex client. 当前 stub
	// (kitex_gen 未生成), 用 any 占位; handler 调用必须 nil check + type assert.
	SplitPayment  any
}
