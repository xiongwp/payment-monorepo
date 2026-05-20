module reconcile-system/packages/payment-gateway

go 1.22

require (
	go.uber.org/fx v1.20.1 // fx DI, 跟 order-core / accounting-system 同款
	go.uber.org/zap v1.27.0
)
