// Package alerter — FEAT-2: 给规则配 diff 阈值,超阈值发 webhook (Slack-style).
//
// 数据流:
//
//	matcher.Worker.processOne 发 publisher → publisher.Publish
//	    ↓ (FEAT-2 钩子)
//	alerter.RecordDiff(rule, verdict)   // 写 Redis ZSET 滑动窗口
//	...
//	(后台 goroutine 每分钟)
//	for rule in alertRules:
//	    count := ZCOUNT recon:alert:diffs:<rule> (now - windowMin .. now)
//	    if count >= threshold && time.Since(lastFiredAt) >= cooldown:
//	        sendWebhook(url, payload)
//	        lastFiredAt = now
//
// 设计取舍:
//   - 滑动窗口落 Redis (跨 pod 共享, ZSET score=unix_seconds), 简单但每条 diff 多 1 RTT.
//     需要量大时考虑改本地计数 + 1s 聚合上报.
//   - cooldown 防同一 rule 1 分钟内告警风暴.
//   - 不引入外部 alertmanager — admin web 直接发 Slack incoming webhook 即可 (一个 HTTP POST).
package alerter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// AlertConfig 单条告警规则.
type AlertConfig struct {
	RuleID      string `json:"rule_id"`     // 关联的 script.id (告警绑定在某条对账规则上)
	Threshold   int64  `json:"threshold"`   // 窗口内 diff 数 >= 此值 → 触发
	WindowMin   int    `json:"window_min"`  // 窗口 (分钟), 默认 5
	CooldownMin int    `json:"cooldown_min"` // 触发后冷却 (分钟), 默认 30
	WebhookURL  string `json:"webhook_url"`  // Slack-compatible incoming webhook
	Enabled     bool   `json:"enabled"`
}

// RecordDiff 给 publisher (或任意上游) 调一行: 落一笔 diff 时间戳到 ZSET.
//
// 仅 verdict 非 matched / pending 时计 (即实际差异: mismatched / orphan / error).
// 写失败仅 warn, 不阻塞主路径.
func RecordDiff(ctx context.Context, rdb redis.UniversalClient, ruleID, verdict string) {
	if rdb == nil || ruleID == "" {
		return
	}
	if verdict == "matched" || verdict == "pending" || verdict == "" {
		return
	}
	now := time.Now().Unix()
	key := "recon:alert:diffs:" + ruleID
	pipe := rdb.Pipeline()
	pipe.ZAdd(ctx, key, redis.Z{Score: float64(now), Member: strconv.FormatInt(now, 10) + ":" + verdict})
	// 修剪 1h 前 (防 ZSET 无限增长; 告警窗口最大也就 1h)
	pipe.ZRemRangeByScore(ctx, key, "0", strconv.FormatInt(now-3600, 10))
	pipe.Expire(ctx, key, 2*time.Hour)
	_, _ = pipe.Exec(ctx)
}

// Store 持久化 alert config 集.
type Store struct {
	rdb redis.UniversalClient
}

// NewStore 构造.
func NewStore(rdb redis.UniversalClient) *Store { return &Store{rdb: rdb} }

const alertConfigKey = "recon:alert:config" // HASH ruleID → JSON AlertConfig

