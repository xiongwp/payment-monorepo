// Package service 是 risk-manage 的业务层，桥接 gRPC server 和 engine。
package service

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/payment-util/shadow"
	"github.com/xiongwp/risk-manage/internal/audit"
	"github.com/xiongwp/risk-manage/internal/engine"
	"github.com/xiongwp/risk-manage/internal/eventbus"
	"github.com/xiongwp/risk-manage/internal/featurestore"
	"github.com/xiongwp/risk-manage/internal/features"
	"github.com/xiongwp/risk-manage/internal/feedback"
	"github.com/xiongwp/risk-manage/internal/ipintel"
	"github.com/xiongwp/risk-manage/internal/metrics"
	"github.com/xiongwp/risk-manage/internal/mlscore"
	"github.com/xiongwp/risk-manage/internal/mloverride"
	"github.com/xiongwp/risk-manage/internal/auth"
	"github.com/xiongwp/risk-manage/internal/reliability"
	"github.com/xiongwp/risk-manage/internal/review"
	"github.com/xiongwp/risk-manage/internal/safety"
	"github.com/xiongwp/risk-manage/internal/sandbox"
	"github.com/xiongwp/risk-manage/internal/session"
	"github.com/xiongwp/risk-manage/internal/store"
	"github.com/xiongwp/risk-manage/internal/webhook"
)

// decisionScorer ML service 的可选扩展：透传 decision_id 让
// ChampionChallengerService 的 SideEffect 拿到真实 id（而不是 ""）。
// 任何实现 ScoreWithDecisionID 的 mlscore.Service 自动 satisfy。
type decisionScorer interface {
	ScoreWithDecisionID(ctx context.Context, decisionID string, f mlscore.Features) (mlscore.Result, error)
}

type RiskService struct {
	engine    *engine.Engine
	counter   store.Counter
	links     store.LinkStore    // nil-safe
	ipIntel   ipintel.Service    // nil-safe
	mlSvc     mlscore.Service    // nil-safe
	sessions  session.Store      // nil-safe
	reviewQ   review.Store       // nil-safe：缺省 verdict=Review 时不入队
	fbRec     feedback.Recorder  // nil-safe：dispute / chargeback 通过 RecordOutcome 写入
	wh        *webhook.Publisher    // nil-safe：商户没订阅 webhook 时不推送
	auditSink audit.Sink            // nil-safe
	bus       eventbus.Publisher    // nil-safe：默认 NoopBus，Screen/Report 完成后异步发布事件
	features  featurestore.Store    // nil-safe：ML 训练样本 sink；nil = 不落 snapshot
	idem      *idempotencyCache  // 永远 non-nil（New / NewWithAudit 内分配）
	// predebits 把 Screen Allow 时落到 counter 的预扣 key 按 IdempotencyKey
	// 索引；Report 阶段查询命中即跳 Incr 避免双计。永远 non-nil（New 内分配）。
	predebits *store.PredebitCommits
	// 下游依赖熔断器：连续失败 → 短路，避免拖死 Screen 主路径 SLA
	ipBreaker *reliability.Breaker
	mlBreaker *reliability.Breaker
	mlDrift   *mlscore.DriftMonitor // nil-safe：ML 分数分布漂移监控
	mlOverride *mloverride.Store    // nil-safe：运营手动 ML 降级开关
	slowThreshold float64 // Screen 总耗时（秒）超此值打 slow log；0=关
	// FeatureExtractor chain：在 Evaluate 前富化 TxnContext（time / card / customer history / ...）
	extractors features.Chain
	// timeouts per-stage timeout budget。SetScreenTimeouts 注入；零值 → activeTimeouts()
	// 退到 defaults（宽松，1s per stage，ceiling=off）。详见 screen_timeouts.go。
	timeouts   ScreenTimeouts
	logger     *zap.Logger
}

// SetExtractors 注入特征提取链。Evaluate 前按顺序跑一遍。nil/空 → 跳过。
func (s *RiskService) SetExtractors(c features.Chain) { s.extractors = c }

// SetMLDrift 注入 ML 分数漂移监控（main.go 启动时调；nil = 不监控）。
func (s *RiskService) SetMLDrift(d *mlscore.DriftMonitor) { s.mlDrift = d }

// SetMLOverride 注入运营 ML 降级开关。nil = 永远走正常 ML 路径。
func (s *RiskService) SetMLOverride(o *mloverride.Store) { s.mlOverride = o }

// SetSlowThreshold 设置 Screen 总耗时 slow log 阈值（秒）。0 关闭。
func (s *RiskService) SetSlowThreshold(secs float64) { s.slowThreshold = secs }

// SetBreakers 注入下游熔断器。nil 等于不熔断（保留旧行为）。
func (s *RiskService) SetBreakers(ip, ml *reliability.Breaker) {
	s.ipBreaker = ip
	s.mlBreaker = ml
}

// SetWebhookPublisher 注入异步 webhook 推送（main.go 在初始化后调一次）。
func (s *RiskService) SetWebhookPublisher(p *webhook.Publisher) { s.wh = p }

// SetEventBus 注入事件总线（main.go 启动时调；nil → NoopBus 不发事件）。
// Screen 完成后发 TopicScreen，每条命中规则发 TopicHit；Report 后发 TopicReport。
// 所有发布是异步的（goroutine + 短超时），不阻塞主路径 SLA。
func (s *RiskService) SetEventBus(b eventbus.Publisher) {
	if b == nil {
		s.bus = eventbus.NoopBus{}
		return
	}
	s.bus = b
}

// SetFeatureStore 注入 ML 特征快照存储（main.go 启动时调；nil → 不落样本）。
// 每条 Screen 异步 Save 一份 (features, decision_id, verdict)；OutcomeRecorder
// 拿到 chargeback / dispute 后通过 SetOutcome 反向 join label。
func (s *RiskService) SetFeatureStore(fs featurestore.Store) { s.features = fs }

// New 默认构造。
func New(eng *engine.Engine, counter store.Counter, logger *zap.Logger) *RiskService {
	return &RiskService{
		engine:    eng,
		counter:   counter,
		idem:      newIdempotencyCache(0, 0),
		predebits: store.NewPredebitCommits(),
		bus:       eventbus.NoopBus{},
		logger:    logger,
	}
}

// NewWithAudit 完整构造。任一参数 nil 都退到 noop（不阻塞主路径）。
func NewWithAudit(
	eng *engine.Engine,
	counter store.Counter,
	links store.LinkStore,
	ipIntel ipintel.Service,
	mlSvc mlscore.Service,
	sessions session.Store,
	reviewQ review.Store,
	fbRec feedback.Recorder,
	sink audit.Sink,
	logger *zap.Logger,
) *RiskService {
	return &RiskService{
		engine:    eng,
		counter:   counter,
		links:     links,
		ipIntel:   ipIntel,
		mlSvc:     mlSvc,
		sessions:  sessions,
		reviewQ:   reviewQ,
		fbRec:     fbRec,
		auditSink: sink,
		idem:      newIdempotencyCache(0, 0),
		predebits: store.NewPredebitCommits(),
		bus:       eventbus.NoopBus{},
		logger:    logger,
	}
}

