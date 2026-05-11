// velocity_geo_device.go — 实时风控扩展规则: velocity / geo / device.
//
// 跟现有 rules/ 目录其他文件并存, 提供 3 个 checker + 综合 Engine:
//   - VelocityChecker:  滑动窗口频次/金额阈值 (按 user / card / IP / device)
//   - GeoChecker:        IP 地理突变 (impossible travel)
//   - DeviceChecker:     设备指纹黑名单 / 新设备 step-up / 共享设备
//
// 单实例内存版; 生产换 Redis (SortedSet) 跨实例.

package rules

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"
)

// VGDDecision 综合决策结果.
type VGDDecision struct {
	Outcome  string   `json:"outcome"`   // allow / review / decline
	Score    int      `json:"score"`     // 0-100, 越高越危险
	HitRules []string `json:"hit_rules"`
	Reason   string   `json:"reason,omitempty"`
}

// VGDTxnContext 一笔交易的上下文.
type VGDTxnContext struct {
	TxnID       string
	UserID      string
	MerchantID  string
	CardToken   string
	AmountMinor int64
	Currency    string
	IP          string
	Country     string
	DeviceID    string
	UserAgent   string
	At          time.Time
}

func (tc VGDTxnContext) field(k string) string {
	switch k {
	case "user_id":
		return tc.UserID
	case "merchant_id":
		return tc.MerchantID
	case "card_token":
		return tc.CardToken
	case "ip":
		return tc.IP
	case "device_id":
		return tc.DeviceID
	}
	return ""
}

// ─── Velocity ─────────────────────────────────────────────────────────

// VGDVelocityRule ...
type VGDVelocityRule struct {
	Key        string        // user_id / card_token / ip / device_id / merchant_id
	Window     time.Duration
	MaxCount   int           // 0=不查频次
	MaxAmount  int64         // 0=不查金额
	ScoreOnHit int
}

// VGDVelocityChecker 内存版滑动窗口.
type VGDVelocityChecker struct {
	mu      sync.Mutex
	buckets map[string][]vgdEntry
	rules   []VGDVelocityRule
}

type vgdEntry struct {
	at     time.Time
	amount int64
}

// NewVGDVelocityChecker ...
func NewVGDVelocityChecker(rules []VGDVelocityRule) *VGDVelocityChecker {
	return &VGDVelocityChecker{buckets: map[string][]vgdEntry{}, rules: rules}
}

// Evaluate 看每条 rule, 算落桶后是否超阈.
func (v *VGDVelocityChecker) Evaluate(_ context.Context, tc VGDTxnContext) []vgdHit {
	v.mu.Lock()
	defer v.mu.Unlock()
	hits := []vgdHit{}
	for _, r := range v.rules {
		val := tc.field(r.Key)
		if val == "" {
			continue
		}
		key := r.Key + "#" + val
		entries := v.buckets[key]
		cutoff := tc.At.Add(-r.Window)
		fresh := entries[:0]
		count := 0
		var amount int64
		for _, e := range entries {
			if e.at.Before(cutoff) {
				continue
			}
			fresh = append(fresh, e)
			count++
			amount += e.amount
		}
		count++ // 算上本笔
		amount += tc.AmountMinor

		hit := false
		reason := ""
		if r.MaxCount > 0 && count > r.MaxCount {
			hit = true
			reason = fmt.Sprintf("velocity:%s count=%d > %d in %s",
				r.Key, count, r.MaxCount, r.Window)
		}
		if r.MaxAmount > 0 && amount > r.MaxAmount {
			hit = true
			reason = fmt.Sprintf("velocity:%s amount=%d > %d in %s",
				r.Key, amount, r.MaxAmount, r.Window)
		}
		if hit {
			hits = append(hits, vgdHit{Name: "velocity:" + r.Key, Score: r.ScoreOnHit, Reason: reason})
		}
		// commit
		fresh = append(fresh, vgdEntry{at: tc.At, amount: tc.AmountMinor})
		v.buckets[key] = fresh
	}
	return hits
}

// ─── Geo (impossible travel) ─────────────────────────────────────────

// VGDGeoChecker IP 突变检测.
type VGDGeoChecker struct {
	mu       sync.Mutex
	lastSeen map[string]vgdGeoPoint
	maxKmH   float64
}

type vgdGeoPoint struct {
	Lat, Lng float64
	At       time.Time
	Country  string
}

// NewVGDGeoChecker default 800 km/h (民航极限).
func NewVGDGeoChecker(maxKmH float64) *VGDGeoChecker {
	if maxKmH <= 0 {
		maxKmH = 800
	}
	return &VGDGeoChecker{lastSeen: map[string]vgdGeoPoint{}, maxKmH: maxKmH}
}

