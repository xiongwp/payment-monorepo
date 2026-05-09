// Package webhook 实现可靠的 Webhook 投递系统。
//
// 核心能力：
//   - HMAC-SHA256 签名（商户用 webhook_secret 验证来源）
//   - 指数退避重试（5 次：0s / 30s / 2min / 15min / 2h）
//   - DB 持久化（进程重启不丢）
//   - 后台 worker 轮询 pending + retry-ready 的 delivery 重发
//
// 签名格式（对齐 Stripe 风格）：
//   header X-Signature: t=<unix_ts>,v1=<HMAC-SHA256 hex>
//   签名内容 = t + "." + body
//   商户验证：sha256_hmac(webhook_secret, t + "." + body) == v1
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	mathrand "math/rand"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xiongwp/order-core/internal/metrics"
	"github.com/xiongwp/order-core/internal/shadow"
	"github.com/xiongwp/payment-util/trace"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// Event 要投递的事件
type Event struct {
	ID         string // 全局唯一事件 ID（idgen 产）
	MerchantID string
	Type       string // payment_intent.succeeded / charge.failed / refund.succeeded / ...
	Payload    interface{}
}

// Delivery DB 行
type Delivery struct {
	ID          int64      `gorm:"primaryKey;autoIncrement"`
	MerchantID  string     `gorm:"column:merchant_id"`
	EventID     string     `gorm:"column:event_id;uniqueIndex"`
	EventType   string     `gorm:"column:event_type"`
	Payload     string     `gorm:"column:payload;type:json"`
	URL         string     `gorm:"column:url"`
	Status      string     `gorm:"column:status"` // pending / succeeded / failed / exhausted
	HTTPStatus  int        `gorm:"column:http_status"`
	Attempts    int        `gorm:"column:attempts"`
	MaxAttempts int        `gorm:"column:max_attempts"`
	NextRetryAt *time.Time `gorm:"column:next_retry_at"`
	LastError   string     `gorm:"column:last_error"`
	// ClaimToken worker claim 一批待投递行时写入；SELECT WHERE claim_token = ?
	// 即可拉到刚 claim 的批次。空 = 未被任何 worker claim。
	ClaimToken string    `gorm:"column:claim_token"`
	CreatedAt  time.Time `gorm:"column:created_at"`
	UpdatedAt  time.Time `gorm:"column:updated_at"`
}

// TableName 默认主表（gorm 默认 fallback）。
// 影子流量必须显式走 dispatcherTable(ctx) / Dispatcher.tbl(ctx)，gorm 默认不会感知 ctx。
func (Delivery) TableName() string { return "webhook_deliveries" }

// dispatcherTable 给定 ctx 返回 webhook_deliveries 主表 / 影子表名。
// shadow=true → "webhook_deliveries_shadow"
const webhookDeliveriesBase = "webhook_deliveries"

func dispatcherTable(ctx context.Context) string {
	return shadow.TableName(ctx, webhookDeliveriesBase)
}

// DefaultRetryDelays 指数退避间隔（可通过 DispatcherConfig 覆盖）
var DefaultRetryDelays = []time.Duration{
	0,                    // 立即
	30 * time.Second,     // 30s
	2 * time.Minute,      // 2m
	15 * time.Minute,     // 15m
	2 * time.Hour,        // 2h
}

// DispatcherConfig 投递器配置
type DispatcherConfig struct {
	RetryDelays []time.Duration // 空 = 用 DefaultRetryDelays
	BatchSize   int             // 每次 processRetries 抓多少条（默认 20）
}

// Dispatcher Webhook 投递器
type Dispatcher struct {
	db        *gorm.DB
	h         *http.Client
	delays    []time.Duration
	batchSize int
	sem       chan struct{} // 全局并发投递信号量，防止 goroutine 爆炸
	// P2-11 per-merchant QoS：每个 merchant 独立 chan struct{} 信号量，限制单
	// merchant 同时 in-flight 投递数（默认 5）。一个慢 endpoint 不会把全局
	// 50 个 slot 全占住、拖累其他 merchant 的投递。lazy init via sync.Map。
	merchantSems    sync.Map // map[merchantID]chan struct{}
	merchantConcur  int      // 单 merchant 并发上限（默认 5)
	// workerID 实例唯一标识：hostname (k8s 下 = pod 名) + 随机后缀。
	// claim_token 用 workerID + 进程内单调计数器，避免多 worker 极端 race 撞 token。
	workerID    string
	claimSeq    atomic.Uint64
	logger      *zap.Logger
}

