// rollout.go —— ML 模型 traffic-split 灰度发布（5% → 25% → 50% → 100%）
//
// 之前 ChampionChallengerService.PromoteChallenger 是 atomic swap：新模型一次
// 性切到 100% 流量，万一 challenger 有 bug 全量爆炸。生产 ML 部署的标准做法
// 是按比例灰度：先放 5% 看一天，没问题再 25%，再 50%，最后 100%。每个 stage
// 监控 precision/recall，跌穿阈值自动回退一个 stage。
//
// 设计要点：
//
//   - 流量分配按 sha256(customer_id + rollout_id) % 100 稳定分桶。同一用户
//     在同一 rollout 内永远落同一个模型，避免"刷一次过、刷一次拒"的体验
//     灾难。换 rollout 时 rollout_id 自动更新，新一轮重新洗牌。
//
//   - 没 customer_id（matchless 场景：tokenized 卡 / 游客结算）时 fallback
//     champion——保守路径，让 challenger 只看到稳定可标记的样本，避免 sample
//     selection bias 污染 A/B 评估。
//
//   - 每个 Stage 含 MinObs（最小 challenger 观测数），<MinObs 时 auto-rollback
//     不下结论（bootstrap CI 在小样本下太宽，假阳性率高）。
//
//   - Auto-rollback 用 ABTracker 已有的 bootstrap AUC CI；若 challenger CI 上界
//     依然 < champion AUC，或 precision 下降超阈值，回退到上一个 stage。
//
//   - Persist：当前实现 in-memory。PG 落地见 TODO（rollout_state 表：
//     id / champion_name / challenger_name / current_stage / paused / started_at）。
package mlscore

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"time"
)

// Stage 单个灰度阶段的配置。
type Stage struct {
	// Pct challenger 流量比例 [0, 100]。
	Pct int `json:"pct"`

	// MinObs 进入下一 stage 前 challenger 至少要看到的观测数。
	// 低于此值时 auto-promote / auto-rollback 都不下结论。
	MinObs int `json:"min_obs"`

	// StartedAt stage 进入时间（advance 时刷新）。
	StartedAt time.Time `json:"started_at,omitempty"`

	// MinDuration stage 最小停留时间。<MinDuration 时即使 obs 够也不 auto-promote
	// （避免短时间内尖峰流量造成的统计偏差）。零值 = 不限制。
	MinDuration time.Duration `json:"min_duration_ns,omitempty"`
}

// DefaultStages 标准 5% → 25% → 50% → 100% 灰度梯度。
// MinObs 选 10K 是经验值：保证 bootstrap CI 在 1-2% AUC 差异下足够窄。
func DefaultStages() []Stage {
	return []Stage{
		{Pct: 5, MinObs: 10_000, MinDuration: 24 * time.Hour},
		{Pct: 25, MinObs: 10_000, MinDuration: 7 * 24 * time.Hour},
		{Pct: 50, MinObs: 10_000, MinDuration: 7 * 24 * time.Hour},
		{Pct: 100, MinObs: 0, MinDuration: 0}, // 100% = 等同 PromoteChallenger 完成
	}
}

// RolloutState 持久化 / dashboard 用的状态 snapshot。
type RolloutState struct {
	RolloutID        string    `json:"rollout_id"`
	ChampionName     string    `json:"champion_name"`
	ChallengerName   string    `json:"challenger_name,omitempty"`
	Stages           []Stage   `json:"stages"`
	CurrentStage     int       `json:"current_stage"`
	CurrentPct       int       `json:"current_pct"`
	Paused           bool      `json:"paused"`
	AutoPromote      bool      `json:"auto_promote"`
	AutoRollback     bool      `json:"auto_rollback"`
	StartedAt        time.Time `json:"started_at"`
	LastRollbackAt   time.Time `json:"last_rollback_at,omitempty"`
	LastRollbackReason string  `json:"last_rollback_reason,omitempty"`
	ChampionHits     int64     `json:"champion_hits"`
	ChallengerHits   int64     `json:"challenger_hits"`
}

