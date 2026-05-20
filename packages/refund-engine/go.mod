module reconcile-system/packages/refund-engine

go 1.22

require (
	github.com/xiongwp/payment-util v0.0.1
	go.uber.org/fx v1.20.1

	go.uber.org/zap v1.27.0
)

// SP-AC-7 SHARED-3: obsbootstrap 需要 payment-util.
replace github.com/xiongwp/payment-util => ../payment-util
