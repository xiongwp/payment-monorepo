// slowlog.go — REL-3: per-rule 慢日志 + 统计.
//
// 用途:
//   规则跑得慢 (尤其 Starlark 写得糟,e.g. for x in ctx.scan(...) 嵌套两层),
//   线上发现 P99 latency 飙到 1s+ 但不知道哪条规则在拖累. 慢日志给 admin 一眼看
//   top-N 慢规则 + 最近 10 条慢样本 (含 trigger / events 数 / duration).
//
// 数据结构:
//   - perRule map[string]*ruleStat (规则名 → 统计):
//     - count, totalDuration, p50/p99 用 HDR-like 直方图 (但简化为按 ms 分桶 0/1/2/5/10/20/.../1000)
//   - ringBuf 最近 N 条 (默认 100) duration > threshold (默认 100ms) 的慢样本
//
// 性能:
//   - Record 仅原子加 + 桶累加, 单条 < 100ns, 不阻塞 matcher hot path.
//   - Snapshot 拍照: 加 RLock 拷出.
//   - 内存: 1000 个规则 × ~ 200 bytes = 200KB, ringBuf 100 × 1KB = 100KB.
//
// 不上 Prometheus histogram 原因: 那个走 /metrics 由 Grafana 看;
// 慢日志要 admin web 实时刷新, 简易快照接口更直接.

package matcher

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// SlowEntry 单条慢样本.
type SlowEntry struct {
	RuleName    string    `json:"rule"`
	TriggerKey  string    `json:"trigger"`
	DurationMS  int64     `json:"duration_ms"`
	EventCount  int       `json:"event_count"`
	Verdict     string    `json:"verdict"`
	ObservedAt  time.Time `json:"observed_at"`
}

// ruleStat 单规则统计.
type ruleStat struct {
	count       int64
	totalMillis int64
	slowest     int64
	// 直方桶 (ms): <1, <2, <5, <10, <20, <50, <100, <200, <500, <1000, >=1000
	buckets [11]int64
}

func bucketIdx(ms int64) int {
	switch {
	case ms < 1:
		return 0
	case ms < 2:
		return 1
	case ms < 5:
		return 2
	case ms < 10:
		return 3
	case ms < 20:
		return 4
	case ms < 50:
		return 5
	case ms < 100:
		return 6
	case ms < 200:
		return 7
	case ms < 500:
		return 8
	case ms < 1000:
		return 9
	default:
		return 10
	}
}

// bucketCeil 桶的上界 (ms) — Snapshot 估算 p99 用.
var bucketCeil = [11]int64{1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 5000}

// SlowLog 全局慢日志记录器, matcher worker 持引用.
type SlowLog struct {
	mu         sync.RWMutex
	perRule    map[string]*ruleStat
	ring       []SlowEntry
	ringHead   int // 下一个写入位置 (环形)
	thresholdMS int64
	ringCap    int
}

// NewSlowLog 慢日志, 默认 threshold=100ms, ring=100.
func NewSlowLog(thresholdMS int64, ringCap int) *SlowLog {
	if thresholdMS <= 0 {
		thresholdMS = 100
	}
	if ringCap <= 0 {
		ringCap = 100
	}
	return &SlowLog{
		perRule:     map[string]*ruleStat{},
		ring:        make([]SlowEntry, ringCap),
		thresholdMS: thresholdMS,
		ringCap:     ringCap,
	}
}

// Record 一次规则执行后调.
//
// 累加: 桶 / count / total / max. > threshold 才写 ring (减内存压力).
func (s *SlowLog) Record(ruleName, triggerKey, verdict string, durationMS int64, eventCount int) {
	if s == nil || ruleName == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs, ok := s.perRule[ruleName]
	if !ok {
		rs = &ruleStat{}
		s.perRule[ruleName] = rs
	}
	rs.count++
	rs.totalMillis += durationMS
	if durationMS > rs.slowest {
		rs.slowest = durationMS
	}
	rs.buckets[bucketIdx(durationMS)]++

	if durationMS >= s.thresholdMS {
		s.ring[s.ringHead] = SlowEntry{
			RuleName:   ruleName,
			TriggerKey: triggerKey,
			DurationMS: durationMS,
			EventCount: eventCount,
			Verdict:    verdict,
			ObservedAt: time.Now().UTC(),
		}
		s.ringHead = (s.ringHead + 1) % s.ringCap
	}
}

