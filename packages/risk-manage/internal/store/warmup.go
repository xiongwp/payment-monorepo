// warmup.go: Redis 重启 / failover 后 LinkStore warm-start。
//
// 商业风控 SOP 痛点：
//   Redis 重启 → LinkStore 全清 → register_velocity / fingerprint_multi_account /
//   link_fanout 等基于图边的规则瞬间无防御 → 攻击者 1-5min 黄金窗口
//
// 解法：从 audit 流（risk_decision audit ring buffer / ClickHouse 都行）拉最近
// N 小时的决策，把当时的 (customer, device, ip, fp, email, phone) 关联边重新
// Link() 进 LinkStore。Audit 行已经包含所有 PII pivot。
//
// 不需要 Counter（counter 只影响 velocity 笔数 / 累计金额，TTL 自然漂回；
// LinkStore 是图，TTL 1h 内空数据等于"白送窗口"）。
package store

import (
	"context"
	"strings"
	"time"
)

// AuditRow warmup 需要的最小字段子集（避免 audit pkg 循环 import）。
// 调用方拿 audit.MemSink.Recent / ClickHouse SELECT 后转换成本结构。
type AuditRow struct {
	OccurredAt time.Time
	MerchantID string
	CustomerID string
	IPAddress  string
	DeviceID   string
	// metadata 可选 — 解析里面的 fp / email / phone hash
	Metadata map[string]string
}

// Warmup 把 since 之后的 audit 行批量 Link 进 store。返回 写入的边数。
// 不阻塞主路径调；建议 main.go 启动后 fx OnStart 异步跑：
//
//	go func() {
//	  rows := pullRecentAudits(time.Now().Add(-1 * time.Hour))
//	  n := store.Warmup(ctx, links, rows)
//	  logger.Info("linkstore warmed", zap.Int("edges", n))
//	}()
//
// 失败 fail-open（Link 是 noop 安全），整个过程不返 error。
func Warmup(ctx context.Context, links LinkStore, rows []AuditRow) int {
	if links == nil || len(rows) == 0 {
		return 0
	}
	edges := 0
	for _, r := range rows {
		cust := nonEmpty("customer:", r.CustomerID)
		dev := nonEmpty("device:", r.DeviceID)
		ip := nonEmpty("ip:", r.IPAddress)
		mer := nonEmpty("merchant:", r.MerchantID)
		fp := metaPivot(r.Metadata, "fingerprint_hash", "fp:")
		em := metaPivot(r.Metadata, "email_hash", "email:")
		ph := metaPivot(r.Metadata, "phone_hash", "phone:")

		// 跟 service.Report 的 link 写入逻辑保持对齐（少了 card pivot — audit 一般
		// 不存 card_fingerprint 字段，PII 太敏感，所以 warmup 不能恢复 card 边）。
		linkPair(ctx, links, dev, cust, &edges)
		linkPair(ctx, links, ip, cust, &edges)
		linkPair(ctx, links, dev, ip, &edges)
		linkPair(ctx, links, dev, mer, &edges)
		linkPair(ctx, links, ip, mer, &edges)
		linkPair(ctx, links, cust, mer, &edges)
		if fp != "" {
			linkPair(ctx, links, fp, cust, &edges)
		}
		if em != "" {
			linkPair(ctx, links, em, cust, &edges)
			linkPair(ctx, links, em, dev, &edges)
		}
		if ph != "" {
			linkPair(ctx, links, ph, cust, &edges)
			linkPair(ctx, links, ph, dev, &edges)
		}
	}
	return edges
}

func linkPair(ctx context.Context, links LinkStore, a, b string, edges *int) {
	if a == "" || b == "" || a == b {
		return
	}
	links.Link(ctx, a, b)
	*edges++
}

func nonEmpty(prefix, value string) string {
	v := strings.TrimSpace(value)
	if v == "" {
		return ""
	}
	return prefix + v
}

func metaPivot(m map[string]string, key, prefix string) string {
	if m == nil {
		return ""
	}
	v := strings.TrimSpace(m[key])
	if v == "" {
		return ""
	}
	return prefix + v
}