// Evaluate ...
func (g *VGDGeoChecker) Evaluate(_ context.Context, tc VGDTxnContext, lat, lng float64) []vgdHit {
	if tc.UserID == "" {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	prev, ok := g.lastSeen[tc.UserID]
	g.lastSeen[tc.UserID] = vgdGeoPoint{Lat: lat, Lng: lng, At: tc.At, Country: tc.Country}
	if !ok {
		return nil
	}
	dist := vgdHaversine(prev.Lat, prev.Lng, lat, lng)
	dt := tc.At.Sub(prev.At).Hours()
	if dt <= 0 {
		return nil
	}
	speed := dist / dt
	if speed > g.maxKmH {
		return []vgdHit{{
			Name: "geo:impossible_travel", Score: 60,
			Reason: fmt.Sprintf("%.0fkm in %.1fh = %.0fkm/h (%s→%s)",
				dist, dt, speed, prev.Country, tc.Country),
		}}
	}
	return nil
}

func vgdHaversine(lat1, lng1, lat2, lng2 float64) float64 {
	const R = 6371.0
	dLat := (lat2 - lat1) * math.Pi / 180
	dLng := (lng2 - lng1) * math.Pi / 180
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*math.Pi/180)*math.Cos(lat2*math.Pi/180)*
			math.Sin(dLng/2)*math.Sin(dLng/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return R * c
}

// ─── Device ───────────────────────────────────────────────────────────

// VGDDeviceChecker 设备指纹检查.
type VGDDeviceChecker struct {
	mu                 sync.Mutex
	blacklist          map[string]struct{}
	seenByUser         map[string]map[string]bool
	usersByDev         map[string]map[string]bool
	multiUserThreshold int
}

// NewVGDDeviceChecker ...
func NewVGDDeviceChecker(blacklist []string, multiUserThreshold int) *VGDDeviceChecker {
	bl := map[string]struct{}{}
	for _, b := range blacklist {
		bl[b] = struct{}{}
	}
	if multiUserThreshold <= 0 {
		multiUserThreshold = 5
	}
	return &VGDDeviceChecker{
		blacklist: bl, multiUserThreshold: multiUserThreshold,
		seenByUser: map[string]map[string]bool{},
		usersByDev: map[string]map[string]bool{},
	}
}

// Evaluate ...
func (d *VGDDeviceChecker) Evaluate(_ context.Context, tc VGDTxnContext) []vgdHit {
	if tc.DeviceID == "" {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	hits := []vgdHit{}

	if _, ok := d.blacklist[tc.DeviceID]; ok {
		return []vgdHit{{Name: "device:blacklist", Score: 100, Reason: "device on blacklist"}}
	}

	if tc.UserID != "" {
		if d.seenByUser[tc.UserID] == nil {
			d.seenByUser[tc.UserID] = map[string]bool{}
		}
		isNew := !d.seenByUser[tc.UserID][tc.DeviceID]
		d.seenByUser[tc.UserID][tc.DeviceID] = true
		if isNew {
			hits = append(hits, vgdHit{
				Name: "device:new_for_user", Score: 25,
				Reason: "first time this user uses this device",
			})
		}

		if d.usersByDev[tc.DeviceID] == nil {
			d.usersByDev[tc.DeviceID] = map[string]bool{}
		}
		d.usersByDev[tc.DeviceID][tc.UserID] = true
		n := len(d.usersByDev[tc.DeviceID])
		if n >= d.multiUserThreshold {
			hits = append(hits, vgdHit{
				Name: "device:shared", Score: 70,
				Reason: fmt.Sprintf("device shared by %d users (>=%d)", n, d.multiUserThreshold),
			})
		}
	}
	return hits
}

// ─── 综合 ─────────────────────────────────────────────────────────────

type vgdHit struct {
	Name   string
	Score  int
	Reason string
}

// VGDEngine 把 3 个 checker 串起来.
type VGDEngine struct {
	Velocity   *VGDVelocityChecker
	Geo        *VGDGeoChecker
	Device     *VGDDeviceChecker
	ResolveGeo func(ip string) (lat, lng float64, country string, err error)
}

// Score 评估一笔交易.
//   score >= 80 → decline
//   50-79     → review (manual approve / step-up)
//   < 50      → allow
func (e *VGDEngine) Score(ctx context.Context, tc VGDTxnContext) VGDDecision {
	hits := []vgdHit{}
	if e.Velocity != nil {
		hits = append(hits, e.Velocity.Evaluate(ctx, tc)...)
	}
	if e.Geo != nil && e.ResolveGeo != nil {
		if lat, lng, country, err := e.ResolveGeo(tc.IP); err == nil {
			tc.Country = country
			hits = append(hits, e.Geo.Evaluate(ctx, tc, lat, lng)...)
		}
	}
	if e.Device != nil {
		hits = append(hits, e.Device.Evaluate(ctx, tc)...)
	}
	dec := VGDDecision{Outcome: "allow"}
	for _, h := range hits {
		if h.Score > dec.Score {
			dec.Score = h.Score
		}
		dec.HitRules = append(dec.HitRules, h.Name)
		if dec.Reason == "" {
			dec.Reason = h.Reason
		}
	}
	switch {
	case dec.Score >= 80:
		dec.Outcome = "decline"
	case dec.Score >= 50:
		dec.Outcome = "review"
	}
	return dec
}