func NewDispatcher(db *gorm.DB, cfg DispatcherConfig, logger *zap.Logger) *Dispatcher {
	delays := cfg.RetryDelays
	if len(delays) == 0 {
		delays = DefaultRetryDelays
	}
	batch := cfg.BatchSize
	if batch <= 0 {
		batch = 20
	}
	return &Dispatcher{
		db:             db,
		h:              &http.Client{Timeout: 10 * time.Second},
		delays:         delays,
		batchSize:      batch,
		sem:            make(chan struct{}, 50), // 最多 50 个并发投递 goroutine
		merchantConcur: 5,                       // 默认单 merchant 最多 5 路并发投递
		workerID:       generateWorkerID(),
		logger:         logger,
	}
}

// generateWorkerID 实例启动时一次性生成。
//
// 格式：<hostname>:<8byte hex random>
// 多个 pod 同 hostname 概率为零（k8s 一 pod 一 hostname）；
// 同 hostname 多进程靠 random 后缀分。
func generateWorkerID() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown"
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败极罕见；退化用纳秒
		return fmt.Sprintf("%s:%d", host, time.Now().UnixNano())
	}
	return host + ":" + hex.EncodeToString(b[:])
}

// acquireMerchantSlot 给指定 merchant 占一个 in-flight 投递槽位。
// 返回 false = 该 merchant 已达并发上限，调用方应放弃本次立即投递（retry worker 之后会接）。
// release 必须在投递结束（成功 / 失败 / panic 都要）调一次。
func (d *Dispatcher) acquireMerchantSlot(merchantID string) (release func(), ok bool) {
	if merchantID == "" {
		// 没有 merchantID（极少：旧数据 / 系统事件）→ 跳过 per-merchant 限流，
		// 仅靠全局 sem 兜底。
		return func() {}, true
	}
	v, _ := d.merchantSems.LoadOrStore(merchantID, make(chan struct{}, d.merchantConcur))
	ch := v.(chan struct{})
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, true
	default:
		return nil, false
	}
}

// Enqueue 把事件写入 DB 等待投递。幂等（event_id UNIQUE）。
// ctx 中的 shadow flag 决定写入主表还是影子表（webhook_deliveries / _shadow）。
func (d *Dispatcher) Enqueue(ctx context.Context, url, secret string, evt Event) error {
	payload, _ := json.Marshal(evt.Payload)
	delivery := Delivery{
		MerchantID:  evt.MerchantID,
		EventID:     evt.ID,
		EventType:   evt.Type,
		Payload:     string(payload),
		URL:         url,
		Status:      "pending",
		MaxAttempts: len(d.delays),
	}
	tbl := dispatcherTable(ctx)
	err := d.db.WithContext(ctx).Table(tbl).Create(&delivery).Error
	if err != nil {
		return err
	}
	// 立即尝试第一次投递（异步，不阻塞调用方）。
	// async goroutine 用 context.WithoutCancel 保留 ctx 值（含 shadow flag）但脱离 cancel 链；
	// 否则 Enqueue 返回后 ctx canceled，tryDeliver 内部 NewRequestWithContext 直接报错。
	asyncCtx := contextWithoutCancel(ctx)
	// P2-11: per-merchant QoS。一个 merchant 同时只允许 N 条 in-flight。
	// 超额时不阻塞 Enqueue，让 retry worker 之后接。
	merchantRelease, mchOK := d.acquireMerchantSlot(evt.MerchantID)
	if !mchOK {
		d.logger.Info("immediate delivery deferred (per-merchant QoS limit hit)",
			zap.String("event_id", delivery.EventID),
			zap.String("merchant_id", evt.MerchantID))
		return nil
	}
	select {
	case d.sem <- struct{}{}:
		go func() {
			defer func() { <-d.sem }()
			defer merchantRelease()
			d.tryDeliver(asyncCtx, delivery.ID, url, secret, payload)
		}()
	default:
		// 队列满，下一轮 retry worker（30s 内）会捡起这条 pending。
		merchantRelease()
		d.logger.Info("immediate delivery deferred to retry worker",
			zap.String("event_id", delivery.EventID))
	}
	return nil
}

// contextWithoutCancel 是 context.WithoutCancel 的兼容垫片（Go 1.21+ 已内置）。
// 这里手写一份避免对最低 Go 版本的硬要求；只保留 Value 不传 cancel/deadline。
func contextWithoutCancel(parent context.Context) context.Context {
	return detachedContext{parent: parent}
}

type detachedContext struct{ parent context.Context }

func (detachedContext) Deadline() (time.Time, bool)       { return time.Time{}, false }
func (detachedContext) Done() <-chan struct{}             { return nil }
func (detachedContext) Err() error                        { return nil }
func (d detachedContext) Value(key interface{}) interface{} { return d.parent.Value(key) }

