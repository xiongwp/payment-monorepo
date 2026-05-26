// champion_challenger.go: 把 ChampionChallenger Service 接到现有 Service 接口。
//
// 商业级 ML 部署的标准模式：
//
//   - **Champion**：当前生产模型；它的 Score 是真正驱动 ml_threshold 规则的值
//   - **Challenger(s)**：候选新模型；并行打分但**不**影响 Decision，输出
//     Side-by-side 落 audit / featurestore，给 offline 评估"如果切到 challenger，
//     拦截率会更高吗？误伤率会上升吗？"
//
// Score 顺序：Champion 同步执行（影响主路径延时）；challengers 并行 goroutine，
// 不阻塞 caller（worst case challenger 慢一点 audit 行少几条 challenger_score
// 字段，不影响线上决策）。每个 challenger 独立超时 50ms 防慢模型拖死。
//
// 上线流程：
//   1. 训出新模型 v2 → 注册为 challenger
//   2. 跑 1-2 周收集 side-by-side 样本
//   3. 离线评估：v2 在 chargeback 上召回率提升 X% / 误伤率下降 Y%
//   4. PromoteChallenger("v2") → champion 切到 v2，老 v1 降级为 challenger 留观
//
// 审计：每条 Score 调用日志 / featurestore snapshot 都包含全部 challenger 输出。
package mlscore

import (
	"context"
	"math/rand"
	"sync"
	"time"
)

// ChampionChallengerService 多模型并行打分。Score 返回的是 champion 的 Result；
// challengers 输出通过 SideEffectFn 钩子吐出去（main.go 接 audit / featurestore）。
//
// 灰度发布（rollout）：StartRollout 启动后，主决策路径按 sha256(customer_id +
// rollout_id) % 100 决定走 champion 还是 challenger。流量比例按 Stage（5/25/
// 50/100）逐步推进——见 rollout.go。没启动 rollout 时退化成原 atomic-promote
// 行为，主路径永远走 champion。
type ChampionChallengerService struct {
	mu          sync.RWMutex
	champion    namedSvc
	challengers []namedSvc

	// challengerTimeout 每个 challenger 独立超时；默认 50ms。
	challengerTimeout time.Duration

	// SideEffect 异步收 challenger 结果；nil = 静默丢。
	// 触发在 Score 返回之后；非阻塞。
	SideEffect func(decisionID string, championResult Result, challengerResults []NamedResult)

	// rollout 当前激活的灰度发布；nil 表示无 rollout（主路径一律走 champion）。
	// 受 mu 保护。
	rollout *rollout

	// OnRolloutEvent 灰度发布的关键节点回调（advance / rollback / auto-rollback
	// / finalize）。caller 在 main.go 接 audit / event bus / metrics。
	// 在持 mu 时同步调用——避免长时间阻塞操作（写慢 sink 应自己内部 goroutine）。
	OnRolloutEvent func(evt RolloutEvent)

	// matchlessRng 给没有 customer_id 的请求抛硬币用；线性同余够了，安全性
	// 不重要（不是 password / token）。复用避免每次 NewSource 浪费。
	// 注意 *rand.Rand 不是并发安全的——当前 routeToChallenger 用默认
	// matchlessFallback=false 时根本不调它；如果将来打开 fallback，得换成
	// 加 mu 保护或用 rand.Reader / sync.Pool。
	matchlessRng *rand.Rand
}

type namedSvc struct {
	Name    string // model id（"logistic_v1" / "xgb_v2"）
	Service Service
}

// NamedResult challenger 的输出，main.go 收到后写到 audit / event bus。
type NamedResult struct {
	Name   string  `json:"name"`
	Score  float64 `json:"score"`
	ModelVer string `json:"model_ver,omitempty"`
	Error  string  `json:"error,omitempty"`
	LatMs  float64 `json:"latency_ms,omitempty"`
}