// RecordOutcome 业务侧（dispute 系统、商户后台）写一条 Outcome 反馈。
// 没有 fbRec 时 no-op。decision_id 是当时 Screen 返回的 audit id。
func (s *RiskService) RecordOutcome(o feedback.Outcome) error {
	if s.fbRec == nil {
		return nil
	}
	return s.fbRec.Record(o)
}

// Screen 交易前风控判定。命中后写一条 DecisionAudit（独立 audit sink，
// 不阻塞主路径）。
//
// IP 富化：如果 IPIntel 服务可用且 txn.IPAddress 非空，先 Lookup() 把结果
// 填到 txn.IPCountry / IPProxy / IPVPN / IPDataCenter / IPASN，引擎规则
// （如 ip_risk）就能直接读这些 enriched 字段。
func (s *RiskService) Screen(ctx context.Context, txn *engine.TxnContext) *engine.Result {
	// 沙箱短路：测试 API key + 触发字段命中预定义场景 → 直接造一个 Result，
	// 不走 evaluate / audit / review queue（沙箱场景不污染生产数据）。
	// 生产 key 永远不触发（sandbox.Detect 内部 IsTest 检查）。
	principal, _ := auth.PrincipalFrom(ctx)
	if sb := sandbox.Detect(principal, txn); sb != nil {
		decisionID := newDecisionID(ctx)
		res := &engine.Result{
			Decision:          sb.Decision,
			RiskScore:         sb.RiskScore,
			RiskLevel:         sb.RiskLevel,
			DecisionID:        decisionID,
			RecommendedAction: sb.RecommendedAction,
			Hits: []engine.Hit{{
				RuleID:   "sandbox",
				RuleName: "sandbox simulator",
				Decision: sb.Decision,
				Detail:   sb.Reason,
				Force:    true,
			}},
		}
		s.logger.Info("risk screen SANDBOX",
			zap.String("decision_id", decisionID),
			zap.String("decision", sb.Decision.String()),
			zap.String("reason", sb.Reason))
		return res
	}
	// 幂等短路：业务侧传 IdempotencyKey 时，60s 内同 key 的二次调用直接返回
	// 上一次结果，不再 evaluate / audit / push review。避免重试污染下游。
	if txn.IdempotencyKey != "" {
		if cached := s.idem.Get(txn.IdempotencyKey); cached != nil {
			s.logger.Info("risk screen idempotent hit",
				zap.String("key", txn.IdempotencyKey),
				zap.String("decision", cached.Decision.String()))
			return cached
		}
	}
	// 端 SDK 富化：业务侧传 RiskSessionID → 查 SessionStore 把 fingerprint +
	// behavior 字段填到 txn。txn 上已有的非零值优先（caller 强行覆盖能力）。
	if s.sessions != nil && txn.RiskSessionID != "" {
		if snap := s.sessions.Get(txn.RiskSessionID); snap != nil {
			fillSessionFields(txn, snap)
		}
	}
	// 资损修复：把 ReservationTracker 注入 ctx，让 amount_limit / velocity /
	// velocity_amount 规则走原子预扣路径（IncrIfBelow*）—— 之前 PR #34 添加的
	// 原子路径需要 ctx 里有 tracker 才会生效，但 service.Screen 从未注入，导致
	// 整套原子机制是 dead code，仍是 race-prone 的 Get+比较。
	//
	// 决策为 Deny/Review 时通过 defer 调 CancelAll 把所有预扣回滚 —— 否则被拒
	// 请求会把限额槽位锁住直到 TTL 才释放（"软 DDoS"：攻击者反复触发 deny 把
	// 正常用户限额耗光）。Allow 路径不 Cancel，保留预扣作为本笔的最终消费。
	//
	// 注意（已知 follow-up）：Allow 路径下 Report() 的 s.counter.Incr 会再加
	// 一次同 key，导致双计。当前 production 用 RedisCounter 不实现 AtomicCounter →
	// 预扣不触发，无双计；MemCounter (dev/test) 会双计但不影响真实资金。彻底
	// 修复需要 Report 按 IdempotencyKey 跳过已预扣 key —— 单独 PR 跟进。
	tracker := store.NewReservationTracker()
	ctx = store.WithReservationTracker(ctx, tracker)

	// ── per-stage timeout budget ───────────────────────────────────────
	// Screen 整体上限 global_ceiling；下面每个 stage 再单独 WithTimeout 子 ctx。
	// 任一 stage 超时 → fail-open（用空结果继续），保证不会拖死主路径。
	// ScreenTimeoutsDisabled env 紧急回退到旧行为（不包 timeout）。
	timeoutsCfg := s.activeTimeouts()
	disableTO := screenTimeoutsDisabled()
	screenStart := time.Now()
	defer func() {
		metrics.ScreenTotalDuration.WithLabelValues().Observe(time.Since(screenStart).Seconds())
	}()
	parentCtx := ctx
	if !disableTO && timeoutsCfg.GlobalCeiling > 0 {
		var cancel context.CancelFunc
		parentCtx, cancel = context.WithTimeout(ctx, timeoutsCfg.GlobalCeiling)
		defer cancel()
	}

	// FeatureExtractor 链：在 IP/ML 富化和 evaluate 之前再补充时间 / 卡 /
	// 客户历史 / 货币 等 typed 特征。每个 extractor fail-open。
	stageFE := time.Now()
	if len(s.extractors) > 0 {
		if disableTO {
			s.extractors.Enrich(parentCtx, txn)
		} else if to := stageWithTimeout(parentCtx, timeoutsCfg.FeatureExtract, func(c context.Context) {
			s.extractors.Enrich(c, txn)
		}); to {
			metrics.StageTimeout.WithLabelValues("feature_extract").Inc()
			metrics.StageFailOpen.WithLabelValues("feature_extract", "timeout").Inc()
			s.logger.Warn("feature_extract stage timeout (fail-open)",
				zap.Duration("budget", timeoutsCfg.FeatureExtract))
			// fail-open：保持 txn 当前富化状态，下游用现有字段继续
		}
	}
	feMs := time.Since(stageFE).Seconds()
	metrics.ScreenStageDuration.WithLabelValues("feature_extract").Observe(feMs)
	stageIP := time.Now()
	if s.ipIntel != nil && txn.IPAddress != "" && txn.IPCountry == "" {
		// 入参 IPCountry 非空表示 caller 已自带富化结果（比如 api-gateway 已查过）
		// → 不重复查询。熔断器 Open 时直接跳过 Lookup，让 ip_risk 规则
		// 看到空值（fail-open 等价 ALLOW，不阻塞 Screen 主路径）。
		if s.ipBreaker == nil || s.ipBreaker.Allow() {
			// per-stage budget：把 Lookup 包到 stageWithTimeout；超时 → 空 Result
			// + fail-open metric。Lookup 本身签名不返 err（实现内部已 fail-open），
			// 这里的 timeout 兜底是"实现卡住"的极端情况。
			// 用 buffered chan 传结果避免 timeout 后 goroutine 写 stack var 的 race。
			var r ipintel.Result
			ipTimedOut := false
			if disableTO {
				r = s.ipIntel.Lookup(parentCtx, txn.IPAddress)
			} else {
				ipCh := make(chan ipintel.Result, 1)
				if to := stageWithTimeout(parentCtx, timeoutsCfg.IPIntel, func(c context.Context) {
					rr := s.ipIntel.Lookup(c, txn.IPAddress)
					select {
					case ipCh <- rr:
					default:
					}
				}); to {
					ipTimedOut = true
					metrics.StageTimeout.WithLabelValues("ip_intel").Inc()
					metrics.StageFailOpen.WithLabelValues("ip_intel", "timeout").Inc()
					s.logger.Warn("ip_intel stage timeout (fail-open)",
						zap.String("ip", safety.MaskIPv4(txn.IPAddress)),
						zap.Duration("budget", timeoutsCfg.IPIntel))
					r = ipintel.Result{} // 显式空：保留旧的 fail-open 语义
				} else {
					select {
					case r = <-ipCh:
					default:
					}
				}
			}
			// 熔断器反馈：timeout 算一次失败；正常返但 Result 完全空也算失败
			// （ipintel 内部已 fail-open 的信号）；其余算成功。
			// 注意：timeout 和 "空结果" 互斥（timeout 一定是空），不会重复计数。
			if s.ipBreaker != nil {
				if ipTimedOut || (r.Country == "" && !r.Proxy && !r.VPN && !r.DataCenter) {
					s.ipBreaker.OnFailure()
				} else {
					s.ipBreaker.OnSuccess()
				}
			}
			txn.IPCountry = r.Country
			txn.IPProxy = r.Proxy
			txn.IPVPN = r.VPN
			txn.IPDataCenter = r.DataCenter
			txn.IPASN = r.ASN
		} else {
			s.logger.Warn("ipintel breaker open; skipping lookup",
				zap.String("ip", safety.MaskIPv4(txn.IPAddress)))
			metrics.StageFailOpen.WithLabelValues("ip_intel", "breaker").Inc()
		}
	}
	ipMs := time.Since(stageIP).Seconds()
	metrics.ScreenStageDuration.WithLabelValues("ip_intel").Observe(ipMs)
	// 提前生成 decision_id，让 ML 推理路径能把 id 透传到 ChampionChallengerService
	// 的 SideEffect → ABTracker 收三元组 (decision_id, champion_score,
	// challenger_score)，给后续 A/B 显著性检验用。
	decisionID := newDecisionID(ctx)

	// 运营手动 ML 降级开关：disabled=true 时整个 ML 路径跳过；ForceScore
	// 非 0 时即使 ML 跑成功也用强制值覆盖。给运营紧急关 ML 不重启用。
	mlOverride := mloverride.Override{}
	if s.mlOverride != nil {
		mlOverride = s.mlOverride.Get()
	}
	// ML 推理：先收集特征 → Score 异步调；失败 fail-open（MLScore 保持 0，
	// ml_threshold 规则按 fail_open 配置自行决定）。熔断器 Open → 跳过推理。
	// 运营 override.Disabled=true 时整段跳过。
	stageML := time.Now()
	if !mlOverride.Disabled && s.mlSvc != nil && (s.mlBreaker == nil || s.mlBreaker.Allow()) {
		feats := mlFeaturesFrom(txn)
		var mlRes mlscore.Result
		var err error
		// 用 buffered chan 传 (mlRes, err) 避免 timeout 后 goroutine 继续写
		// stack var 的 race。
		type mlOut struct {
			r mlscore.Result
			e error
		}
		mlCh := make(chan mlOut, 1)
		// ChampionChallengerService 走 decision_id 透传路径；其它实现退到
		// 普通 Score（不影响主路径）。
		scoreFn := func(c context.Context) {
			var out mlOut
			if cc, ok := s.mlSvc.(decisionScorer); ok {
				out.r, out.e = cc.ScoreWithDecisionID(c, decisionID, feats)
			} else {
				out.r, out.e = s.mlSvc.Score(c, feats)
			}
			select {
			case mlCh <- out:
			default:
			}
		}
		timedOut := false
		if disableTO {
			scoreFn(parentCtx)
		} else {
			timedOut = stageWithTimeout(parentCtx, timeoutsCfg.MLScore, scoreFn)
		}
		if !timedOut {
			select {
			case out := <-mlCh:
				mlRes, err = out.r, out.e
			default:
				// fn 正常返回但没写 chan：理论不可能
			}
		}
		switch {
		case timedOut:
			metrics.StageTimeout.WithLabelValues("ml_score").Inc()
			metrics.StageFailOpen.WithLabelValues("ml_score", "timeout").Inc()
			if s.mlBreaker != nil {
				s.mlBreaker.OnFailure()
			}
			s.logger.Warn("ml_score stage timeout (fail-open)",
				zap.Duration("budget", timeoutsCfg.MLScore))
			// fail-open：MLScore 保留 0，ml_threshold 规则按 fail_open 配置自定
		case err != nil:
			metrics.StageFailOpen.WithLabelValues("ml_score", "error").Inc()
			if s.mlBreaker != nil {
				s.mlBreaker.OnFailure()
			}
			s.logger.Warn("ml score failed (fail-open)", zap.Error(err))
		default:
			if s.mlBreaker != nil {
				s.mlBreaker.OnSuccess()
			}
			txn.MLScore = mlRes.Score
			txn.MLModelVer = mlRes.ModelVer
			if s.mlDrift != nil {
				s.mlDrift.Observe(mlRes.Score)
				// 把入参数值型特征也喂进 drift monitor，做 per-feature PSI / KS。
				// 字符串 / bool 特征 hash 化无意义于 PSI，跳过；amount / 行为时序
				// 类数值特征是漂移最早暴露的地方。
				s.mlDrift.ObserveFeatures(driftFeaturesFrom(feats))
			}
		}
	} else if s.mlSvc != nil && s.mlBreaker != nil && !mlOverride.Disabled && !s.mlBreaker.Allow() {
		// 显式记录熔断器短路的 fail-open（与 ml 服务 nil / override.Disabled 区分）
		metrics.StageFailOpen.WithLabelValues("ml_score", "breaker").Inc()
	}
	// override.ForceScore 优先级最高：在 ML 路径之后强制覆盖（debug /
	// 故障演练用）。ModelVer 加 "+override" 让 audit 一眼看出来。
	if mlOverride.ForceScore != 0 {
		txn.MLScore = mlOverride.ForceScore
		if txn.MLModelVer == "" {
			txn.MLModelVer = "override"
		} else {
			txn.MLModelVer = txn.MLModelVer + "+override"
		}
	} else if mlOverride.Disabled {
		// 跳过整个 ML 推理；ModelVer 标记让审计可看
		txn.MLModelVer = "disabled-by-operator"
	}
	mlMs := time.Since(stageML).Seconds()
	metrics.ScreenStageDuration.WithLabelValues("ml_score").Observe(mlMs)

	start := time.Now()
	// 用 channel 安全传 result 而非共享变量赋值 — engine.Evaluate 不检查 ctx，
	// timeout 后 goroutine 仍可能写入；用 channel 让 main path 跳出后 goroutine
	// 安全地把结果丢弃（写到 buffered chan 不阻塞）。
	resCh := make(chan *engine.Result, 1)
	evalFn := func(c context.Context) {
		r := s.engine.Evaluate(c, txn)
		select {
		case resCh <- r:
		default:
			// 已超时返回了；丢弃
		}
	}
	var res *engine.Result
	if disableTO {
		evalFn(parentCtx)
		res = <-resCh
	} else if to := stageWithTimeout(parentCtx, timeoutsCfg.EngineEval, evalFn); to {
		metrics.StageTimeout.WithLabelValues("engine_eval").Inc()
		metrics.StageFailOpen.WithLabelValues("engine_eval", "timeout").Inc()
		s.logger.Warn("engine_eval stage timeout (fail-open)",
			zap.Duration("budget", timeoutsCfg.EngineEval))
		// engine 超时是最严重的 — 规则没跑就 Allow 等于裸奔。但 fail-policy
		// 仍然是 fail-open（与文档一致）：返一个最小 Allow Result，让后续
		// audit / event 仍正常落，便于 SRE 定位"engine 卡了多少笔"。
		res = &engine.Result{
			Decision:  engine.Allow,
			RiskScore: 0,
			RiskLevel: "unknown",
		}
	} else {
		// 正常路径：stageWithTimeout 已等 goroutine 跑完
		select {
		case res = <-resCh:
		default:
			// 极不可能：fn 正常返回但没往 chan 写。兜底。
			res = &engine.Result{Decision: engine.Allow, RiskScore: 0, RiskLevel: "unknown"}
		}
	}
	evalSec := time.Since(start).Seconds()
	evalMs := evalSec * 1000
	metrics.ScreenStageDuration.WithLabelValues("engine_eval").Observe(evalSec)

	// global_ceiling 检查：parentCtx 已 Done 表示整个 Screen 已超总上限。
	// 仍然 fail-open（不阻断当前 caller），但打 ceiling metric 让 SRE 知道。
	if parentCtx.Err() == context.DeadlineExceeded && timeoutsCfg.GlobalCeiling > 0 {
		metrics.ScreenCeilingTotal.Inc()
		s.logger.Warn("screen global ceiling exceeded (fail-open)",
			zap.Duration("ceiling", timeoutsCfg.GlobalCeiling))
	}

	s.logger.Info("risk screen",
		zap.String("decision_id", decisionID),
		zap.String("pi_id", txn.PaymentIntentID),
		zap.String("merchant", txn.MerchantID),
		// PII：customer_id 通常已经是 hash/UUID，但万一是邮箱/手机原文，
		// safety.MaskGeneric 会留首尾 + 星号填充。日志写出去前先过一道。
		zap.String("customer", safety.MaskGeneric(txn.CustomerID)),
		zap.Int64("amount", txn.Amount),
		zap.String("method", txn.PaymentMethod),
		zap.String("decision", res.Decision.String()),
		zap.Int("score", res.RiskScore),
		zap.String("level", res.RiskLevel),
		zap.Int("hits", len(res.Hits)),
		zap.Float64("eval_ms", evalMs),
	)
	res.DecisionID = decisionID
	res.RecommendedAction = recommendedActionFor(res.Decision)
	stageAudit := time.Now()
	// audit_write 路径：生产已通过 AsyncBatchSink 异步化（main.go 包了一层
	// queue+worker，Sink.Write 是非阻塞 channel send）。但为了防御一些
	// 自定义 Sink 实现（直接 ClickHouse / S3 写）阻塞主路径，这里再加一层
	// 保险：AuditWrite ≤ 0 → fire-and-forget goroutine；> 0 → 同步 + timeout。
	// 注：异步路径下 auditMs 只是"调度 goroutine 的耗时"，几乎为 0 — 这就是
	// 设计意图，让 audit_write 不再算入 Screen SLA。
	if disableTO || timeoutsCfg.AuditWrite > 0 {
		if !disableTO && timeoutsCfg.AuditWrite > 0 {
			if to := stageWithTimeout(parentCtx, timeoutsCfg.AuditWrite, func(c context.Context) {
				s.recordAuditWith(c, decisionID, txn, res, evalMs)
			}); to {
				metrics.StageTimeout.WithLabelValues("audit_write").Inc()
				metrics.StageFailOpen.WithLabelValues("audit_write", "timeout").Inc()
				s.logger.Warn("audit_write stage timeout (dropped)",
					zap.Duration("budget", timeoutsCfg.AuditWrite))
			}
		} else {
			s.recordAuditWith(parentCtx, decisionID, txn, res, evalMs)
		}
	} else {
		// async：detach 到 background ctx（parentCtx 已要 cancel；如果 sink
		// 是同步实现且较慢，不阻塞 caller）。
		// 假设：现有 AsyncBatchSink 已经做了 backpressure / drop 统计；这里
		// goroutine 只是再加一层保险（不重新造 sink）。
		// TODO(SLA): 当所有 audit Sink 都迁移到 AsyncBatchSink 后可去掉
		// 这层 goroutine，直接调 recordAuditWith。
		go func() {
			defer func() {
				// 防御 audit sink panic 不冒泡到 main goroutine
				_ = recover()
			}()
			bgCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			s.recordAuditWith(bgCtx, decisionID, txn, res, evalMs)
		}()
	}
	auditMs := time.Since(stageAudit).Seconds()
	metrics.ScreenStageDuration.WithLabelValues("audit_write").Observe(auditMs)

	// Slow log：总耗时超 100ms 时记一条 + 标最慢 stage，让 SRE 一眼看出
	// p99 突变是哪个 stage 拖的（feature_extract/ip_intel/ml_score/
	// engine_eval/audit_write）。阈值走 slow_threshold_ms (默认 100ms)。
	totalSec := feMs + ipMs + mlMs + evalSec + auditMs
	if s.slowThreshold > 0 && totalSec >= s.slowThreshold {
		slowest := stageNameOfMax(feMs, ipMs, mlMs, evalSec, auditMs)
		metrics.ScreenSlowTotal.WithLabelValues(slowest).Inc()
		s.logger.Warn("screen slow",
			zap.String("decision_id", decisionID),
			zap.Float64("total_sec", totalSec),
			zap.String("slowest_stage", slowest),
			zap.Float64("feature_extract_sec", feMs),
			zap.Float64("ip_intel_sec", ipMs),
			zap.Float64("ml_score_sec", mlMs),
			zap.Float64("engine_eval_sec", evalSec),
			zap.Float64("audit_write_sec", auditMs))
	}
	s.maybeQueueReview(decisionID, txn, res)
	s.publishWebhook(ctx, txn, res)
	// 商业 SLO 指标：按商户拆分 verdict + score 分布。merchant_id 空时打 "_unknown"
	// 避免 cardinality 爆炸只看全局。
	mid := txn.MerchantID
	if mid == "" {
		mid = "_unknown"
	}
	metrics.VerdictTotal.WithLabelValues(mid, res.Decision.String()).Inc()
	metrics.RiskScore.WithLabelValues(mid).Observe(float64(res.RiskScore))
	// 幂等键缓存：写入是 evaluate 完之后，确保 cache 里只放真正落地的 result。
	if txn.IdempotencyKey != "" {
		s.idem.Put(txn.IdempotencyKey, res)
	}
	// 异步发事件总线：TopicScreen + 每条命中 TopicHit。fire-and-forget 不阻塞。
	s.publishScreenAsync(txn, res)
	// 异步落 ML 特征快照（给 retrain / champion-challenger offline eval 用）。
	s.saveFeatureSnapshotAsync(decisionID, txn, res)
	// 资损修复：Deny / Review 时回滚所有预扣，避免被拒请求把限额槽位锁到 TTL
	// 才释放（"软 DDoS"）。Allow 保留预扣作为最终消费。
	if res.Decision == engine.Deny || res.Decision == engine.Review {
		tracker.CancelAll(ctx, s.counter)
		if !tracker.Empty() {
			// CancelAll 之后 tracker.items 已被清空；这条只在 nil-tracker 边角才进。
			s.logger.Warn("reservation tracker not empty after CancelAll",
				zap.String("decision_id", decisionID))
		}
	} else if txn.IdempotencyKey != "" {
		// Allow 路径：把 tracker 里所有预扣的 counter key 索引到 IdempotencyKey
		// 下；Report 阶段 Incr 同 key 时会跳过避免双计。预扣未发生（regular
		// path / 非 AtomicCounter）→ tracker.Items() 空 → Commit no-op。
		s.predebits.Commit(txn.IdempotencyKey, tracker.Items())
	}
	return res
}