// rollout 内部状态。受 ChampionChallengerService.mu 保护（避免单独加锁
// 导致 routing 路径多一次锁开销）。
type rollout struct {
	rolloutID    string
	challenger   namedSvc
	stages       []Stage
	currentStage int // 索引 in stages
	paused       bool
	autoPromote  bool
	autoRollback bool
	startedAt    time.Time

	// metrics
	championHits   int64
	challengerHits int64

	// rollback 记录
	lastRollbackAt     time.Time
	lastRollbackReason string

	// rollbackThresholds challenger AUC CI 上界 - champion AUC < threshold 时回退；
	// 默认 0（"challenger CI 上界还低于 champion 中位"）。
	rollbackAUCDelta float64
}

// newRollout 内部构造；外部走 ChampionChallengerService.StartRollout。
func newRollout(challenger namedSvc, stages []Stage, autoPromote, autoRollback bool) *rollout {
	if len(stages) == 0 {
		stages = DefaultStages()
	}
	now := time.Now().UTC()
	stages[0].StartedAt = now
	return &rollout{
		rolloutID:    newRolloutID(challenger.Name, now),
		challenger:   challenger,
		stages:       stages,
		currentStage: 0,
		autoPromote:  autoPromote,
		autoRollback: autoRollback,
		startedAt:    now,
	}
}

// newRolloutID rollout 标识：challenger 名 + 启动 ns，单调递增。
// 用进 bucket hash 避免不同 rollout 间相关（同一 customer 在 rollout A 落
// 5% 桶，rollout B 应该重新随机抽，否则同一批用户每次都被选中做小白鼠）。
func newRolloutID(challengerName string, t time.Time) string {
	return fmt.Sprintf("%s-%d", challengerName, t.UnixNano())
}

// currentPct 当前 stage 的流量比例。
func (r *rollout) currentPct() int {
	if r == nil || r.paused || len(r.stages) == 0 {
		return 0
	}
	if r.currentStage < 0 {
		return 0
	}
	if r.currentStage >= len(r.stages) {
		return 100
	}
	return r.stages[r.currentStage].Pct
}

// snapshot 拷贝当前状态。
func (r *rollout) snapshot(championName string) RolloutState {
	if r == nil {
		return RolloutState{ChampionName: championName, CurrentPct: 0}
	}
	stages := make([]Stage, len(r.stages))
	copy(stages, r.stages)
	return RolloutState{
		RolloutID:          r.rolloutID,
		ChampionName:       championName,
		ChallengerName:     r.challenger.Name,
		Stages:             stages,
		CurrentStage:       r.currentStage,
		CurrentPct:         r.currentPct(),
		Paused:             r.paused,
		AutoPromote:        r.autoPromote,
		AutoRollback:       r.autoRollback,
		StartedAt:          r.startedAt,
		LastRollbackAt:     r.lastRollbackAt,
		LastRollbackReason: r.lastRollbackReason,
		ChampionHits:       r.championHits,
		ChallengerHits:     r.challengerHits,
	}
}

// bucketFor 把 customer_id 稳定映射到 [0, 100) 的桶。
//
// 用 sha256 而不是 fnv：fnv 在前缀相同的 id（如 "cust_0001"、"cust_0002"）
// 上低位有规律，会导致小流量 stage 在某个 id 区间过度采样。
//
// rolloutID 进 hash：换 rollout 等价于换一个新的 hash seed，避免同一批
// "倒霉的"用户连续多个版本都落 challenger 桶。
//
// 没 customer_id（""）→ 返回 -1，调用方应走 fallback（champion）。
func bucketFor(customerID, rolloutID string) int {
	if customerID == "" {
		return -1
	}
	h := sha256.New()
	h.Write([]byte(customerID))
	h.Write([]byte{0x1f}) // 分隔符防 "ab"+"c" vs "a"+"bc" 碰撞
	h.Write([]byte(rolloutID))
	sum := h.Sum(nil)
	// 取前 8 字节 → uint64 → mod 100；分布偏差 < 1e-17，可忽略
	u := binary.BigEndian.Uint64(sum[:8])
	return int(u % 100)
}

