// Package service contains accounting business logic. This file defines the
// CutDateProvider — a small read-cached source of truth for the day-cut tag
// that booking entries (and the TCC coordinator) carry.
//
// 设计要点：
//   - 业务侧（HybridDoubleEntryBooking / outbox persistEntry）在 booking 入口处
//     调用 CutDateNow() 决定本笔记账归属哪一日切（cut_date YYYY-MM-DD）。
//   - 该值随 bookingParams / SettlementEvent 透传到所有 per-shard Confirm tx，
//     最终写入 account_transaction.cut_date + tcc_coordinator.cut_date。
//   - 同 voucher 所有 entry 必然共享同一 cut_date → 日切扫描 WHERE cut_date=X
//     时整张凭证全入选或全不入 → 试算平衡天然成立。
//
// 配置（system_config 表）：
//   day_cut.scheduled_time = "10:00:00"  HH:MM:SS in TZ；当日早于此时间的请求归
//                                         "昨天" 业务日，等于/晚于的归 "今天"。
//   day_cut.timezone       = "Asia/Manila"  时区；非法值/空 → 服务器 Local。
//
// 热重载：SystemConfigService 监听到 KV 变化后调 Reload()，原子替换内部 snapshot。
package service

import (
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
)

// CutDateProvider 给 booking 路径用的 cut_date 计算器。
// 读 SystemConfigService 缓存（已 JSON-decode），无 DB 往返。
type CutDateProvider interface {
	CutDateNow() string
	// CutDateForTime 返回指定时刻对应的 cut_date。outbox persistEntry 用：
	// hot path 的 booking 时间是 SettlementEvent.TransactAt，不是 NOW()。
	CutDateForTime(t time.Time) string
}

// cutDateProvider 默认实现：每次 CutDateNow 都从 SystemConfigService 取最新值。
// SystemConfigService 已经在内存里维护一份 RWMutex 保护的 cache + 60s 兜底 reload + admin
// 写后扇出 reload —— 所以这里直接读它即可，无需自己再缓存一层。
type cutDateProvider struct {
	cfgSvc SystemConfigService
	logger *zap.Logger
}

// NewCutDateProvider 构造。cfgSvc 为 nil 时回落到默认（午夜 Local）。
func NewCutDateProvider(cfgSvc SystemConfigService, logger *zap.Logger) CutDateProvider {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &cutDateProvider{cfgSvc: cfgSvc, logger: logger}
}

func (p *cutDateProvider) CutDateNow() string {
	return p.CutDateForTime(time.Now())
}

func (p *cutDateProvider) CutDateForTime(t time.Time) string {
	h, m, sec, tz := p.snapshot()
	return ComputeCutDate(t, h, m, sec, tz)
}

// snapshot 读最新 day_cut config。每次调用都走 SystemConfigService.GetString，
// 后者从 RWMutex 保护的内存 map 取值，O(1) 无 DB 往返；admin 端改值后下一次
// 调用自动看到新值（admin-web 写完会扇出 reload）。
func (p *cutDateProvider) snapshot() (h, m, s int, tz *time.Location) {
	tz = time.Local
	if p.cfgSvc == nil {
		return 0, 0, 0, tz
	}
	timeStr := p.cfgSvc.GetString(ConfigKeyDayCutScheduledTime, defaultDayCutScheduledTime)
	tzStr := p.cfgSvc.GetString(ConfigKeyDayCutTimezone, "")
	if hh, mm, ss, ok := parseHHMMSS(timeStr); ok {
		h, m, s = hh, mm, ss
	}
	if tzStr != "" {
		if loc, err := time.LoadLocation(tzStr); err == nil {
			tz = loc
		}
	}
	return h, m, s, tz
}

// system_config keys + 默认值。集中定义便于 seed + 单点修改。
const (
	ConfigKeyDayCutEnabled       = "day_cut.enabled"
	ConfigKeyDayCutScheduledTime = "day_cut.scheduled_time"
	ConfigKeyDayCutTimezone      = "day_cut.timezone"
	ConfigKeyDayCutCurrencies    = "day_cut.currencies"

	defaultDayCutEnabled       = "false"
	defaultDayCutScheduledTime = "00:00:00"
	defaultDayCutTimezone      = "Local"
	defaultDayCutCurrencies    = "PHP"
)

// ComputeCutDate 是无状态的 cut_date 计算函数（导出便于测试）。
//
// 语义：以 (cutHour, cutMinute, cutSecond) 为日切 boundary。
//   - 当日 boundary 之前的请求 → 归属"昨天"业务日
//   - 当日 boundary 之时/之后  → 归属"今天"业务日
//
// 例：scheduled_time=10:00:00, tz=Asia/Manila
//   2026-04-27 09:59:59 → 2026-04-26
//   2026-04-27 10:00:00 → 2026-04-27
//   2026-04-27 10:00:01 → 2026-04-27
func ComputeCutDate(t time.Time, cutHour, cutMinute, cutSecond int, tz *time.Location) string {
	if tz == nil {
		tz = time.Local
	}
	local := t.In(tz)
	cutToday := time.Date(local.Year(), local.Month(), local.Day(),
		cutHour, cutMinute, cutSecond, 0, tz)
	if local.Before(cutToday) {
		return local.AddDate(0, 0, -1).Format("2006-01-02")
	}
	return local.Format("2006-01-02")
}

// parseHHMMSS 解析 "HH:MM:SS"。允许 "HH:MM"（秒填 0）。
// 出错或越界返回 ok=false，调用方 fallback 到默认 00:00:00。
func parseHHMMSS(s string) (h, m, sec int, ok bool) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 && len(parts) != 3 {
		return 0, 0, 0, false
	}
	hh, err := strconv.Atoi(parts[0])
	if err != nil || hh < 0 || hh > 23 {
		return 0, 0, 0, false
	}
	mm, err := strconv.Atoi(parts[1])
	if err != nil || mm < 0 || mm > 59 {
		return 0, 0, 0, false
	}
	ss := 0
	if len(parts) == 3 {
		ss, err = strconv.Atoi(parts[2])
		if err != nil || ss < 0 || ss > 59 {
			return 0, 0, 0, false
		}
	}
	return hh, mm, ss, true
}