// saveFeatureSnapshotAsync 异步把 (features, decision) 落到 featurestore。
// nil-safe；失败不打扰主路径。
func (s *RiskService) saveFeatureSnapshotAsync(decisionID string, txn *engine.TxnContext, res *engine.Result) {
	if s.features == nil || txn == nil || res == nil {
		return
	}
	feats := mlFeaturesFrom(txn) // 复用现有 feature extractor
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		s.features.Save(ctx, featurestore.Snapshot{
			DecisionID: decisionID,
			OccurAt:    time.Now().UTC(),
			Features:   &feats,
			Verdict:    res.Decision.String(),
			RiskScore:  res.RiskScore,
			MLScore:    txn.MLScore,
			MLModelVer: txn.MLModelVer,
		})
	}()
}

// publishScreenAsync 异步发布 Screen 决策事件 + 命中规则事件。
// 失败只 log（bus 是 nil-safe，noop 实现不会失败）；不影响主路径 SLA。
func (s *RiskService) publishScreenAsync(txn *engine.TxnContext, res *engine.Result) {
	if s.bus == nil || txn == nil || res == nil {
		return
	}
	// 拷贝一份关键字段；不对原 txn / res 取地址，避免并发写
	mid := txn.MerchantID
	cust := txn.CustomerID
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		body, _ := encodeScreenEvent(txn, res)
		_ = s.bus.Publish(ctx, eventbus.Event{
			Topic: eventbus.TopicScreen,
			Key:   mid + "/" + cust,
			Data:  body,
		})
		for _, h := range res.Hits {
			hb, _ := encodeHitEvent(txn, &h)
			_ = s.bus.Publish(ctx, eventbus.Event{
				Topic: eventbus.TopicHit,
				Key:   mid + "/" + h.RuleID,
				Data:  hb,
			})
		}
	}()
}