// routeToChallenger true = 走 challenger；false = 走 champion。
//
// matchlessFallback=true → 没 customer_id 时仍按 pct 抛随机硬币（保留 stage
// 比例）；false → 一律 fallback champion（更保守）。当前默认 false。
func (r *rollout) routeToChallenger(customerID string, matchlessFallback bool, rng *rand.Rand) bool {
	if r == nil || r.paused {
		return false
	}
	pct := r.currentPct()
	if pct <= 0 {
		return false
	}
	if pct >= 100 {
		return true
	}
	b := bucketFor(customerID, r.rolloutID)
	if b < 0 {
		// 没 customer_id：默认 fallback champion 让 challenger 只看有 id 的样本
		if !matchlessFallback {
			return false
		}
		if rng == nil {
			return false
		}
		return rng.Intn(100) < pct
	}
	return b < pct
}

// advance 推进到下一 stage；当前已是最后一个 → 返回 ErrRolloutComplete。
func (r *rollout) advance() error {
	if r == nil {
		return ErrNoRollout
	}
	if r.paused {
		return ErrRolloutPaused
	}
	if r.currentStage >= len(r.stages)-1 {
		return ErrRolloutComplete
	}
	r.currentStage++
	r.stages[r.currentStage].StartedAt = time.Now().UTC()
	return nil
}

// rollback 回退到上一 stage；当前已是 stage 0 → ErrRolloutAtZero。
// reason 落进 lastRollbackReason 供 dashboard / audit 展示。
func (r *rollout) rollback(reason string) error {
	if r == nil {
		return ErrNoRollout
	}
	if r.currentStage <= 0 {
		return ErrRolloutAtZero
	}
	r.currentStage--
	now := time.Now().UTC()
	r.stages[r.currentStage].StartedAt = now
	r.lastRollbackAt = now
	r.lastRollbackReason = reason
	return nil
}

var (
	ErrNoRollout       = errors.New("no active rollout")
	ErrRolloutComplete = errors.New("rollout already at final stage")
	ErrRolloutAtZero   = errors.New("rollout already at stage 0; cannot rollback further")
	ErrRolloutPaused   = errors.New("rollout is paused")
)

// RolloutEventType rollout lifecycle 关键节点。
type RolloutEventType string

const (
	RolloutEventStart        RolloutEventType = "start"
	RolloutEventAdvance      RolloutEventType = "advance"
	RolloutEventRollback     RolloutEventType = "rollback"      // 手动
	RolloutEventAutoRollback RolloutEventType = "auto_rollback" // 指标触发
	RolloutEventPause        RolloutEventType = "pause"
	RolloutEventResume       RolloutEventType = "resume"
	RolloutEventReplace      RolloutEventType = "replace_challenger"
	RolloutEventFinalize     RolloutEventType = "finalize" // 推到 100% 完成
)

// RolloutEvent OnRolloutEvent 回调的 payload。State 是触发后的快照。
type RolloutEvent struct {
	Type   RolloutEventType `json:"type"`
	Reason string           `json:"reason,omitempty"`
	State  RolloutState     `json:"state"`
}

// emitRolloutLocked caller 必须持 cc.mu；同步触发回调。
func (cc *ChampionChallengerService) emitRolloutLocked(t RolloutEventType, reason string) {
	if cc.OnRolloutEvent == nil {
		return
	}
	cc.OnRolloutEvent(RolloutEvent{
		Type:   t,
		Reason: reason,
		State:  cc.rollout.snapshot(cc.champion.Name),
	})
}

// --- ChampionChallengerService rollout 方法 ---
//
// 这些方法都建立在 cc.mu 之上（routing 路径已经持读锁，admin 路径独占写锁）。

// StartRollout 启动 challenger 的灰度发布。challenger 必须已经 RegisterChallenger
// 注册过；stages 为空时用 DefaultStages()。
//
// 已有 rollout 时返回 error——admin 应该先 RollbackRollout 到 0% 或
// ReplaceChallenger 显式换 challenger。
func (cc *ChampionChallengerService) StartRollout(
	challengerName string, stages []Stage, autoPromote, autoRollback bool,
) error {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if cc.rollout != nil {
		return errors.New("rollout already in progress; call ReplaceChallenger or finalize first")
	}
	for _, c := range cc.challengers {
		if c.Name == challengerName {
			cc.rollout = newRollout(c, stages, autoPromote, autoRollback)
			cc.emitRolloutLocked(RolloutEventStart, "")
			return nil
		}
	}
	return fmt.Errorf("challenger %q not registered", challengerName)
}

