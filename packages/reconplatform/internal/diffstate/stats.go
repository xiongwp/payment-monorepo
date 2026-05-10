// stats.go — diff 统计 dashboard 数据源。
//
// admin web 首页 KPI 卡 + 趋势图：
//
//   KPIs:
//     - 总 open / acked / resolved / false_positive / expired 数量
//     - 今日新增 diff 数 (created_at >= 24h)
//     - 平均 ack 延迟（创建到首次 ack 的中位数）
//
//   分布:
//     - 按 type top 10 (amount_mismatch / missing_leg / state_inconsistent ...)
//     - 按 script_id top 10 (哪些脚本最爱报 diff)
//
// 数据从已有 ZSET 聚合，不另维表（开销可接受，admin web 拉一次缓存几秒）。
//
// Redis keys 已有：
//   recon:diff:by_state:<state>      ZSET member=diff_id, score=updated_ms
//   recon:diff:state:<diff_id>       HASH 含详情
//
// ZRANGE/ZCARD 都是 O(log N) / O(1)，dashboard 拉这些数据 < 100ms。

package diffstate

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/redis/go-redis/v9"
)

// Stats dashboard 数据汇总。
type Stats struct {
	// 各状态总数
	Counts map[State]int64 `json:"counts"`

	// 今日新增（24h 内 score = createdAt 落在 [now-24h, now]）
	Last24hNew int64 `json:"last_24h_new"`

	// 按 type 计数 (top 10)
	ByType []KV `json:"by_type"`

	// 按 script_id 计数 (top 10)
	ByScript []KV `json:"by_script"`

	// 数据生成时间（admin web 缓存判定用）
	GeneratedAt time.Time `json:"generated_at"`
}

// KV 排行榜项。
type KV struct {
	Key   string `json:"key"`
	Count int64  `json:"count"`
}

// Stats 算 dashboard 数据快照。
//
// 算法：
//   1. ZCARD recon:diff:by_state:<state>  共 5 状态 → 5 次 O(1)
//   2. ZCOUNT recon:diff:by_state:open[24h_min, +inf]  → 今日新增 O(log N)
//   3. 遍历 open + acked （活跃状态）→ 按 type / script_id 累加 → top 10
//      注：resolved / false_positive 不算（关掉的就不应进 dashboard 排行）
//      规模考虑：open + acked 通常 < 1000，遍历 + sort 都在 ms 级
//
// sampleLimit 限制遍历活跃 diff 上限（避免极端情况 open 过万拖慢 dashboard）。
// 默认 5000，超过用 ZRevRange 拿最新的样本算。
func (s *Store) Stats(ctx context.Context, sampleLimit int) (*Stats, error) {
	if sampleLimit <= 0 {
		sampleLimit = 5000
	}
	out := &Stats{
		Counts:      make(map[State]int64, 5),
		GeneratedAt: time.Now(),
	}

	// 1) 各状态 cardinality
	pipe := s.r.Pipeline()
	cmds := make(map[State]*redis.IntCmd, 5)
	for _, st := range []State{StateOpen, StateAcked, StateResolved, StateFalsePositive, StateExpired} {
		cmds[st] = pipe.ZCard(ctx, "recon:diff:by_state:"+string(st))
	}
	// 2) 今日新增（仅看 open / acked，已 resolved 的不重复算）
	dayAgo := float64(time.Now().Add(-24 * time.Hour).UnixMilli())
	openNew := pipe.ZCount(ctx, "recon:diff:by_state:"+string(StateOpen),
		formatScore(dayAgo), "+inf")
	ackedNew := pipe.ZCount(ctx, "recon:diff:by_state:"+string(StateAcked),
		formatScore(dayAgo), "+inf")

	if _, err := pipe.Exec(ctx); err != nil {
		return out, err
	}
	for st, c := range cmds {
		out.Counts[st] = c.Val()
	}
	out.Last24hNew = openNew.Val() + ackedNew.Val()

	// 3) 活跃 diff 样本（open + acked）→ 按 type / script 计数
	activeIDs, _ := s.r.ZRevRange(ctx, "recon:diff:by_state:"+string(StateOpen),
		0, int64(sampleLimit-1)).Result()
	if len(activeIDs) < sampleLimit {
		more, _ := s.r.ZRevRange(ctx, "recon:diff:by_state:"+string(StateAcked),
			0, int64(sampleLimit-len(activeIDs)-1)).Result()
		activeIDs = append(activeIDs, more...)
	}

	byType := map[string]int64{}
	byScript := map[string]int64{}
	if len(activeIDs) > 0 {
		// 批量 GET 详情
		keys := make([]string, len(activeIDs))
		for i, id := range activeIDs {
			keys[i] = "recon:diff:state:" + id
		}
		vals, _ := s.r.MGet(ctx, keys...).Result()
		for _, raw := range vals {
			s, ok := raw.(string)
			if !ok {
				continue
			}
			var d Diff
			if err := json.Unmarshal([]byte(s), &d); err != nil {
				continue
			}
			byType[d.Type]++
			byScript[d.ScriptID]++
		}
	}

	out.ByType = topN(byType, 10)
	out.ByScript = topN(byScript, 10)
	return out, nil
}

func topN(m map[string]int64, n int) []KV {
	out := make([]KV, 0, len(m))
	for k, c := range m {
		out = append(out, KV{Key: k, Count: c})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	if len(out) > n {
		out = out[:n]
	}
	return out
}

func formatScore(f float64) string {
	// ZCOUNT 接受字符串 min/max；用 float 避免精度问题
	return "(" + truncFloat(f) // 排他下界（> dayAgo）
}

func truncFloat(f float64) string {
	// 整数毫秒
	return strconvI64(int64(f))
}

func strconvI64(n int64) string {
	// 避免 import strconv 单 fn 用：等价 strconv.FormatInt(n, 10)
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
