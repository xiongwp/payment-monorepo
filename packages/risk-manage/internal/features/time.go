package features

import (
	"context"
	"strings"
	"time"

	"github.com/xiongwp/risk-manage/internal/engine"
)

// TimeExtractor 算 HourOfDay / DayOfWeek / IsWeekend / IsBusinessHours。
//
// 时区策略：
//   1. 若 txn.Timezone (IANA) 非空 → 用 it（SDK 上报的本地时区）
//   2. 否则若 txn.IPCountry 非空 → 推断典型时区（粗：每国一个代表市）
//   3. 否则用 UTC
//
// 业务时间窗：默认 9:00-21:00 local（多数零售商户有效）；商户级 override
// 通过配置传入（BusinessHours{Start, End}）。
type TimeExtractor struct {
	Now           func() time.Time // for test override；nil → time.Now
	BusinessStart int              // 默认 9
	BusinessEnd   int              // 默认 21
}

// NewTimeExtractor 默认 9-21 业务时段。
func NewTimeExtractor() *TimeExtractor {
	return &TimeExtractor{BusinessStart: 9, BusinessEnd: 21}
}

func (e *TimeExtractor) Name() string { return "time" }

func (e *TimeExtractor) Enrich(_ context.Context, txn *engine.TxnContext) {
	if txn == nil {
		return
	}
	now := time.Now()
	if e.Now != nil {
		now = e.Now()
	}
	loc := time.UTC
	if txn.Timezone != "" {
		if l, err := time.LoadLocation(txn.Timezone); err == nil {
			loc = l
		}
	} else if tz := tzForCountry(txn.IPCountry); tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	}
	t := now.In(loc)
	txn.HourOfDay = t.Hour()
	txn.DayOfWeek = int(t.Weekday())
	txn.IsWeekend = txn.DayOfWeek == 0 || txn.DayOfWeek == 6
	bs, be := e.BusinessStart, e.BusinessEnd
	if bs == 0 && be == 0 {
		bs, be = 9, 21
	}
	txn.IsBusinessHours = txn.HourOfDay >= bs && txn.HourOfDay < be
}

// tzForCountry 粗略 ISO-2 → 时区 fallback（每国一个代表）。
// 不上完整 GeoIP+timezone DB，对风控规则够用。
func tzForCountry(c string) string {
	switch strings.ToUpper(c) {
	case "CN":
		return "Asia/Shanghai"
	case "PH":
		return "Asia/Manila"
	case "HK":
		return "Asia/Hong_Kong"
	case "JP":
		return "Asia/Tokyo"
	case "SG", "MY":
		return "Asia/Singapore"
	case "ID":
		return "Asia/Jakarta"
	case "TH":
		return "Asia/Bangkok"
	case "VN":
		return "Asia/Ho_Chi_Minh"
	case "IN":
		return "Asia/Kolkata"
	case "GB":
		return "Europe/London"
	case "DE":
		return "Europe/Berlin"
	case "FR":
		return "Europe/Paris"
	case "RU":
		return "Europe/Moscow"
	case "US":
		return "America/New_York"
	case "CA":
		return "America/Toronto"
	case "MX":
		return "America/Mexico_City"
	case "BR":
		return "America/Sao_Paulo"
	case "AU":
		return "Australia/Sydney"
	}
	return ""
}