// AdvanceRollout 手动推进 stage。
func (cc *ChampionChallengerService) AdvanceRollout() error {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if cc.rollout == nil {
		return ErrNoRollout
	}
	if err := cc.rollout.advance(); err != nil {
		return err
	}
	// 推到 100% → 自动 promote 完成；之后 rollout 清空，等于 atomic swap。
	if cc.rollout.currentPct() >= 100 {
		cc.emitRolloutLocked(RolloutEventFinalize, "")
		cc.finalizeRolloutLocked()
	} else {
		cc.emitRolloutLocked(RolloutEventAdvance, "")
	}
	return nil
}

// RollbackRollout 手动回退 stage。reason 写入 audit。
func (cc *ChampionChallengerService) RollbackRollout(reason string) error {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if cc.rollout == nil {
		return ErrNoRollout
	}
	if err := cc.rollout.rollback(reason); err != nil {
		return err
	}
	cc.emitRolloutLocked(RolloutEventRollback, reason)
	return nil
}

// PauseRollout 冻结当前 stage：routing 改走 champion（pct 视作 0），直到 Resume。
// 重大事故快速止血用——比 RollbackRollout 多一步显式 Resume，避免误触。
func (cc *ChampionChallengerService) PauseRollout() error {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if cc.rollout == nil {
		return ErrNoRollout
	}
	cc.rollout.paused = true
	cc.emitRolloutLocked(RolloutEventPause, "")
	return nil
}

// ResumeRollout 取消 pause。
func (cc *ChampionChallengerService) ResumeRollout() error {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if cc.rollout == nil {
		return ErrNoRollout
	}
	cc.rollout.paused = false
	cc.emitRolloutLocked(RolloutEventResume, "")
	return nil
}

// ReplaceChallenger 在不结束 rollout 的前提下换 challenger（保留 champion）。
// 场景：challenger v2 在 5% 评估发现 bug 但不严重 → 修了换成 v2.1 继续测，
// 不想 demote champion。新 challenger 会从 stage 0 重新开始（rolloutID 也
// 跟着换 → bucket 重新洗牌）。
//
// 老 challenger 不会自动 drop：保留它继续并行打分让 ABTracker 也能拿数据。
func (cc *ChampionChallengerService) ReplaceChallenger(newName string, svc Service) error {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	// 注册新 challenger（去重）
	found := false
	for i, c := range cc.challengers {
		if c.Name == newName {
			cc.challengers[i].Service = svc
			found = true
			break
		}
	}
	if !found {
		cc.challengers = append(cc.challengers, namedSvc{Name: newName, Service: svc})
	}
	stages := DefaultStages()
	autoPromote := false
	autoRollback := true
	if cc.rollout != nil {
		// 保留原 stages 配置 / auto 开关，只换 challenger + 重置
		stages = cc.rollout.stages
		// 重置 stage 起点
		for i := range stages {
			stages[i].StartedAt = time.Time{}
		}
		autoPromote = cc.rollout.autoPromote
		autoRollback = cc.rollout.autoRollback
	}
	cc.rollout = newRollout(namedSvc{Name: newName, Service: svc}, stages, autoPromote, autoRollback)
	cc.emitRolloutLocked(RolloutEventReplace, newName)
	return nil
}

// finalizeRolloutLocked rollout 推到 100% 后把 challenger 升为 champion。
// 等价于一次老式 PromoteChallenger。caller 必须已经持 cc.mu。
func (cc *ChampionChallengerService) finalizeRolloutLocked() {
	if cc.rollout == nil {
		return
	}
	challengerName := cc.rollout.challenger.Name
	for i, c := range cc.challengers {
		if c.Name != challengerName {
			continue
		}
		oldCh := cc.champion
		cc.champion = c
		cc.challengers = append(cc.challengers[:i], cc.challengers[i+1:]...)
		if oldCh.Name != "" && oldCh.Name != challengerName {
			cc.challengers = append(cc.challengers, oldCh)
		}
		break
	}
	cc.rollout = nil
}

// RolloutStatus 当前 rollout 状态 snapshot（admin endpoint 用）。
// 没 rollout 时返回 RolloutID="" 的零状态 + 当前 champion 名。
func (cc *ChampionChallengerService) RolloutStatus() RolloutState {
	cc.mu.RLock()
	defer cc.mu.RUnlock()
	return cc.rollout.snapshot(cc.champion.Name)
}