// RuleSummary 单规则汇总 (Snapshot 内嵌).
type RuleSummary struct {
	RuleName  string `json:"rule"`
	Count     int64  `json:"count"`
	AvgMS     int64  `json:"avg_ms"`
	P50MS     int64  `json:"p50_ms"`
	P99MS     int64  `json:"p99_ms"`
	SlowestMS int64  `json:"slowest_ms"`
}

// SlowLogSnapshot Snapshot() 返回的快照.
type SlowLogSnapshot struct {
	ThresholdMS int64         `json:"threshold_ms"`
	Top         []RuleSummary `json:"top"`        // 按 P99 降排
	Recent      []SlowEntry   `json:"recent"`     // 最近 N 条慢样本 (新 → 旧)
}

// Snapshot 拍照. 不阻塞写入路径 (RLock).
func (s *SlowLog) Snapshot() SlowLogSnapshot {
	if s == nil {
		return SlowLogSnapshot{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	summaries := make([]RuleSummary, 0, len(s.perRule))
	for name, rs := range s.perRule {
		avg := int64(0)
		if rs.count > 0 {
			avg = rs.totalMillis / rs.count
		}
		summaries = append(summaries, RuleSummary{
			RuleName:  name,
			Count:     rs.count,
			AvgMS:     avg,
			P50MS:     estimatePercentile(rs.buckets, rs.count, 0.50),
			P99MS:     estimatePercentile(rs.buckets, rs.count, 0.99),
			SlowestMS: rs.slowest,
		})
	}
	// 按 P99 降排
	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].P99MS > summaries[j].P99MS
	})
	if len(summaries) > 20 {
		summaries = summaries[:20]
	}

	// Recent: 反向遍历环 (从 ringHead-1 开始,新 → 旧).
	recent := make([]SlowEntry, 0, s.ringCap)
	for i := 0; i < s.ringCap; i++ {
		idx := (s.ringHead - 1 - i + s.ringCap) % s.ringCap
		e := s.ring[idx]
		if e.RuleName == "" {
			continue // 未写过
		}
		recent = append(recent, e)
	}
	return SlowLogSnapshot{
		ThresholdMS: s.thresholdMS,
		Top:         summaries,
		Recent:      recent,
	}
}

// estimatePercentile 用桶累加估 percentile, 取桶上界 (近似).
func estimatePercentile(buckets [11]int64, total int64, p float64) int64 {
	if total == 0 {
		return 0
	}
	target := int64(float64(total) * p)
	var accum int64
	for i, c := range buckets {
		accum += c
		if accum >= target {
			return bucketCeil[i]
		}
	}
	return bucketCeil[len(bucketCeil)-1]
}

// SlowLogRedisKey admin /api/v1/perf/slow 读这个 Redis key 拿快照.
//
// matcher 进程周期 SET 全量 JSON, 多 pod 时只取首个 (snapshot 间隔够短可接受).
const SlowLogRedisKey = "recon:perf:slowlog"

// PublishPeriodic 后台 ticker 周期 Snapshot + SET 到 Redis.
//
// 用法 (matcher 主程):
//   slowLog := matcher.NewSlowLog(100, 100)
//   reg.WithSlowLog(slowLog)
//   go slowLog.PublishPeriodic(ctx, rdb, 10*time.Second)
//
// interval=0 → 默认 10s. SET TTL 60s (跨 pod 多写覆盖 OK,断流 60s 后 admin 看到 stale).
func (s *SlowLog) PublishPeriodic(ctx context.Context, rdb redis.UniversalClient, interval time.Duration) {
	if s == nil || rdb == nil {
		return
	}
	if interval <= 0 {
		interval = 10 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			snap := s.Snapshot()
			b, err := json.Marshal(snap)
			if err != nil {
				continue
			}
			_ = rdb.Set(ctx, SlowLogRedisKey, b, 60*time.Second).Err()
		}
	}
}