// RunRetryWorker 后台 worker：轮询 pending + retry-ready，重发。
//
// 每个 tick 用 trace.NewBackground 重置 ctx：
//   - 新 trace_id：单 cycle 在日志可聚合
//   - **shadow=false 强制**：webhook 出站给商户是真请求，不能漏带 shadow=true
//     把压测流量发到真商户回调地址。
func (d *Dispatcher) RunRetryWorker(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tickCtx, cancel := trace.NewBackground(ctx, "webhook-retry-worker", d.logger, interval)
			d.processRetries(tickCtx)
			cancel()
		}
	}
}

func (d *Dispatcher) processRetries(ctx context.Context) {
	now := time.Now()

	// 多实例并发安全：原 SELECT ... FOR UPDATE SKIP LOCKED 写在 Raw().Scan()，
	// MySQL 行锁仅在该语句执行期间生效——语句返回后锁立即释放，多实例仍可能拿到
	// 重叠批次。改成两步 claim：先 UPDATE ... LIMIT 把候选行的 next_retry_at 推到
	// 远未来 + 写入 claim_marker 时间戳（用 attempts 不变保证幂等），再 SELECT
	// 这些被 marker 标记的行处理。这样无需事务也能保证每条 delivery 在 batchSize
	// 时间窗内只被一个实例处理。
	//
	// 多实例并发安全：两步 claim。
	//   1. UPDATE ... LIMIT N SET next_retry_at=远未来, claim_token=本 worker 唯一值
	//      其他 worker 的 SELECT 会 skip（next_retry_at 在未来 + claim_token 已写）
	//   2. SELECT WHERE claim_token = ? 拉回本批次详细数据处理
	//
	// claim_token 是独立列（idx_claim 索引），与业务字段 last_error 解耦：
	// 历史 last_error 不再被 claim 操作覆盖，便于排查投递失败原因。
	// claim_token 必须实例间唯一：workerID（启动时一次性生成）+ 进程内单调计数器。
	// 同一 worker 不会重复，跨 worker hostname+random 分开，碰撞概率为零。
	seq := d.claimSeq.Add(1)
	claimToken := fmt.Sprintf("claim:%s:%d", d.workerID, seq)
	farFuture := now.Add(24 * time.Hour)
	tbl := dispatcherTable(ctx) // 主表 / 影子表（按 ctx）
	// 表名是受信常量（webhook_deliveries / _shadow），用 fmt.Sprintf 拼入 SQL 安全。
	updateRes := d.db.WithContext(ctx).Exec(
		fmt.Sprintf(
			"UPDATE %s "+
				"SET next_retry_at = ?, claim_token = ? "+
				"WHERE id IN ("+
				"  SELECT id FROM ("+
				"    SELECT id FROM %s "+
				"    WHERE status IN ('pending','failed') "+
				"      AND (next_retry_at IS NULL OR next_retry_at <= ?) "+
				"      AND attempts < max_attempts "+
				"    ORDER BY next_retry_at ASC LIMIT ?"+
				"  ) AS t "+
				") ", tbl, tbl),
		farFuture, claimToken, now, d.batchSize,
	)
	if updateRes.Error != nil {
		d.logger.Warn("webhook claim batch failed", zap.Error(updateRes.Error))
		return
	}
	if updateRes.RowsAffected == 0 {
		return
	}

	var deliveries []Delivery
	if err := d.db.WithContext(ctx).Table(tbl).
		Where("claim_token = ?", claimToken).
		Find(&deliveries).Error; err != nil {
		d.logger.Warn("webhook claim re-read failed", zap.Error(err))
		return
	}

	// wave L: 批量拉 merchant webhook_secret，避免 N+1。
	mchSecrets := d.fetchMerchantSecrets(ctx, deliveries)
	for _, del := range deliveries {
		d.tryDeliver(ctx, del.ID, del.URL, mchSecrets[del.MerchantID], []byte(del.Payload))
	}
}

// fetchMerchantSecrets batch-loads webhook_secret for the set of merchant IDs
// present in deliveries, replacing the per-delivery query that used to sit in
// the hot retry loop.
func (d *Dispatcher) fetchMerchantSecrets(ctx context.Context, deliveries []Delivery) map[string]string {
	if len(deliveries) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(deliveries))
	ids := make([]string, 0, len(deliveries))
	for _, del := range deliveries {
		if _, ok := seen[del.MerchantID]; ok {
			continue
		}
		seen[del.MerchantID] = struct{}{}
		ids = append(ids, del.MerchantID)
	}
	type row struct {
		ID            string `gorm:"column:id"`
		WebhookSecret string `gorm:"column:webhook_secret"`
	}
	var rows []row
	if err := d.db.WithContext(ctx).
		Table("merchants").
		Select("id, webhook_secret").
		Where("id IN ?", ids).
		Scan(&rows).Error; err != nil {
		d.logger.Warn("batch merchant secret lookup failed", zap.Error(err))
		return map[string]string{}
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.ID] = r.WebhookSecret
	}
	return out
}