// maybeQueueReview verdict==Review 时把 decision_id + 关键字段 push 到
// 人工 review 队列。pending 在 admin UI 中可见，运营通过 /admin/review/decide
// 决议；决议结果通过 OutcomeRecorder 回写到 audit（feedback 闭环）。
func (s *RiskService) maybeQueueReview(decisionID string, txn *engine.TxnContext, res *engine.Result) {
	if s.reviewQ == nil || res.Decision != engine.Review {
		return
	}
	reasons := make([]string, 0, len(res.Hits))
	for _, h := range res.Hits {
		reasons = append(reasons, h.RuleID+": "+h.Detail)
	}
	if err := s.reviewQ.Push(review.Item{
		ID:              decisionID,
		MerchantID:      txn.MerchantID,
		CustomerID:      txn.CustomerID,
		PaymentIntentID: txn.PaymentIntentID,
		Amount:          txn.Amount,
		Currency:        txn.Currency,
		RiskScore:       res.RiskScore,
		Reasons:         reasons,
	}); err != nil {
		s.logger.Warn("review queue push failed", zap.Error(err))
	}
}

// recordAuditWith 用调用方传入的 decisionID 写审计，让同一 id 在 audit /
// review queue / OutcomeRecorder 三处保持一致（feedback 闭环用）。
func (s *RiskService) recordAuditWith(ctx context.Context, decisionID string, txn *engine.TxnContext, res *engine.Result, evalMs float64) {
	if s.auditSink == nil {
		return
	}
	s.auditSink.Write(ctx, &audit.DecisionAudit{
		DecisionID:  decisionID,
		OccurredAt:  time.Now().UTC(),
		// RuleVersion / RuleSetHash 现在用 engine.RuleSetHash() — 排序后的
		// (rule_id, active_version) 集合的 fnv32a；同规则集任何变动都能
		// 在 audit 里精确反查到当时的 engine 状态。
		RuleVersion: int(s.engine.RuleSetHash()),
		RuleSetHash: s.engine.RuleSetHashHex(),
		Input: audit.AuditInput{
			PaymentIntentID: txn.PaymentIntentID,
			MerchantID:      txn.MerchantID,
			CustomerID:      txn.CustomerID,
			Amount:          txn.Amount,
			Currency:        txn.Currency,
			PaymentMethod:   txn.PaymentMethod,
			Country:         txn.Country,
			IPAddress:       txn.IPAddress,
			DeviceID:        txn.DeviceID,
			Metadata:        txn.Metadata,
		},
		Verdict:        res.Decision.String(),
		RiskScore:      res.RiskScore,
		RiskLevel:      res.RiskLevel,
		Hits:           convertHits(res.Hits),
		ShadowHits:     convertHits(res.ShadowHits),
		MLScore:        txn.MLScore,
		MLModelVer:     txn.MLModelVer,
		EvalDurationMs: evalMs,
	})
}