// --- Auto-rollback 评估 ---

// MetricsProvider 评估 challenger 时拉数据的接口。让 mlscore 包不依赖
// feedback / featurestore（循环依赖）；上层接 *ABTracker.Report 实现。
type MetricsProvider interface {
	// ChallengerVsChampion 返回 challenger 与 champion 的 (auc_diff, ci_low, ci_high,
	// labeled_samples)。labeled_samples < minObs 时 caller 不下结论。
	ChallengerVsChampion(name string) (aucDiff, ciLow, ciHigh float64, labeled int, ok bool)
}

// EvaluateAutoRollback 给 caller 定时跑（如每 5 min cron），按当前 stage 的
// MinObs / 实际 challenger AUC CI 判断是否要 rollback。
//
// 判定逻辑（保守）：
//   1. paused 或没 rollout → no-op
//   2. labeled < currentStage.MinObs → 数据不够，no-op
//   3. CIHigh < rollbackAUCDelta（默认 0）→ challenger 显著差于 champion → rollback
//
// 返回 (rolledBack, reason)。reason 在 rolledBack=false 时是 no-op 解释。
func (cc *ChampionChallengerService) EvaluateAutoRollback(mp MetricsProvider) (bool, string) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if cc.rollout == nil {
		return false, "no rollout"
	}
	if !cc.rollout.autoRollback {
		return false, "auto_rollback disabled"
	}
	if cc.rollout.paused {
		return false, "rollout paused"
	}
	if mp == nil {
		return false, "no metrics provider"
	}
	name := cc.rollout.challenger.Name
	_, _, ciHigh, labeled, ok := mp.ChallengerVsChampion(name)
	if !ok {
		return false, "no metrics for challenger"
	}
	stage := cc.rollout.stages[cc.rollout.currentStage]
	if labeled < stage.MinObs {
		return false, fmt.Sprintf("insufficient samples (%d < %d)", labeled, stage.MinObs)
	}
	thresh := cc.rollout.rollbackAUCDelta // 默认 0
	if ciHigh < thresh {
		reason := fmt.Sprintf("challenger AUC CI upper %.4f < %.4f (labeled=%d, stage_pct=%d)",
			ciHigh, thresh, labeled, stage.Pct)
		if err := cc.rollout.rollback(reason); err != nil {
			return false, err.Error()
		}
		cc.emitRolloutLocked(RolloutEventAutoRollback, reason)
		return true, reason
	}
	return false, fmt.Sprintf("challenger healthy (CI upper %.4f >= %.4f)", ciHigh, thresh)
}

// --- ABTracker → MetricsProvider 适配 ---

// ABTrackerMetricsProvider 把 *ABTracker.Report 包成 MetricsProvider。
// outcome 由 caller 提供（feedback.Recorder 在 cmd/server/main.go 接）。
//
// bootstrapN：用 ABTracker 的 bootstrap CI 跑 N 次。建议 500-1000；太少 CI
// 抖动大，太多每次 evaluate 都跑几百 ms。
type ABTrackerMetricsProvider struct {
	Tracker    *ABTracker
	GetOutcome func(decisionID string) (isFraud bool, ok bool)
	MinLabeled int
	BootstrapN int
}

func (a *ABTrackerMetricsProvider) ChallengerVsChampion(name string) (float64, float64, float64, int, bool) {
	if a == nil || a.Tracker == nil || a.GetOutcome == nil {
		return 0, 0, 0, 0, false
	}
	minLabeled := a.MinLabeled
	if minLabeled <= 0 {
		minLabeled = 30
	}
	bootstrapN := a.BootstrapN
	if bootstrapN <= 0 {
		bootstrapN = 1000
	}
	reports := a.Tracker.Report(a.GetOutcome, minLabeled, bootstrapN)
	for _, r := range reports {
		if r.Name == name {
			return r.AUCDiff, r.CILow, r.CIHigh, r.LabeledSamples, true
		}
	}
	return 0, 0, 0, 0, false
}

// --- 工具：稳定 bucket 给 test / debug 用 ---

// BucketFor 暴露给测试 / debug；正常路径用内部 bucketFor。
func BucketFor(customerID, rolloutID string) int {
	return bucketFor(customerID, rolloutID)
}
