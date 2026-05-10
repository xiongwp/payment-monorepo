// dedup.go — 告警聚合 + 抑制窗口。
//
// 问题：脚本一跑可能 100 条 amount_mismatch，全发钉钉机器人 = 群里炸号
// + oncall 麻木。
//
// 方案：以 (script_id, diff_type, key) 三元组做去重 key，N 分钟窗口内
// 同 key 只发 1 条 summary，后续相同的累计计数到下一窗口。窗口走
// Redis ZSET（score = expire_ts），过期自动 GC。
//
// 用法：包在 Dispatcher.Send 之前。
//
//	suppressor := notifier.NewSuppressor(rdb, 5*time.Minute)
//	if filtered := suppressor.Filter(ctx, runResult); len(filtered.Diffs) > 0 {
//	    dispatcher.Dispatch(ctx, filtered)
//	}

package notifier

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// Suppressor 基于 Redis 的告警抑制器。
//
// Key 设计：
//
//	recon:notify:dedup:<sha1(script_id|type|key)>  → ZSET，score=expire_ts，
//	                                                  member=run_id（防同 run 多 diff 重复抑制）
//
// Filter 行为：
//   - 同 key 在窗口内首次出现 → 标 ACTIVE 让 Dispatcher 发出去
//   - 后续重复 → counter +1 但不发；返 nil 让 Dispatcher 跳过
//   - 抑制 counter 在下个窗口 reset；如果 counter > 1 那条 summary 会写在
//     下次首发的 detail 里，告诉 oncall "上窗口期共抑制了 N 条"
type Suppressor struct {
	r      redis.UniversalClient
	window time.Duration
}

// NewSuppressor 构造。window 典型 5min；< 1min 没意义（dispatch 频率本身 >1s）。
func NewSuppressor(r redis.UniversalClient, window time.Duration) *Suppressor {
	if window < time.Minute {
		window = time.Minute
	}
	return &Suppressor{r: r, window: window}
}

// Filter 把 Diffs 切成 send / 抑制 两组。返新的 RunResult 仅含 send 部分；
// suppressedCount > 0 时把数量塞 result.Detail.suppressed_total 让 sink 提示。
//
// Redis 不可达时 Fallback：全部直发（fail-open，不为了 dedup 把告警吞掉）。
func (s *Suppressor) Filter(ctx context.Context, r *RunResult) (*RunResult, int) {
	if r == nil || len(r.Diffs) == 0 {
		return r, 0
	}
	now := time.Now()
	expire := now.Add(s.window).Unix()

	send := make([]Diff, 0, len(r.Diffs))
	suppressed := 0
	pipe := s.r.Pipeline()
	defer func() { _, _ = pipe.Exec(ctx) }()

	for _, d := range r.Diffs {
		key := dedupKey(r.ScriptID, d.Type, d.Key)
		// SETNX 风格：只在窗口内不存在时填入新条目（首次发）
		set, err := s.r.SetNX(ctx, key, "1", s.window).Result()
		if err != nil {
			// Redis 挂 → fail-open，原样发
			send = append(send, d)
			continue
		}
		if set {
			send = append(send, d)
			continue
		}
		// 已存在：累加计数
		pipe.Incr(ctx, key+":count")
		pipe.ExpireAt(ctx, key+":count", time.Unix(expire, 0))
		suppressed++
	}

	if suppressed == 0 {
		return r, 0
	}
	// 把抑制计数挂进 result（让 sink 在告警里展示 "本窗口已抑制 N 条同类告警"）
	out := *r
	out.Diffs = send
	if len(out.Diffs) > 0 {
		// 在第一条 diff 的 Detail 里加一行
		first := out.Diffs[0]
		dd, _ := first.Detail.(map[string]any)
		if dd == nil {
			dd = map[string]any{}
		}
		dd["_suppressed_in_window"] = suppressed
		dd["_window"] = s.window.String()
		first.Detail = dd
		out.Diffs[0] = first
	}
	return &out, suppressed
}

// dedupKey 三元组哈希。SHA1 截前 20 hex（10 byte）足够，碰撞概率忽略。
func dedupKey(scriptID, diffType, key string) string {
	h := sha1.New()
	h.Write([]byte(scriptID))
	h.Write([]byte{0})
	h.Write([]byte(diffType))
	h.Write([]byte{0})
	h.Write([]byte(key))
	return "recon:notify:dedup:" + hex.EncodeToString(h.Sum(nil)[:10])
}

// SuppressedCount 给 admin web 显示某 key 当前累计抑制数。
func (s *Suppressor) SuppressedCount(ctx context.Context, scriptID, diffType, key string) int64 {
	v, err := s.r.Get(ctx, dedupKey(scriptID, diffType, key)+":count").Result()
	if err != nil || v == "" {
		return 0
	}
	n, _ := strconv.ParseInt(v, 10, 64)
	return n
}