// convertHits 把 engine.Hit 转成 audit.AuditHit。
func convertHits(in []engine.Hit) []audit.AuditHit {
	if len(in) == 0 {
		return nil
	}
	out := make([]audit.AuditHit, 0, len(in))
	for _, h := range in {
		out = append(out, audit.AuditHit{
			RuleID:   h.RuleID,
			RuleName: h.RuleName,
			Decision: h.Decision.String(),
			Detail:   h.Detail,
		})
	}
	return out
}

// publishWebhook verdict=REVIEW / DENY 时推送商户配置的 webhook URL。
// ALLOW 不推（每次支付都推会把商户后台塞爆）；商户要全量决策应订阅 audit
// log 而不是 webhook。
func (s *RiskService) publishWebhook(ctx context.Context, txn *engine.TxnContext, res *engine.Result) {
	if s.wh == nil || txn.MerchantID == "" {
		return
	}
	var et webhook.EventType
	switch res.Decision {
	case engine.Review:
		et = webhook.EventReviewCreated
	case engine.Deny:
		et = webhook.EventDecisionDenied
	default:
		return
	}
	hits := make([]string, 0, len(res.Hits))
	for _, h := range res.Hits {
		hits = append(hits, h.RuleID+": "+h.Detail)
	}
	s.wh.Publish(ctx, txn.MerchantID, &webhook.Event{
		Type:       et,
		MerchantID: txn.MerchantID,
		Data: map[string]any{
			"decision_id":        res.DecisionID,
			"verdict":            res.Decision.String(),
			"risk_score":         res.RiskScore,
			"risk_level":         res.RiskLevel,
			"recommended_action": res.RecommendedAction,
			"payment_intent_id":  txn.PaymentIntentID,
			"customer_id":        txn.CustomerID,
			"amount":             txn.Amount,
			"currency":           txn.Currency,
			"hits":               hits,
		},
	})
}

