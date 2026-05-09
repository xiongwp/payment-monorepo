package sharding

// router_dispatch.go — Dual-read 期 V1/V2 路由分发助手。
//
// 用法：repo 在读写时调 RouteByPrefixedIDDual，传 V1 + V2 router 引用，
// 自动按 ID 字符串识别 layout 版本走对应 router。
//
// 写入侧：根据 CurrentLayoutVersion 决定用 V1 FormatID 还是 V2 FormatIDV2。
//
// 这两个函数是 dual-read SOP 的**单一入口**——确保任何路由 / 落表逻辑都走它，
// 否则 V1/V2 表混插会出 silent 数据漂移。

// RouteByPrefixedIDDual 自动按 ID 字符串识别 layout 版本，调相应 router。
//
//	id 是 V1 layout (e.g. "pi_337xxx") -> v1.RouteByPrefixedID
//	id 是 V2 layout (e.g. "pi_v2_03037_xxx") -> v2.RouteByPrefixedID
//
// v2 == nil 时（dual-read 未启用），即便检测到 V2 标记也回落 v1.RouteByString
// 兜底（保证不 panic；同时观测 metrics 上的 v2_seen_without_v2_router_total
// 推动告警）。
func RouteByPrefixedIDDual(v1 *Router, v2 *RouterV2, id string) (dbIndex, tableIndex int, version LayoutVersion) {
	if v1 == nil {
		v1 = NewRouter()
	}
	v := LayoutVersionOfID(id)
	if v == LayoutV2 {
		if v2 != nil {
			db, tbl := v2.RouteByPrefixedID(id)
			return db, tbl, LayoutV2
		}
		// dual-read 未启用却看到 V2 ID — 走 V1 hash 兜底。
		// caller 应监控并打 metric（v2_seen_without_v2_router_total）。
		db, tbl := v1.RouteByString(id)
		return db, tbl, LayoutV2
	}
	db, tbl := v1.RouteByPrefixedID(id)
	return db, tbl, LayoutV1
}

// FormatIDForCurrentLayout 按 CurrentLayoutVersion 选 V1 / V2 编码生成新 ID。
//
// 切换 V2 cutover 时，main.go 通过 SetCurrentLayoutVersion(LayoutV2) 一次性翻盘，
// 之后所有 service 的 FormatID 调用统一走 V2。reverse rollback 也只是再切回 V1，
// 老 V1 数据不动。
func FormatIDForCurrentLayout(v1 *Router, v2 *RouterV2, prefix string, dbIndex, tableIndex int, seq int64) string {
	switch CurrentLayoutVersion {
	case LayoutV2:
		if v2 != nil {
			return v2.FormatIDV2(prefix, dbIndex, tableIndex, seq)
		}
		// 配置错误（CurrentLayoutVersion=V2 但没注入 v2 router）→ 退回 V1，
		// 不要静默生成无法路由的 ID。
		fallthrough
	default:
		if v1 == nil {
			v1 = NewRouter()
		}
		return v1.FormatID(prefix, dbIndex, tableIndex, seq)
	}
}
