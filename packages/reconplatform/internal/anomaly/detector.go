// Package anomaly — 异常检测：每小时 diff 量 EWMA + 3σ 突增告警。
//
// 基线方法：Exponentially Weighted Moving Average + Standard deviation。
// 不用 ML / Prophet，纯统计就能抓 90% 系统级异常（某 channel 故障 / 上游
// peak / 脚本误改 false positive 暴涨）。
//
// 算法:
//   ewma_t  = α × x_t + (1-α) × ewma_{t-1}        # α = 0.2 (smoothing)
//   var_t   = α × (x_t - ewma_t)² + (1-α) × var_{t-1}
//   z_score = (x_t - ewma_t) / √var_t
//   if abs(z_score) > 3 → fire alert
//
// 数据流:
//   每小时整点 cron tick:
//     1. 拉过去 1h 的 diff 量 (ZCOUNT recon:diff:by_state:open by score window)
//     2. 按 diff.type 分组累加
//     3. 加载 baseline (recon:anomaly:baseline:{type})
//     4. 计算 ewma / var / z_score
//     5. z_score abnormal → 写 diff "anomaly_burst" 到 diffstate
//     6. 更新 baseline
//
// 启动期没基线时（cold start）：前 24h 只观察不报警，让 baseline 收敛。

package anomaly

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/robfig/cron/v3"
	"go.uber.org/zap"

	"reconcile-system/internal/diffstate"
)

// Baseline 单个 diff_type 的统计基线（持久化在 Redis）。
type Baseline struct {
	Type      string  `json:"type"`
	EWMA      float64 `json:"ewma"`
	Variance  float64 `json:"variance"`
	Samples   int     `json:"samples"`     // 已观察的小时数（< 24 时不告警）
	UpdatedAt int64   `json:"updated_at"`  // unix ms
}

// Alpha EWMA 平滑系数，越大越敏感（捕到突增更快但更易误报）。
const Alpha = 0.2

// WarmupHours 启动后多少小时才允许告警（让 baseline 收敛）。
const WarmupHours = 24

// SigmaThreshold 触发告警的 z-score 阈值。
const SigmaThreshold = 3.0

// Detector 异常检测器。
type Detector struct {
	rdb       redis.UniversalClient
	diffStore *diffstate.Store
	log       *zap.Logger
	cron      *cron.Cron
	mu        sync.Mutex
}

func New(rdb redis.UniversalClient, diffStore *diffstate.Store, log *zap.Logger) *Detector {
	if log == nil {
		log = zap.NewNop()
	}
	return &Detector{
		rdb:       rdb,
		diffStore: diffStore,
		log:       log,
		cron:      cron.New(),
	}
}

// Start 注册每小时 1 分 cron（避开整点冲突）。
func (d *Detector) Start() error {
	_, err := d.cron.AddFunc("1 * * * *", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := d.runOnce(ctx); err != nil {
			d.log.Warn("anomaly detector run failed", zap.Error(err))
		}
	})
	if err != nil {
		return err
	}
	d.cron.Start()
	d.log.Info("anomaly detector started")
	return nil
}

func (d *Detector) Stop(ctx context.Context) {
	stopped := d.cron.Stop()
	select {
	case <-stopped.Done():
	case <-ctx.Done():
	}
}

// runOnce 单次扫描所有 diff_type，计算 z-score，超 3σ 报。
func (d *Detector) runOnce(ctx context.Context) error {
	// 过去 1h 时间窗
	now := time.Now().UTC()
	from := now.Add(-time.Hour).UnixMilli()
	to := now.UnixMilli()

	// 按 type 累加 — 用 SCAN by_type:* 可能 太慢。这里走 diffstate.Stats 节流。
	// 简化：直接拉 open + acked 状态的 diff 总数（type 字段在 hash 里）
	counts, err := d.countByTypeInWindow(ctx, from, to)
	if err != nil {
		return err
	}

	for diffType, count := range counts {
		baseline, err := d.loadBaseline(ctx, diffType)
		if err != nil {
			continue
		}
		x := float64(count)
		// 第一次见的 type → 用本次值初始化 baseline
		if baseline.Samples == 0 {
			baseline.EWMA = x
			baseline.Variance = 0
		} else {
			delta := x - baseline.EWMA
			baseline.EWMA = Alpha*x + (1-Alpha)*baseline.EWMA
			baseline.Variance = Alpha*delta*delta + (1-Alpha)*baseline.Variance
		}
		baseline.Samples++
		baseline.UpdatedAt = now.UnixMilli()

		// warmup 期不告警
		if baseline.Samples > WarmupHours && baseline.Variance > 0 {
			std := math.Sqrt(baseline.Variance)
			z := (x - baseline.EWMA) / std
			if math.Abs(z) > SigmaThreshold {
				d.fireAlert(ctx, diffType, count, baseline.EWMA, std, z)
			}
		}
		_ = d.saveBaseline(ctx, baseline)
	}
	d.log.Info("anomaly detector cycle",
		zap.Int("types", len(counts)),
		zap.Int64("from_ms", from))
	return nil
}