// PublishReviewDecided 给 admin handler 调：review queue 决议后推送商户。
func (s *RiskService) PublishReviewDecided(ctx context.Context, item *review.Item) {
	if s.wh == nil || item == nil {
		return
	}
	s.wh.Publish(ctx, item.MerchantID, &webhook.Event{
		Type:       webhook.EventReviewDecided,
		MerchantID: item.MerchantID,
		Data: map[string]any{
			"decision_id":       item.ID,
			"status":            string(item.Status),
			"decided_by":        item.DecidedBy,
			"decide_reason":     item.DecideReason,
			"payment_intent_id": item.PaymentIntentID,
			"customer_id":       item.CustomerID,
			"amount":            item.Amount,
			"currency":          item.Currency,
		},
	})
}

// recommendedActionFor 把 verdict 翻译成 payment-core 能动作的语义：
//   - DENY → block        直接拒，不路由到渠道
//   - REVIEW → step_up_3ds 推荐发起 3DS step-up（有能力时）；否则 hold_for_review
//   - ALLOW → ""           不做特殊动作，按原流程
//
// payment-core 端最终选哪个动作还依赖商户配置（是否开 3DS、是否接收 review hold），
// 这里只给"风控建议"。
func recommendedActionFor(d engine.Decision) string {
	switch d {
	case engine.Deny:
		return "block"
	case engine.Review:
		return "step_up_3ds"
	}
	return ""
}

// newDecisionID 按位编码生成 risk decision ID（idType=400）。
// seq 用 8-byte crypto-rand → uint64 → mod 1e13（fit 13-digit seq slot）。
// 跨实例去重靠 crypto-rand 的高熵；shadow flag 由 ctx 决定。
// crypto/rand 失败兜底用 ns 时间戳（极罕见）。
func newDecisionID(ctx context.Context) string {
	var b [8]byte
	var seq int64
	if _, err := rand.Read(b[:]); err == nil {
		seq = int64(binary.BigEndian.Uint64(b[:])&0x7FFFFFFFFFFFFFFF) % 9_999_999_999_999
	} else {
		seq = time.Now().UTC().UnixNano() % 9_999_999_999_999
	}
	if seq <= 0 {
		seq = 1
	}
	id, err := shadow.EncodeIDStr(ctx, shadow.IDTypeRiskDecision, 0, seq)
	if err != nil {
		// 极不可能失败（layout 约束都满足），兜底 nano timestamp 字符串
		return time.Now().UTC().Format("20060102150405.000000000")
	}
	return id
}

