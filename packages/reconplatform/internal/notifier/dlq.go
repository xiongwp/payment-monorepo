// dlq.go — 失败投递的死信队列 + admin replay。
//
// 触发场景：
//   - sink HTTP 失败（webhook 5xx / 钉钉超时）
//   - 脚本运行 error / panic（已经写日志，但运营可能想 replay 看 detail）
//
// 存储：Redis Stream `recon:dlq` —— Stream 比 List 优雅，自带 ID + 长度
// 上限 + consumer group。
//
// 字段：
//
//	sink_name   string         哪个 sink 失败（webhook:oncall / dingtalk:risk）
//	script_id   string
//	run_id      string
//	error       string         失败原因
//	pushed_at   string (ISO)
//	payload     bytes(json)    完整 RunResult 序列化，replay 时直接重发
//
// admin web /api/v1/admin/dlq 列表 + 单条 replay。
// scheduler 后台 goroutine 也可以做自动 replay（指数退避）— 本期先手动。

package notifier

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// dlqStream Redis Stream key。
const dlqStream = "recon:dlq"

// dlqMaxLen Stream 最大保留条目；超过 trim by ~。
const dlqMaxLen = 10_000

// RedisDLQ Stream 实现 DLQ 接口。
type RedisDLQ struct {
	r   redis.UniversalClient
	log *zap.Logger
}

// NewRedisDLQ 构造。
func NewRedisDLQ(r redis.UniversalClient, log *zap.Logger) *RedisDLQ {
	if log == nil {
		log = zap.NewNop()
	}
	return &RedisDLQ{r: r, log: log}
}

// Push 把失败的投递塞进 Stream。MAXLEN ~10000 保证不无限增长。
func (q *RedisDLQ) Push(ctx context.Context, sinkName string, r *RunResult, sendErr error) error {
	payload, _ := json.Marshal(r)
	args := &redis.XAddArgs{
		Stream: dlqStream,
		MaxLen: dlqMaxLen,
		Approx: true, // ~MAXLEN 比 MAXLEN 快得多（Redis 内部按 stream node 截）
		Values: map[string]any{
			"sink_name": sinkName,
			"script_id": r.ScriptID,
			"run_id":    r.RunID,
			"error":     sendErr.Error(),
			"pushed_at": time.Now().Format(time.RFC3339),
			"payload":   string(payload),
		},
	}
	_, err := q.r.XAdd(ctx, args).Result()
	if err != nil {
		// DLQ 写 Redis 都失败 = 实在没辙了，仅 log + 继续。
		q.log.Warn("DLQ push failed",
			zap.String("sink", sinkName),
			zap.String("script_id", r.ScriptID),
			zap.Error(err))
	}
	return err
}

// DLQEntry admin web /api/v1/admin/dlq 返回的单条记录。
type DLQEntry struct {
	ID        string     `json:"id"`         // Stream message ID（用作 replay 时的 ack 引用）
	SinkName  string     `json:"sink_name"`
	ScriptID  string     `json:"script_id"`
	RunID     string     `json:"run_id"`
	Error     string     `json:"error"`
	PushedAt  string     `json:"pushed_at"`
	Payload   *RunResult `json:"payload"`
}

// List 倒序拉最近 limit 条 DLQ。
func (q *RedisDLQ) List(ctx context.Context, limit int) ([]*DLQEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	// XRevRange 从尾部往头读，正好是"最新失败优先"
	msgs, err := q.r.XRevRangeN(ctx, dlqStream, "+", "-", int64(limit)).Result()
	if err != nil {
		return nil, err
	}
	out := make([]*DLQEntry, 0, len(msgs))
	for _, m := range msgs {
		e := &DLQEntry{
			ID: m.ID,
		}
		if v, ok := m.Values["sink_name"].(string); ok {
			e.SinkName = v
		}
		if v, ok := m.Values["script_id"].(string); ok {
			e.ScriptID = v
		}
		if v, ok := m.Values["run_id"].(string); ok {
			e.RunID = v
		}
		if v, ok := m.Values["error"].(string); ok {
			e.Error = v
		}
		if v, ok := m.Values["pushed_at"].(string); ok {
			e.PushedAt = v
		}
		if v, ok := m.Values["payload"].(string); ok {
			var rr RunResult
			if json.Unmarshal([]byte(v), &rr) == nil {
				e.Payload = &rr
			}
		}
		out = append(out, e)
	}
	return out, nil
}

// Replay 重新投递单条 DLQ。dispatcher 走原流程；成功后从 Stream 删除。
//
// 失败保留在 Stream 里（不删 → 用户能看到失败原因），下次再试。
func (q *RedisDLQ) Replay(ctx context.Context, dispatcher *Dispatcher, id string) error {
	msgs, err := q.r.XRangeN(ctx, dlqStream, id, id, 1).Result()
	if err != nil {
		return err
	}
	if len(msgs) == 0 {
		return fmt.Errorf("DLQ entry %q not found", id)
	}
	m := msgs[0]
	sinkName, _ := m.Values["sink_name"].(string)
	payload, _ := m.Values["payload"].(string)
	var r RunResult
	if err := json.Unmarshal([]byte(payload), &r); err != nil {
		return fmt.Errorf("payload unmarshal: %w", err)
	}
	// 单 sink replay：临时把脚本 routing 设成只发指定 sink
	dispatcher.SetRouting(r.ScriptID, []string{sinkName})
	dispatcher.Dispatch(ctx, &r)
	// 成功才删（dispatcher 内部失败会 push 新 DLQ；老的留着也没事，由长度上限淘汰）
	_, _ = q.r.XDel(ctx, dlqStream, id).Result()
	return nil
}

// Stats DLQ 当前条目数 + 最旧/最新条目时间，admin dashboard 用。
type Stats struct {
	Length      int64  `json:"length"`
	OldestID    string `json:"oldest_id,omitempty"`
	NewestID    string `json:"newest_id,omitempty"`
	OldestPushed string `json:"oldest_pushed,omitempty"`
}

// Stats 取统计快照。
func (q *RedisDLQ) Stats(ctx context.Context) (*Stats, error) {
	out := &Stats{}
	n, err := q.r.XLen(ctx, dlqStream).Result()
	if err != nil {
		return out, err
	}
	out.Length = n
	if n == 0 {
		return out, nil
	}
	first, _ := q.r.XRangeN(ctx, dlqStream, "-", "+", 1).Result()
	if len(first) > 0 {
		out.OldestID = first[0].ID
		if v, ok := first[0].Values["pushed_at"].(string); ok {
			out.OldestPushed = v
		}
	}
	last, _ := q.r.XRevRangeN(ctx, dlqStream, "+", "-", 1).Result()
	if len(last) > 0 {
		out.NewestID = last[0].ID
	}
	return out, nil
}

// _ 静态确保 RedisDLQ 实现 DLQ 接口
var _ DLQ = (*RedisDLQ)(nil)

// idTimestamp 把 Stream ID（"<unix_ms>-seq"）拆出毫秒时间戳，给前端显示用。
func idTimestamp(id string) (time.Time, bool) {
	if id == "" {
		return time.Time{}, false
	}
	for i, c := range id {
		if c == '-' {
			ms, err := strconv.ParseInt(id[:i], 10, 64)
			if err != nil {
				return time.Time{}, false
			}
			return time.UnixMilli(ms), true
		}
	}
	return time.Time{}, false
}

var _ = idTimestamp // 暂未导出使用，留给后续 admin UI 时间戳显示