func (d *Dispatcher) tryDeliver(parent context.Context, id int64, url, secret string, payload []byte) {
	tbl := dispatcherTable(parent) // 主 / 影子表
	// 影子流量绝不真发到商户：直接标 succeeded + last_error 注明 dryrun，
	// 投递记录留在 webhook_deliveries_shadow 供压测核查。商户生产 URL 不被打扰。
	if shadow.IsShadow(parent) {
		metrics.WebhookDeliveryTotal.WithLabelValues("shadow_dryrun").Inc()
		d.db.WithContext(parent).Table(tbl).Where("id = ?", id).Updates(map[string]interface{}{
			"status":      "succeeded",
			"http_status": 0,
			"attempts":    gorm.Expr("attempts + 1"),
			"last_error":  "shadow:dryrun (HTTP suppressed)",
		})
		d.logger.Info("webhook shadow dryrun (no HTTP send)",
			zap.Int64("delivery_id", id), zap.String("url", url))
		return
	}
	if url == "" {
		d.markFailed(parent, tbl, id, 0, "webhook URL is empty", 0)
		return
	}
	if secret == "" {
		d.markFailed(parent, tbl, id, 0, "webhook secret is empty — refusing to send unsigned webhook", 0)
		return
	}
	ts := fmt.Sprintf("%d", time.Now().Unix())
	sig := sign(secret, ts, payload)

	// 给 HTTP 调用一个 10s 上限，但保留 parent 中的 shadow flag。
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		d.markFailed(parent, tbl, id, 0, err.Error(), 0)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Event-Type", "")
	req.Header.Set("X-Signature", fmt.Sprintf("t=%s,v1=%s", ts, sig))
	req.Header.Set("User-Agent", "PaymentGateway-Webhook/1.0")
	// X-Trace-ID 把当前 ctx 上的 trace_id 透传到商户侧。
	// 故障排查时商户报"webhook 收不到"，他们带着 X-Trace-ID 回到我们后台
	// 一查全链路（api-gateway → order-core → webhook）就明朗，不用按
	// event_id 反向 grep N 个服务的日志。
	if tid := trace.FromContext(parent); tid != "" {
		req.Header.Set("X-Trace-ID", tid)
	}
	// X-Webhook-Delivery-ID 让商户按 (event_id, delivery_id) 做更细去重。
	// 同一事件历史 retry 多次时 delivery_id 不变（=DB 行 id），event_id 相同，
	// 所以这个 header 是给商户审计 / 客服使用的 1:1 反查锚点。
	req.Header.Set("X-Webhook-Delivery-ID", strconv.FormatInt(id, 10))

	deliveryStart := time.Now()
	resp, err := d.h.Do(req)
	metrics.WebhookDeliveryLatency.Observe(time.Since(deliveryStart).Seconds())
	if err != nil {
		metrics.WebhookDeliveryTotal.WithLabelValues("transport_error").Inc()
		d.markFailed(parent, tbl, id, 0, err.Error(), 0)
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		metrics.WebhookDeliveryTotal.WithLabelValues("succeeded").Inc()
		// 关键：必须 .WithContext(parent) — tbl 已经经 shadow.TableName(ctx) 解出
		// 影子后缀，但 GORM callback 路径里 stmt.Context 来自 db 链上的 WithContext，
		// 漏带会让 callback 看不到 IsShadow=true，shadow 流量更新最终落到主表。
		d.db.WithContext(parent).Table(tbl).Where("id = ?", id).Updates(map[string]interface{}{
			"status":      "succeeded",
			"http_status": resp.StatusCode,
			"attempts":    gorm.Expr("attempts + 1"),
			"last_error":  "",
		})
		d.logger.Info("webhook delivered",
			zap.Int64("delivery_id", id), zap.Int("status", resp.StatusCode))
		return
	}
	retryAfterSec := parseRetryAfter(resp.Header.Get("Retry-After"))
	metrics.WebhookDeliveryTotal.WithLabelValues("http_error").Inc()
	d.markFailed(parent, tbl, id, resp.StatusCode, string(respBody), retryAfterSec)
}