// Report 交易后上报。
//
// 两类副作用：
//  1. **限额计数器**（store.Counter）：成功交易累计金额，velocity 规则用
//  2. **图谱关联**（store.LinkStore）：成功交易把 (device, ip, customer)
//     三对边写入 link store，让 link_fanout 规则下次评估时能看到该图谱
//     变化。失败交易**不**写图谱（避免攻击者用失败 txn 污染图谱）。
func (s *RiskService) Report(ctx context.Context, txn *engine.TxnContext, eventType string) {
	cust := nonEmpty("customer:", txn.CustomerID)
	dev := nonEmpty("device:", txn.DeviceID)
	ip := nonEmpty("ip:", txn.IPAddress)
	mer := nonEmpty("merchant:", txn.MerchantID)
	fp := ""
	if txn.FingerprintHash != "" {
		fp = "fp:" + txn.FingerprintHash
	}
	uaH := ""
	if txn.UAHash != "" {
		uaH = "ua:" + txn.UAHash
	}
	emH := ""
	if txn.EmailHash != "" {
		emH = "email:" + txn.EmailHash
	}
	phH := ""
	if txn.PhoneHash != "" {
		phH = "phone:" + txn.PhoneHash
	}

	// ── 1) 支付成功类：累计金额 + 写完整图谱（含 card/email pivot）
	if engine.IsPaymentSuccessEvent(eventType) {
		keys := []string{cust, ip, dev, mer}
		for _, k := range keys {
			if k == "" {
				continue
			}
			// 资损修复：Screen Allow 阶段已经通过 IncrIfBelow* 把同 (idemKey,k)
			// counter 加过 → 这里再 Incr 就是双计。WasReserved 命中即跳。
			// idemKey 空 / 非 AtomicCounter / 未跨 Screen-Report 链路 → 命中 false → 走原 Incr。
			if s.predebits != nil && s.predebits.WasReserved(txn.IdempotencyKey, k) {
				continue
			}
			s.counter.Incr(ctx, k, txn.Amount)
		}
		if s.links != nil {
			s.links.Link(ctx, dev, cust)
			s.links.Link(ctx, ip, cust)
			s.links.Link(ctx, dev, ip)
			s.links.Link(ctx, dev, mer)
			s.links.Link(ctx, ip, mer)
			s.links.Link(ctx, cust, mer)
			if cardFP := txn.Metadata["card_fingerprint"]; cardFP != "" {
				card := "card:" + cardFP
				s.links.Link(ctx, card, cust)
				s.links.Link(ctx, card, dev)
				s.links.Link(ctx, card, ip)
				s.links.Link(ctx, card, mer)
			}
			if emH != "" {
				s.links.Link(ctx, emH, dev)
				s.links.Link(ctx, emH, ip)
				s.links.Link(ctx, emH, cust)
			}
			if phH != "" {
				s.links.Link(ctx, phH, dev)
				s.links.Link(ctx, phH, ip)
				s.links.Link(ctx, phH, cust)
			}
		}
	}

	// ── 2) 账户事件类（register / login / password_change / bind_* / kyc.*）：
	//     写 device/ip/fp/ua/email/phone → customer 全连边。让批量注册 /
	//     fingerprint_multi_account / ua_batch_register / register_velocity 看到。
	if engine.IsAccountEvent(eventType) && s.links != nil {
		s.links.Link(ctx, dev, cust)
		s.links.Link(ctx, ip, cust)
		s.links.Link(ctx, dev, ip)
		if fp != "" {
			s.links.Link(ctx, fp, cust)
		}
		if uaH != "" {
			s.links.Link(ctx, uaH, cust)
		}
		if emH != "" {
			s.links.Link(ctx, emH, cust)
		}
		if phH != "" {
			s.links.Link(ctx, phH, cust)
		}
	}

	// ── 3) 失败账户事件（register.failed / login.failed）：只增计数器，不写图边
	//     避免攻击者用别人的 email 触发 failed register / login 把图谱污染。
	if engine.IsAccountFailedEvent(eventType) {
		if ip != "" {
			s.counter.Incr(ctx, "fail:"+ip, 1)
		}
		if dev != "" {
			s.counter.Incr(ctx, "fail:"+dev, 1)
		}
		if cust != "" {
			s.counter.Incr(ctx, "fail:"+cust, 1)
		}
	}

	// ── 4) Fraud 标签传播：fraud / chargeback / dispute.lost
	if engine.IsFraudSignalEvent(eventType) && s.links != nil {
		tag := engine.FraudTagFor(eventType)
		if cust != "" {
			s.links.Tag(ctx, cust, tag)
		}
		if dev != "" {
			s.links.Tag(ctx, dev, tag)
		}
		if ip != "" {
			s.links.Tag(ctx, ip, tag)
		}
		if fp != "" {
			s.links.Tag(ctx, fp, tag)
		}
		if emH != "" {
			s.links.Tag(ctx, emH, tag)
		}
		if phH != "" {
			s.links.Tag(ctx, phH, tag)
		}
	}

	// ── 5) Withdraw 系列：累计提现金额 + 关联图（提现地址作为新 pivot）
	if eventType == engine.EventWithdrawSucceeded {
		if cust != "" {
			s.counter.Incr(ctx, "withdraw:"+cust, txn.Amount)
		}
		if s.links != nil {
			if addr := txn.Metadata["withdraw_address"]; addr != "" {
				s.links.Link(ctx, "wallet:"+addr, cust)
				s.links.Link(ctx, "wallet:"+addr, dev)
			}
		}
	}

	// ── 6) 邀请关系（拉新 / 羊毛党检测）：邀请人 → 新人
	if eventType == engine.EventReferralApply && s.links != nil {
		if inviter := txn.Metadata["referrer_customer_id"]; inviter != "" && cust != "" {
			s.links.Link(ctx, "ref:"+inviter, cust)
		}
	}

	s.logger.Info("risk report",
		zap.String("pi_id", txn.PaymentIntentID),
		zap.String("event", eventType),
		zap.Int64("amount", txn.Amount),
	)
	s.publishReportAsync(txn, eventType)
}

// publishReportAsync 异步发布 Report 事件。Report 跑了 counter.Incr / links.Link
// 等可能耗时的副作用，事件发布对延迟不敏感，单独 goroutine 即可。
func (s *RiskService) publishReportAsync(txn *engine.TxnContext, eventType string) {
	if s.bus == nil || txn == nil {
		return
	}
	mid := txn.MerchantID
	cust := txn.CustomerID
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		body, _ := encodeReportEvent(txn, eventType)
		_ = s.bus.Publish(ctx, eventbus.Event{
			Topic: eventbus.TopicReport,
			Key:   mid + "/" + cust,
			Data:  body,
		})
	}()
}

// nonEmpty 把 dim:value 拼起来；value 为空时返回 ""，让 LinkStore.Link 自然 no-op。
func nonEmpty(prefix, value string) string {
	if value == "" {
		return ""
	}
	return prefix + value
}

// fillSessionFields 把 SessionStore 拿到的 snapshot 填到 txn。已经填的非零值
// 不覆盖（caller 显式传 > sdk 上报）。
func fillSessionFields(txn *engine.TxnContext, s *session.Snapshot) {
	// fingerprint
	if txn.FingerprintHash == "" {
		txn.FingerprintHash = s.FingerprintHash
	}
	if txn.CanvasFingerprint == "" {
		txn.CanvasFingerprint = s.CanvasFingerprint
	}
	if txn.WebGLRenderer == "" {
		txn.WebGLRenderer = s.WebGLRenderer
	}
	if txn.AudioContextHash == "" {
		txn.AudioContextHash = s.AudioContextHash
	}
	if txn.ScreenWxH == "" {
		txn.ScreenWxH = s.ScreenWxH
	}
	if txn.Timezone == "" {
		txn.Timezone = s.Timezone
	}
	if txn.Language == "" {
		txn.Language = s.Language
	}
	if txn.HardwareConcurrency == 0 {
		txn.HardwareConcurrency = s.HardwareConcurrency
	}
	if txn.Platform == "" {
		txn.Platform = s.Platform
	}
	if txn.UserAgent == "" {
		txn.UserAgent = s.UserAgent
	}
	if txn.IPAddress == "" {
		txn.IPAddress = s.IPAddress
	}
	// behavior
	if txn.TimeToCheckoutMs == 0 {
		txn.TimeToCheckoutMs = s.TimeToCheckoutMs
	}
	if txn.MouseMovementEntropy == 0 {
		txn.MouseMovementEntropy = s.MouseMovementEntropy
	}
	if txn.ClickIntervalMs == 0 {
		txn.ClickIntervalMs = s.ClickIntervalMs
	}
	if txn.ScrollSpeedPxPerSec == 0 {
		txn.ScrollSpeedPxPerSec = s.ScrollSpeedPxPerSec
	}
	if txn.TypingRhythmCV == 0 {
		txn.TypingRhythmCV = s.TypingRhythmCV
	}
	if txn.KeystrokeCount == 0 {
		txn.KeystrokeCount = s.KeystrokeCount
	}
	if len(txn.PastedFields) == 0 {
		txn.PastedFields = s.PastedFields
	}
}