// Set 保存或更新一条告警.
func (s *Store) Set(ctx context.Context, c AlertConfig) error {
	if c.RuleID == "" {
		return fmt.Errorf("rule_id required")
	}
	if c.WindowMin <= 0 {
		c.WindowMin = 5
	}
	if c.CooldownMin <= 0 {
		c.CooldownMin = 30
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return s.rdb.HSet(ctx, alertConfigKey, c.RuleID, b).Err()
}

// Get 取一条.
func (s *Store) Get(ctx context.Context, ruleID string) (*AlertConfig, error) {
	raw, err := s.rdb.HGet(ctx, alertConfigKey, ruleID).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c AlertConfig
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// List 全部告警.
func (s *Store) List(ctx context.Context) ([]AlertConfig, error) {
	m, err := s.rdb.HGetAll(ctx, alertConfigKey).Result()
	if err != nil {
		return nil, err
	}
	out := make([]AlertConfig, 0, len(m))
	for _, raw := range m {
		var c AlertConfig
		if err := json.Unmarshal([]byte(raw), &c); err == nil {
			out = append(out, c)
		}
	}
	return out, nil
}

// Delete 删一条.
func (s *Store) Delete(ctx context.Context, ruleID string) error {
	return s.rdb.HDel(ctx, alertConfigKey, ruleID).Err()
}

// Runner 后台 ticker 跑评估 + 触发.
type Runner struct {
	store  *Store
	rdb    redis.UniversalClient
	logger *zap.Logger
	hc     *http.Client

	// 内存维护 lastFiredAt, 进程重启重置. 跨 pod 用 Redis SET NX 加锁防风暴可后续加.
	mu          sync.Mutex
	lastFiredAt map[string]time.Time

	firedTotal  int64
	checkErrors int64
}

// NewRunner 构造.
func NewRunner(store *Store, rdb redis.UniversalClient, logger *zap.Logger) *Runner {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Runner{
		store:       store,
		rdb:         rdb,
		logger:      logger,
		hc:          &http.Client{Timeout: 5 * time.Second},
		lastFiredAt: map[string]time.Time{},
	}
}

// Run 阻塞跑 ticker 直到 ctx 取消.
func (r *Runner) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 1 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	r.logger.Info("alerter started", zap.Duration("interval", interval))
	for {
		select {
		case <-ctx.Done():
			r.logger.Info("alerter stopped")
			return
		case <-t.C:
			r.tick(ctx)
		}
	}
}

// tick 单次评估.
func (r *Runner) tick(ctx context.Context) {
	configs, err := r.store.List(ctx)
	if err != nil {
		r.logger.Warn("alerter list configs failed", zap.Error(err))
		return
	}
	now := time.Now()
	for _, c := range configs {
		if !c.Enabled || c.WebhookURL == "" || c.Threshold <= 0 {
			continue
		}
		windowStart := now.Add(-time.Duration(c.WindowMin) * time.Minute).Unix()
		key := "recon:alert:diffs:" + c.RuleID
		count, err := r.rdb.ZCount(ctx, key,
			strconv.FormatInt(windowStart, 10),
			strconv.FormatInt(now.Unix(), 10)).Result()
		if err != nil {
			r.checkErrors++
			r.logger.Warn("alerter ZCount failed", zap.String("rule", c.RuleID), zap.Error(err))
			continue
		}
		if count < c.Threshold {
			continue
		}
		// 冷却检查
		r.mu.Lock()
		last := r.lastFiredAt[c.RuleID]
		coolElapsed := now.Sub(last) >= time.Duration(c.CooldownMin)*time.Minute
		if coolElapsed {
			r.lastFiredAt[c.RuleID] = now
		}
		r.mu.Unlock()
		if !coolElapsed {
			continue
		}
		r.fire(ctx, c, count)
	}
}

// fire 发 webhook.
//
// payload 格式 Slack-compatible (text + attachment).
// 失败仅 log warn, 下次仍会重试 (lastFiredAt 已设, 下次 cooldown 后再试).
func (r *Runner) fire(ctx context.Context, c AlertConfig, count int64) {
	payload := map[string]any{
		"text": fmt.Sprintf(":rotating_light: Recon alert: rule %s breached %d/%dmin (threshold %d)",
			c.RuleID, count, c.WindowMin, c.Threshold),
		"attachments": []map[string]any{
			{
				"color": "danger",
				"fields": []map[string]any{
					{"title": "Rule", "value": c.RuleID, "short": true},
					{"title": "Window", "value": fmt.Sprintf("%dmin", c.WindowMin), "short": true},
					{"title": "Threshold", "value": fmt.Sprintf("%d", c.Threshold), "short": true},
					{"title": "Actual", "value": fmt.Sprintf("%d", count), "short": true},
				},
			},
		},
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.WebhookURL, bytes.NewReader(body))
	if err != nil {
		r.logger.Warn("alerter webhook req failed", zap.Error(err))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.hc.Do(req)
	if err != nil {
		r.logger.Warn("alerter webhook send failed",
			zap.String("rule", c.RuleID), zap.Error(err))
		return
	}
	defer resp.Body.Close()
	r.firedTotal++
	r.logger.Info("alerter fired",
		zap.String("rule", c.RuleID),
		zap.Int64("count", count),
		zap.Int("status", resp.StatusCode))
}

// Stats 监控 / debug.
type Stats struct {
	FiredTotal  int64 `json:"fired_total"`
	CheckErrors int64 `json:"check_errors"`
}

// Stats 拿计数.
func (r *Runner) Stats() Stats {
	return Stats{FiredTotal: r.firedTotal, CheckErrors: r.checkErrors}
}