// NewChampionChallenger 单 champion 起步；challengers 后续 RegisterChallenger。
func NewChampionChallenger(championName string, champion Service) *ChampionChallengerService {
	return &ChampionChallengerService{
		champion:          namedSvc{Name: championName, Service: champion},
		challengerTimeout: 50 * time.Millisecond,
		matchlessRng:      rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (cc *ChampionChallengerService) SetChallengerTimeout(d time.Duration) {
	cc.mu.Lock()
	cc.challengerTimeout = d
	cc.mu.Unlock()
}

// RegisterChallenger 注册一个 challenger。同 name 替换。
func (cc *ChampionChallengerService) RegisterChallenger(name string, svc Service) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	for i, c := range cc.challengers {
		if c.Name == name {
			cc.challengers[i].Service = svc
			return
		}
	}
	cc.challengers = append(cc.challengers, namedSvc{Name: name, Service: svc})
}

// RemoveChallenger 移除一个 challenger（admin "下线" 路径用）。返回 true 表示
// 找到并移除。线上模型不再用时直接停 score 节省 latency；champion 不能用此
// 方法移除（要先 promote 别的 challenger）。
func (cc *ChampionChallengerService) RemoveChallenger(name string) bool {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	for i, c := range cc.challengers {
		if c.Name == name {
			cc.challengers = append(cc.challengers[:i], cc.challengers[i+1:]...)
			return true
		}
	}
	return false
}

// PromoteChallenger 把指定 challenger 提升为新 champion；老 champion 自动降级
// 为 challenger 留观（除非同名）。线上模型 promotion 后保留对照样本至关重要。
//
// 兼容性：等价于一次完整 rollout 推到 100%；如果当前正在跑 rollout 且
// promote 的就是 rollout challenger，直接清空 rollout（rollout 已完成）。
// 其它 case 不动 rollout（admin 应该先 rollback 或 finalize）。
func (cc *ChampionChallengerService) PromoteChallenger(name string) bool {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	for i, c := range cc.challengers {
		if c.Name != name {
			continue
		}
		oldCh := cc.champion
		cc.champion = c
		cc.challengers = append(cc.challengers[:i], cc.challengers[i+1:]...)
		if oldCh.Name != "" && oldCh.Name != name {
			cc.challengers = append(cc.challengers, oldCh)
		}
		// rollout 指向了这个 challenger → 等价于"灰度直接完成"。
		if cc.rollout != nil && cc.rollout.challenger.Name == name {
			cc.rollout = nil
		}
		return true
	}
	return false
}

// ChampionName 当前 champion 名（运维 / metric 用）。
func (cc *ChampionChallengerService) ChampionName() string {
	cc.mu.RLock()
	defer cc.mu.RUnlock()
	return cc.champion.Name
}

// ChallengerNames 当前所有 challenger 名（用于 dashboard 展示）。
func (cc *ChampionChallengerService) ChallengerNames() []string {
	cc.mu.RLock()
	defer cc.mu.RUnlock()
	out := make([]string, 0, len(cc.challengers))
	for _, c := range cc.challengers {
		out = append(out, c.Name)
	}
	return out
}

// Score 同步走 routed model（rollout 决定 champion or challenger）；其它
// challengers 并行打分；side effect 钩子异步触发。
//
// 决策路径只读 routed model → 老模型 latency / 错误不被未参与 rollout 的
// challenger 影响。challenger 单个失败 / 超时不影响主路径，只在 SideEffect
// 结果里有 Error。
func (cc *ChampionChallengerService) Score(ctx context.Context, f Features) (Result, error) {
	return cc.scoreInternal(ctx, "", f)
}

// ScoreWithDecisionID 给已经分配 decision_id 的调用方用：SideEffect 收到的
// decisionID 是真的，而不是 ""。
func (cc *ChampionChallengerService) ScoreWithDecisionID(
	ctx context.Context, decisionID string, f Features,
) (Result, error) {
	return cc.scoreInternal(ctx, decisionID, f)
}

func (cc *ChampionChallengerService) scoreInternal(
	ctx context.Context, decisionID string, f Features,
) (Result, error) {
	// 大多数请求（无 rollout）走 read lock fast path；rollout active 时再升级
	// 到 write lock 更新 hits 计数。
	cc.mu.RLock()
	champion := cc.champion
	challengers := append([]namedSvc(nil), cc.challengers...)
	timeout := cc.challengerTimeout
	se := cc.SideEffect
	hasRollout := cc.rollout != nil
	primary := champion
	routedToChallenger := false
	if hasRollout && cc.rollout.routeToChallenger(f.CustomerID, false /* matchlessFallback */, cc.matchlessRng) {
		primary = cc.rollout.challenger
		routedToChallenger = true
	}
	cc.mu.RUnlock()
	if hasRollout {
		cc.mu.Lock()
		if cc.rollout != nil { // 状态没在 Unlock 间隙被外部 finalize 掉
			if routedToChallenger {
				cc.rollout.challengerHits++
			} else {
				cc.rollout.championHits++
			}
		}
		cc.mu.Unlock()
	}

	chRes, err := primary.Service.Score(ctx, f)

	// SideEffect 总是按 (championResult, challengerResults) 接口约定来：即使
	// 主路径走了 challenger，也要给 ABTracker 喂 "champion 视角下这笔评分应是
	// 什么" → 必须额外跑 champion 一遍才能算 AUC 差。代价是 rollout 期间
	// champion + challenger 都跑（routing 只决定主决策用哪个 score），但
	// challenger 跑在 goroutine 里 + 有超时；champion 在 rollout 期间被路由
	// 走时同样异步跑。
	if se != nil || len(challengers) > 0 {
		go func() {
			// runChallengers 跑 challenger 并行；若主路径已经走了 challenger，
			// 从 toRun 剔除避免重复跑同一模型。
			toRun := challengers
			if routedToChallenger {
				filtered := make([]namedSvc, 0, len(challengers))
				for _, c := range challengers {
					if c.Name != primary.Name {
						filtered = append(filtered, c)
					}
				}
				toRun = filtered
			}
			// 补跑 champion 让 ABTracker 始终拿到 champion_score（routing 决定
			// 主决策用哪个，但 A/B 评估始终需要两边的分）。
			var supplementalChampion *NamedResult
			if routedToChallenger {
				cctx, cancel := context.WithTimeout(context.Background(), timeout)
				start := time.Now()
				r, e := champion.Service.Score(cctx, f)
				cancel()
				if e == nil {
					nr := NamedResult{
						Name:     champion.Name,
						Score:    r.Score,
						ModelVer: r.ModelVer,
						LatMs:    time.Since(start).Seconds() * 1000,
					}
					supplementalChampion = &nr
				}
			}
			results := runChallengers(toRun, f, timeout)
			if se != nil {
				championResult := chRes
				if routedToChallenger {
					if supplementalChampion == nil {
						// champion 补跑失败 → 不能给 ABTracker 假 baseline，跳过此次
						return
					}
					championResult = Result{
						Score:    supplementalChampion.Score,
						ModelVer: supplementalChampion.ModelVer,
					}
					results = append(results, NamedResult{
						Name:     primary.Name,
						Score:    chRes.Score,
						ModelVer: chRes.ModelVer,
					})
				}
				se(decisionID, championResult, results)
			}
		}()
	}
	return chRes, err
}

func runChallengers(challengers []namedSvc, f Features, timeout time.Duration) []NamedResult {
	results := make([]NamedResult, len(challengers))
	var wg sync.WaitGroup
	for i, c := range challengers {
		wg.Add(1)
		go func(i int, c namedSvc) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			start := time.Now()
			r, err := c.Service.Score(ctx, f)
			lat := time.Since(start).Seconds() * 1000
			out := NamedResult{Name: c.Name, LatMs: lat}
			if err != nil {
				out.Error = err.Error()
			} else {
				out.Score = r.Score
				out.ModelVer = r.ModelVer
			}
			results[i] = out
		}(i, c)
	}
	wg.Wait()
	return results
}