// mlFeaturesFrom 把 TxnContext 的关键字段拷到 ML Features。Extra 透传 metadata。
// 不让 mlscore 包反向依赖 engine.TxnContext，留出独立演进空间。
func mlFeaturesFrom(txn *engine.TxnContext) mlscore.Features {
	if txn == nil {
		return mlscore.Features{}
	}
	return mlscore.Features{
		MerchantID:           txn.MerchantID,
		CustomerID:           txn.CustomerID,
		Amount:               txn.Amount,
		Currency:             txn.Currency,
		PaymentMethod:        txn.PaymentMethod,
		Country:              txn.Country,
		IPCountry:            txn.IPCountry,
		IPProxy:              txn.IPProxy,
		IPVPN:                txn.IPVPN,
		IPDataCenter:         txn.IPDataCenter,
		FingerprintHash:      txn.FingerprintHash,
		CanvasFingerprint:    txn.CanvasFingerprint,
		WebGLRenderer:        txn.WebGLRenderer,
		HardwareConcurrency:  txn.HardwareConcurrency,
		TimeToCheckoutMs:     txn.TimeToCheckoutMs,
		MouseMovementEntropy: txn.MouseMovementEntropy,
		ClickIntervalMs:      txn.ClickIntervalMs,
		TypingRhythmCV:       txn.TypingRhythmCV,
		KeystrokeCount:       txn.KeystrokeCount,
		Extra:                txn.Metadata,
	}
}

// driftFeaturesFrom 抽出 mlscore.Features 里所有数值型字段（int / float / bool）
// 做 per-feature PSI / KS。key 名稳定（map key 字符串），改名会丢历史 baseline。
// bool → 0/1。字符串 / hash 类特征 PSI 没意义，故不收。
func driftFeaturesFrom(f mlscore.Features) map[string]float64 {
	b := func(v bool) float64 {
		if v {
			return 1
		}
		return 0
	}
	return map[string]float64{
		"amount":                 float64(f.Amount),
		"ip_proxy":               b(f.IPProxy),
		"ip_vpn":                 b(f.IPVPN),
		"ip_datacenter":          b(f.IPDataCenter),
		"hardware_concurrency":   float64(f.HardwareConcurrency),
		"time_to_checkout_ms":    float64(f.TimeToCheckoutMs),
		"mouse_movement_entropy": f.MouseMovementEntropy,
		"click_interval_ms":      float64(f.ClickIntervalMs),
		"typing_rhythm_cv":       f.TypingRhythmCV,
		"keystroke_count":        float64(f.KeystrokeCount),
	}
}

// Engine 暴露给 admin / reload 用
func (s *RiskService) Engine() *engine.Engine { return s.engine }

// AuditSink 暴露给 admin 端 /admin/audit 查询用（如果 sink 实现了 Recent）。
func (s *RiskService) AuditSink() audit.Sink { return s.auditSink }

// ─── GDPR right-to-erasure ───────────────────────────────────────────

// EraseInput GDPR 删除输入。至少给一个标识符。
type EraseInput struct {
	CustomerID  string
	DeviceID    string
	IPAddress   string
	Fingerprint string
	EmailHash   string
	PhoneHash   string
	Reason      string
	RequestedBy string
}

// EraseResult 各 store 实际清掉的条数。粗粒度，给运营 dashboard 用。
type EraseResult struct {
	LinkEdgesPurged int
	CountersPurged  int
	FeaturesPurged  int
	AuditsMasked    int // audit 不物理删（合规留 7 年），改 PII 字段脱敏
}

// ErasePersonalData 级联删 LinkStore + Counter + featurestore 中跟该用户关联
// 的所有数据。audit 行不删（合规要求保留），但 PII 字段会被脱敏（后续做）。
//
// 重复调幂等。任何输入字段为空 → 跳过该 key 的 purge。
func (s *RiskService) ErasePersonalData(ctx context.Context, in *EraseInput) (*EraseResult, error) {
	if in == nil {
		return &EraseResult{}, nil
	}
	if in.CustomerID == "" && in.DeviceID == "" && in.IPAddress == "" &&
		in.Fingerprint == "" && in.EmailHash == "" && in.PhoneHash == "" {
		return &EraseResult{}, errors.New("erase: at least one identifier required")
	}
	res := &EraseResult{}

	// LinkStore：清节点 + 反向边
	if s.links != nil {
		keys := []string{}
		if in.CustomerID != "" {
			keys = append(keys, "customer:"+in.CustomerID)
		}
		if in.DeviceID != "" {
			keys = append(keys, "device:"+in.DeviceID)
		}
		if in.IPAddress != "" {
			keys = append(keys, "ip:"+in.IPAddress)
		}
		if in.Fingerprint != "" {
			keys = append(keys, "fp:"+in.Fingerprint)
		}
		if in.EmailHash != "" {
			keys = append(keys, "email:"+in.EmailHash)
		}
		if in.PhoneHash != "" {
			keys = append(keys, "phone:"+in.PhoneHash)
		}
		for _, k := range keys {
			res.LinkEdgesPurged += s.links.Purge(ctx, k)
		}
	}

	// Counter：清同样的 key 集合的计数
	if s.counter != nil {
		keys := []string{}
		if in.CustomerID != "" {
			keys = append(keys, "customer:"+in.CustomerID)
		}
		if in.DeviceID != "" {
			keys = append(keys, "device:"+in.DeviceID)
		}
		if in.IPAddress != "" {
			keys = append(keys, "ip:"+in.IPAddress)
		}
		// merchant counter 不删（多用户共享，删了影响其他用户）
		// fail:* counter 删（fraud signal 跟着用户走）
		for _, k := range keys {
			res.CountersPurged += s.counter.Purge(ctx, k)
			res.CountersPurged += s.counter.Purge(ctx, "fail:"+k)
		}
	}

	// FeatureStore：按 customer_id 删 ML 样本（ML 训练数据保留期 ≤ 90d 通常即够）
	if s.features != nil && in.CustomerID != "" {
		res.FeaturesPurged = s.features.PurgeByCustomer(ctx, in.CustomerID)
	}

	// audit 不物理删（合规 ≥ 7 年留存）。
	// PII masking 由 audit-log 服务的离线 cron 兜底：每周扫一遍 GDPR erasure
	// 请求的 customer_id，把 DecisionAudit.Input 的 IPAddress / DeviceID / Metadata
	// 字段重写为 "ERASED"。本服务的 ring-buffer Sink 是进程内易失,重启即清。
	// 因此本地路径 AuditsMasked=0,符合预期(代表"不在本进程清理")。
	res.AuditsMasked = 0

	s.logger.Warn("personal data erased (GDPR right-to-erasure)",
		zap.String("customer_id", safety.MaskGeneric(in.CustomerID)),
		zap.String("requested_by", in.RequestedBy),
		zap.String("reason", in.Reason),
		zap.Int("link_edges_purged", res.LinkEdgesPurged),
		zap.Int("counters_purged", res.CountersPurged),
		zap.Int("features_purged", res.FeaturesPurged))
	return res, nil
}

// stageNameOfMax 给定 5 个 stage 耗时返回最大的那个的名字。
func stageNameOfMax(fe, ip, ml, eng, aud float64) string {
	type pair struct {
		name string
		v    float64
	}
	stages := []pair{
		{"feature_extract", fe},
		{"ip_intel", ip},
		{"ml_score", ml},
		{"engine_eval", eng},
		{"audit_write", aud},
	}
	max := stages[0]
	for _, p := range stages[1:] {
		if p.v > max.v {
			max = p
		}
	}
	return max.name
}
