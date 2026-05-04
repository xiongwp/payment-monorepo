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
	"sync"
	"time"
)

// ChampionChallengerService 多模型并行打分。Score 返回的是 champion 的 Result；
// challengers 输出通过 SideEffectFn 钩子吐出去（main.go 接 audit / featurestore）。
type ChampionChallengerService struct {
	mu          sync.RWMutex
	champion    namedSvc
	challengers []namedSvc

	// challengerTimeout 每个 challenger 独立超时；默认 50ms。
	challengerTimeout time.Duration

	// SideEffect 异步收 challenger 结果；nil = 静默丢。
	// 触发在 Score 返回之后；非阻塞。
	SideEffect func(decisionID string, championResult Result, challengerResults []NamedResult)
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

// Score 同步走 champion；challengers 并行打分；side effect 钩子异步触发。
//
// 决策路径只读 champion → 老模型 latency / 错误不被新模型影响。
// challenger 单个失败 / 超时不影响主路径，只在 SideEffect 结果里有 Error。
func (cc *ChampionChallengerService) Score(ctx context.Context, f Features) (Result, error) {
	cc.mu.RLock()
	champion := cc.champion
	challengers := append([]namedSvc(nil), cc.challengers...)
	timeout := cc.challengerTimeout
	se := cc.SideEffect
	cc.mu.RUnlock()

	chRes, err := champion.Service.Score(ctx, f)

	// challenger 在 Score 返回后再触发 SideEffect；同步等"50ms 内"全部跑完。
	// 即使 SideEffect=nil 也跑 challenger（让健康监控的 metric 继续）。
	if len(challengers) > 0 {
		go func() {
			results := runChallengers(challengers, f, timeout)
			if se != nil {
				se("", chRes, results)
			}
		}()
	}
	return chRes, err
}

// ScoreWithDecisionID 给已经分配 decision_id 的调用方用：SideEffect 收到的
// decisionID 是真的，而不是 ""。
func (cc *ChampionChallengerService) ScoreWithDecisionID(
	ctx context.Context, decisionID string, f Features,
) (Result, error) {
	cc.mu.RLock()
	champion := cc.champion
	challengers := append([]namedSvc(nil), cc.challengers...)
	timeout := cc.challengerTimeout
	se := cc.SideEffect
	cc.mu.RUnlock()

	chRes, err := champion.Service.Score(ctx, f)

	if len(challengers) > 0 {
		go func() {
			results := runChallengers(challengers, f, timeout)
			if se != nil {
				se(decisionID, chRes, results)
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
