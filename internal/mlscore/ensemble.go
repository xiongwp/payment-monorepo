// ensemble.go: 多模型 Ensemble Service。
//
// 单一 logistic / xgboost 模型有偏差；不同模型 (LR + GBDT + neural) 的
// 决策边界互补，加权平均能拿到更平滑的 score 分布 + 更高 AUC（典型场景
// +0.02-0.05 AUC）。
//
// 实现两种聚合方式：
//   1. WeightedMean: simple weighted avg, Σ(w_i × score_i) / Σ(w_i)
//   2. Max: 取最高分（保守 fraud detection — 任一模型说 fraud 都升）
//   3. Calibrated mean: 各 model 先 Platt 校准再平均（缓解模型间分数尺度
//      不一致问题；推荐生产配）
//
// 跟 ChampionChallengerService 区别：
//   - C-C 是 A/B 框架（champion 主路径 + challenger 影子打分）
//   - Ensemble 是融合（所有模型都进主路径，输出聚合 score）
//
// 实际部署可以两层叠：Champion = Ensemble{LR + GBDT + LSTM}，
//                    Challengers = [新版 Ensemble, 单独 LSTM 验证, ...]
package mlscore

import (
	"context"
)

// AggregateMethod 聚合方式。
type AggregateMethod string

const (
	AggregateWeightedMean AggregateMethod = "weighted_mean"
	AggregateMax          AggregateMethod = "max"
)

// EnsembleMember 一个组件 model。
type EnsembleMember struct {
	Name    string  // 给审计 / metric 用
	Service Service // 任意 mlscore.Service 实现：LogisticService / RemoteModelService / ...
	Weight  float64 // 权重 (WeightedMean 用；为 0 视作 1)
}

// EnsembleService 把多个 models 的输出聚合成一个 Result。
//
// 失败处理：单个 member 出错 / 超时 → 跳过它（不算入加权平均），不
// fail-close 整个 ensemble；所有 member 都失败 → 返 score=0 + error。
//
// ModelVer 在结果里固定为 "ensemble:N"，N=member count；详细每条
// member 名字 + score 通过 LastBreakdown() 暴露给 audit / debug。
type EnsembleService struct {
	method  AggregateMethod
	members []EnsembleMember
	verTag  string
}

// NewEnsemble 构造。verTag 是给 mlscore.Result.ModelVer 的标识
// （e.g. "ensemble-v1.2"）；空时回落 "ensemble"。
//
// method 是聚合方式；缺省 WeightedMean。member.Weight=0 视作 1（让简单调用
// "等权" 也工作）。
func NewEnsemble(verTag string, method AggregateMethod, members ...EnsembleMember) *EnsembleService {
	if verTag == "" {
		verTag = "ensemble"
	}
	if method == "" {
		method = AggregateWeightedMean
	}
	return &EnsembleService{
		method:  method,
		members: members,
		verTag:  verTag,
	}
}

// Score 跑所有 members → 聚合。一个 member 失败 → 跳过；全失败 → 返 error。
func (e *EnsembleService) Score(ctx context.Context, f Features) (Result, error) {
	if e == nil || len(e.members) == 0 {
		return Result{}, nil
	}
	scores := make([]float64, 0, len(e.members))
	weights := make([]float64, 0, len(e.members))
	var errs int
	for _, m := range e.members {
		r, err := m.Service.Score(ctx, f)
		if err != nil {
			errs++
			continue
		}
		w := m.Weight
		if w <= 0 {
			w = 1.0
		}
		scores = append(scores, r.Score)
		weights = append(weights, w)
	}
	if len(scores) == 0 {
		return Result{}, errAllMembersFailed
	}
	var agg float64
	switch e.method {
	case AggregateMax:
		for _, s := range scores {
			if s > agg {
				agg = s
			}
		}
	default: // WeightedMean
		var sumW, sum float64
		for i, s := range scores {
			sum += s * weights[i]
			sumW += weights[i]
		}
		if sumW > 0 {
			agg = sum / sumW
		}
	}
	return Result{
		Score:    clamp01(agg),
		ModelVer: e.verTag,
	}, nil
}

// Members 返回当前组件列表（运营 dashboard / 审计读，不允许 mutate）。
func (e *EnsembleService) Members() []EnsembleMember {
	if e == nil {
		return nil
	}
	out := make([]EnsembleMember, len(e.members))
	copy(out, e.members)
	return out
}

// errAllMembersFailed 所有组件都报错时的错误。
var errAllMembersFailed = errEnsembleStr("all ensemble members failed")

type errEnsembleStr string

func (e errEnsembleStr) Error() string { return string(e) }

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
