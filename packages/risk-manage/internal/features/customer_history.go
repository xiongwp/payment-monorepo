package features

import (
	"context"

	"github.com/xiongwp/risk-manage/internal/engine"
	"github.com/xiongwp/risk-manage/internal/store"
)

// CustomerHistoryExtractor 用 LinkStore + Counter 算客户聚合特征。
//
// 输出字段：
//   CustomerPaidCount90d     ← Counter.GetMonthly(customer:X) > 0 笔数（粗）
//   CustomerDistinctMerchants90d ← LinkStore.Peers(customer:X, "merchant:")
//   CustomerDistinctDevices90d   ← LinkStore.Peers(customer:X, "device:")
//   CustomerDistinctIPs90d       ← LinkStore.Peers(customer:X, "ip:")
//   IsNewDevice                  ← txn.DeviceID 不在 customer 已知 device 列表里
//   IsNewIP                      ← txn.IPAddress 不在 customer 已知 IP 列表里
//
// 注意：
//   - LinkStore TTL 1 小时，所以 "90d" 标签是希望中的（要 Redis backed 才真到 90d）
//     当前 Mem 实现下读到的是"过去 1 小时"。生产把 LinkStore 换成 Redis 后语义自然达成。
//   - paid_count_90d / chargeback_count_90d 商户最好直接在 metadata 传，比从内部
//     聚合更准确（RDBMS / Spark 离线算）。本 extractor 只在商户没传时兜底。
type CustomerHistoryExtractor struct {
	links   store.LinkStore
	counter store.Counter
}

func NewCustomerHistoryExtractor(links store.LinkStore, counter store.Counter) *CustomerHistoryExtractor {
	return &CustomerHistoryExtractor{links: links, counter: counter}
}

func (e *CustomerHistoryExtractor) Name() string { return "customer_history" }

func (e *CustomerHistoryExtractor) Enrich(ctx context.Context, txn *engine.TxnContext) {
	if txn == nil || txn.CustomerID == "" {
		return
	}
	custKey := "customer:" + txn.CustomerID

	if e.links != nil {
		merchants := e.links.Peers(ctx, custKey, "merchant:")
		devices := e.links.Peers(ctx, custKey, "device:")
		ips := e.links.Peers(ctx, custKey, "ip:")
		if txn.CustomerDistinctMerchants90d == 0 {
			txn.CustomerDistinctMerchants90d = len(merchants)
		}
		if txn.CustomerDistinctDevices90d == 0 {
			txn.CustomerDistinctDevices90d = len(devices)
		}
		if txn.CustomerDistinctIPs90d == 0 {
			txn.CustomerDistinctIPs90d = len(ips)
		}

		// IsNewDevice / IsNewIP：当前 txn 的 device/IP 是否在已知列表中
		txn.IsNewDevice = txn.DeviceID != "" && !contains(devices, "device:"+txn.DeviceID)
		txn.IsNewIP = txn.IPAddress != "" && !contains(ips, "ip:"+txn.IPAddress)
	}

	if e.counter != nil && txn.CustomerPaidCount90d == 0 {
		// Counter 是按金额累计的；笔数得另外维护，当前不直接拿；用 daily/monthly
		// 是否非零做粗略 "曾经支付过" 信号
		if e.counter.GetMonthly(ctx, custKey) > 0 {
			txn.CustomerPaidCount90d = 1 // 至少 1（粗）；商户传 metadata.customer_paid_count_90d 更准
		}
	}
}

func contains(slice []string, v string) bool {
	for _, s := range slice {
		if s == v {
			return true
		}
	}
	return false
}