// countByTypeInWindow 拉过去时间窗内的 diff 数量按 type 分组。
//
// 实现: SCAN recon:diff:state:* → GET → 看 created_at_ms in window? → 累加 type
// O(n_total)，1 万级 diff 没问题，更大量级该走 ClickHouse SQL。
func (d *Detector) countByTypeInWindow(ctx context.Context, fromMs, toMs int64) (map[string]int, error) {
	out := map[string]int{}
	iter := d.rdb.Scan(ctx, 0, "recon:diff:state:*", 1000).Iterator()
	for iter.Next(ctx) {
		key := iter.Val()
		body, err := d.rdb.Get(ctx, key).Bytes()
		if err != nil {
			continue
		}
		var dx struct {
			Type      string `json:"type"`
			CreatedAt string `json:"created_at"`
		}
		if err := json.Unmarshal(body, &dx); err != nil {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, dx.CreatedAt)
		if err != nil {
			t, err = time.Parse(time.RFC3339, dx.CreatedAt)
			if err != nil {
				continue
			}
		}
		ms := t.UnixMilli()
		if ms < fromMs || ms >= toMs {
			continue
		}
		out[dx.Type]++
	}
	return out, iter.Err()
}

func (d *Detector) loadBaseline(ctx context.Context, t string) (*Baseline, error) {
	key := "recon:anomaly:baseline:" + t
	body, err := d.rdb.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return &Baseline{Type: t}, nil
	}
	if err != nil {
		return nil, err
	}
	var b Baseline
	if err := json.Unmarshal(body, &b); err != nil {
		return &Baseline{Type: t}, nil
	}
	return &b, nil
}

func (d *Detector) saveBaseline(ctx context.Context, b *Baseline) error {
	body, _ := json.Marshal(b)
	return d.rdb.Set(ctx, "recon:anomaly:baseline:"+b.Type, body, 90*24*time.Hour).Err()
}

// fireAlert 写一条特殊 diff "anomaly_burst" 到 diffstate，让 notifier 推。
func (d *Detector) fireAlert(ctx context.Context, diffType string, count int, ewma, std, z float64) {
	if d.diffStore == nil {
		return
	}
	dx := diffstate.Diff{
		ID: diffstate.IDFor("anomaly_detector", strconv.FormatInt(time.Now().Unix(), 10),
			"anomaly_burst", diffType, 0),
		ScriptID: "anomaly_detector",
		RunID:    "anomaly_" + time.Now().UTC().Format("2006-01-02T15"),
		Type:     "anomaly_burst",
		Key:      diffType,
		Detail: map[string]any{
			"diff_type":    diffType,
			"hour_count":   count,
			"baseline":     fmt.Sprintf("%.2f", ewma),
			"stddev":       fmt.Sprintf("%.2f", std),
			"z_score":      fmt.Sprintf("%.2f", z),
			"threshold":    SigmaThreshold,
			"alert_reason": fmt.Sprintf("%s 过去 1h 出现 %d 条，基线均值 %.0f ± %.0f (z=%.1f)",
				diffType, count, ewma, std, z),
		},
	}
	_ = d.diffStore.CreateOpen(ctx, dx)
	d.log.Warn("anomaly alert fired",
		zap.String("type", diffType),
		zap.Int("count", count),
		zap.Float64("ewma", ewma),
		zap.Float64("z_score", z))
}