// parseRetryAfter 解析 RFC 7231 §7.1.3：HTTP-date 或 delta-seconds。
// 不支持 / 失败 → 返回 0（让默认指数退避兜底）。
// 上限 3600 秒，防止商户传超长延迟挂死队列。
func parseRetryAfter(s string) int {
	if s == "" {
		return 0
	}
	if v, err := strconv.Atoi(s); err == nil && v > 0 {
		if v > 3600 {
			return 3600
		}
		return v
	}
	if t, err := http.ParseTime(s); err == nil {
		v := int(time.Until(t).Seconds())
		if v <= 0 {
			return 0
		}
		if v > 3600 {
			return 3600
		}
		return v
	}
	return 0
}

// markFailed 把 delivery 推到下一次重试。retryAfterSec>0 时优先使用商户给的
// Retry-After 时长（被 parseRetryAfter 限制为 ≤3600s）；否则用默认指数退避表
// 加 ±25% jitter——同一时刻大批失败时（典型场景：商户回调端宕机）jitter 把
// 重试时刻铺开到一个区间，避免下一波 retry tick 撞同一秒造成"二次雷暴"。
// markFailed 推下一次重试。tbl 由调用方根据 ctx 解析（主表 / 影子表），
// 这样同一行不会跨表跳转。
func (d *Dispatcher) markFailed(ctx context.Context, tbl string, id int64, httpStatus int, errMsg string, retryAfterSec int) {
	var del Delivery
	d.db.WithContext(ctx).Table(tbl).Where("id = ?", id).First(&del)

	nextAttempt := del.Attempts + 1
	updates := map[string]interface{}{
		"attempts":    nextAttempt,
		"http_status": httpStatus,
		"last_error":  errMsg,
	}
	if nextAttempt >= del.MaxAttempts {
		updates["status"] = "exhausted"
		metrics.WebhookDeliveryTotal.WithLabelValues("exhausted").Inc()
		d.logger.Warn("webhook exhausted",
			zap.Int64("delivery_id", id), zap.String("merchant", del.MerchantID), zap.String("event", del.EventID))
	} else {
		updates["status"] = "failed"
		var delay time.Duration
		if retryAfterSec > 0 {
			// 商户主动指示 → 完全尊重，不加 jitter（jitter 反而会违背 hint 语义）。
			delay = time.Duration(retryAfterSec) * time.Second
		} else {
			delay = d.delays[0]
			if nextAttempt < len(d.delays) {
				delay = d.delays[nextAttempt]
			}
			delay = jitter(delay)
		}
		next := time.Now().Add(delay)
		updates["next_retry_at"] = next
		d.logger.Info("webhook retry scheduled",
			zap.Int64("delivery_id", id),
			zap.Int("attempt", nextAttempt),
			zap.Time("next", next),
			zap.Bool("retry_after_hint", retryAfterSec > 0))
	}
	d.db.WithContext(ctx).Table(tbl).Where("id = ?", id).Updates(updates)
}

// jitter 给定 base 加上 ±25% 随机扰动，避免大批同时失败的 retry 撞在同一秒。
// 0 / 负值原样返回（不能 jitter 一个 0）。
func jitter(base time.Duration) time.Duration {
	if base <= 0 {
		return base
	}
	// math/rand 全局 source 在 Go 1.20+ 是 racy-safe 且每次进程随机种子，对 jitter 用例足够。
	spread := float64(base) * 0.5 * (mathrand.Float64() - 0.5) // [-25%, +25%]
	out := base + time.Duration(spread)
	if out <= 0 {
		return base
	}
	return out
}

// ─── HMAC-SHA256 签名（Stripe 风格） ───────────────────────────────

func sign(secret, timestamp string, body []byte) string {
	msg := timestamp + "." + string(body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(msg))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifySignature 商户侧验签辅助函数（可给 SDK 用）
func VerifySignature(secret, signatureHeader string, body []byte, tolerance time.Duration) bool {
	var ts, v1 string
	for _, part := range splitSig(signatureHeader) {
		if len(part) > 2 && part[:2] == "t=" {
			ts = part[2:]
		}
		if len(part) > 3 && part[:3] == "v1=" {
			v1 = part[3:]
		}
	}
	if ts == "" || v1 == "" {
		return false
	}
	// 时间偏差检查（防重放）
	var tsUnix int64
	fmt.Sscanf(ts, "%d", &tsUnix)
	if tolerance > 0 {
		diff := time.Since(time.Unix(tsUnix, 0))
		if diff < 0 {
			diff = -diff
		}
		if diff > tolerance {
			return false
		}
	}
	expected := sign(secret, ts, body)
	return hmac.Equal([]byte(expected), []byte(v1))
}

func splitSig(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
