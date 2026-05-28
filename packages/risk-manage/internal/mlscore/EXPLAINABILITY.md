# 决策可解释性 (Decision Explainability)

## 背景

`risk-manage` 现 `audit.DecisionAudit` 只记 _命中的 rule + 总 ml_score_。
运营接到商户投诉时（"我的客户被你们风控误判了"）经常答不出：

> "为什么 ML 给这笔 0.4 分？哪个特征推高的？"

本模块加 SHAP-style **feature contribution attribution** + per-rule
contribution，让 `/admin/decisions/{id}/explain` 直接列出 "top-K 推风险
信号 + top-K 降风险信号 + 每条 rule 的 score_delta"，运营 30 秒答疑。

## 用户场景

```
商户客服 → 运营：客户 cust_42 报投诉，订单 pi_abcd 被你们 DENY 了
运营 → admin UI：搜 payment_intent_id=pi_abcd → decision_id=dec_xyz
运营 → /admin/decisions/dec_xyz/explain:
{
  "base_value": -2.5,
  "final_score": 0.87,
  "ml_model_version": "logistic-v1.0-prior",
  "top_contributors": [
    {"feature": "rapid_checkout", "value": 1.0, "contribution": 1.2, "direction": "increase_risk"},
    {"feature": "ip_vpn", "value": 1.0, "contribution": 0.8, "direction": "increase_risk"},
    {"feature": "no_fingerprint", "value": 1.0, "contribution": 0.6, "direction": "increase_risk"}
  ],
  "rule_contributions": [
    {"rule_id": "ml_threshold", "hit": true, "weight": 40, "score_delta": 40, "sequence": 1, "detail": "ml_score=0.87 ≥ 0.7"},
    {"rule_id": "high_amount", "hit": false, "sequence": 2}
  ]
}

运营 → 商户客服：
  "这笔触发 3 个强信号：(1) 用户结账时间 < 2 秒（典型 bot），
  (2) 来自 VPN，(3) 没有设备指纹。其中 rapid_checkout 单条贡献 +1.2 logit。
  如果客户能提供正常浏览器指纹 + 关掉 VPN 重试，规则会正常放行。"
```

## 架构

```
┌─── service.Screen ─── engine.Evaluate
│         │
│         └── mlSvc.Score → ML score (0-1)
│
│   recordAuditWith()
│         │
│         ├── 主路径：sink.Write(DecisionAudit{...})
│         │
│         └── 同步副路径：explainer.Explain(features, topK=10)
│               ↓
│         AttachExplain(audit, block)   ← 失败 log warn 不阻塞
│
└─→ /admin/decisions/{id}/explain
        ├── audit.LookupByDecisionID(id)
        ├── 有 audit.Explain → 直接返
        └── 没有 → explainer.Explain 重算 + 现填
```

## 实现

| 组件 | 文件 | 职责 |
|---|---|---|
| 接口 + types | `mlscore/explain.go` `ExplainResult` / `FeatureContribution` / `Explainer` | 决策 → 解释结构 |
| Logistic 解释器 | `mlscore/explain.go` `LogisticExplainer` | LR 模型：`contribution = weight × indicator` |
| ONNX SHAP 解释器 | `mlscore/explain.go` `OnnxShapExplainer` | 加载 `.shap.json`，`contribution = importance × (value - mean)` |
| audit 集成 | `audit/explain.go` `ExplainBlock` + `RuleContribution` | 落 audit ring；admin 端点拉 |
| HTTP 端点 | `cmd/server` `registerDecisionExplainHandler` | `GET /admin/decisions/{id}/explain` |
| Python 训练侧 | `mlscore/explain_export.py` | 训练后跑 `shap.TreeExplainer` 产 `.shap.json` |

## 算法选择

### 简化版 SHAP（**不严格 Shapley**）

```
contribution_i = w_i × (x_i - baseline_mean_i)
```

- **LogisticExplainer**：`w_i` = LogisticConfig.Weights；`baseline_mean_i` = 0
  （特征都是二值 indicator）→ `contribution = weight × indicator`，这是
  **logit 空间的真实贡献**（不是近似）—— 因为 LR 是可加模型，shapley
  退化为 weight × value。
- **OnnxShapExplainer**：`w_i` = mean(|shap_value|) per feature（Python
  侧 `shap.TreeExplainer` 算）；`baseline_mean_i` = X_train.mean()。
  这是 **deterministic feature attribution**，不是真 Shapley—— TreeSHAP
  per-sample 算要遍历整棵 forest，Go 侧没实现。

### 为什么用简化版而不是 KernelSHAP

| 方案 | per-sample 延迟 | Go 侧实现复杂度 | 严格性 |
|---|---|---|---|
| KernelSHAP (model-agnostic) | 100ms-10s | 中（要重复推理） | 近似 |
| TreeSHAP (per-tree exact) | < 1ms | 高（要翻译整棵 forest 到 Go） | 精确 |
| **简化 attribution（本实现）** | < 1µs | 极低 | 方向 + 量级正确 |

对答疑场景（"哪些特征推风险"），方向 + 相对量级足够；严格分配 Shapley
值是 reporting / 监管场景才需要。

## 排序

`top_contributors` 按 **|contribution|** 降序：

- 优先级不分正负 — 既要看推风险的（VPN / rapid checkout），也要看抑制
  风险的（fingerprint OK / 正常 keystroke），都是"最有解释力"的信号
- 相同 |contribution| 按 feature name 字典序（稳定输出，便于 diff）
- topK ≤ 0 / topK > total → 返全量；零贡献的从末尾截断

## audit 集成 — 同步 vs 异步

**同步**。理由：

1. **数据完整性**：异步路径，决策已经响应给 payment-core，audit 里没
   explain 块 → admin 端点拉的时候要重新跑 features 提取 + ML inference，
   逻辑复杂 + 容易 train-serve skew。
2. **延迟可控**：LogisticExplainer < 1µs；OnnxShapExplainer < 10µs
   （纯 map 查 + 乘加）。相对 engine.Evaluate (1-10ms) + ML inference
   (1-50ms) 完全 negligible。
3. **失败兜底**：`AttachExplain` 包了 recover；explainer 返 err → log warn
   留 nil，admin 端点会按需重算（详见 cmd/server/main.go）。

异步版本（未来 / OnnxShap on hot path）：用 `audit.AsyncSink` 风格的 channel
buffer + worker，但本期不做。

## 限制

1. **简化 SHAP**：不严格 Shapley 值，不能拿来做 fair-lending 监管报告。
   监管要求严格 attribution 时切到 Python shap-service（POST 一笔特征
   返完整 SHAP）。
2. **GBDT/XGBoost 精确 SHAP**：需要 ML 团队在 Python 侧产 per-sample
   shap_values 落库（一起跟 decision_id 入 ClickHouse）→ Go 侧从库里直
   接拉，不重算。本期没做。
3. **LSTM / 神经网络**：完全不支持。简化公式假设可加模型；非线性模型
   要 `shap.DeepExplainer` / `LIME`，Go 侧不可能复现。

## 后续

- [ ] 接 [`github.com/marcojaviergonzalez/onnx-shap-go`](https://github.com/marcojaviergonzalez/onnx-shap-go)（如果发布）或自实现 TreeSHAP（参考 LightGBM 论文）
- [ ] Python shap-service：Go 侧 gRPC 到 shap-service，per-sample 精确 SHAP
- [ ] 把 `RuleContribution.Weight` 从 engine 拿真实 weight 填（当前 stub）
- [ ] admin UI 把 `top_contributors` 渲染成 horizontal bar chart（红绿条）

## 测试

- `mlscore/explain_test.go`：LogisticExplainer 排序 / topK / 全 0 边界 /
  跟 LogisticService 分数一致性 / OnnxShapExplainer 加载 .shap.json + 缺失文件
- 集成测试待补（admin endpoint e2e）— 不在本期范围

## 兼容性

- 默认 build（无 `-tags onnx`）：LogisticExplainer 工作；OnnxShapExplainer
  在没有 .shap.json 时构造失败 → fallback 走 LogisticExplainer。
- 老 audit 行（没有 `Explain` 字段）：admin 端点重新算一次填到响应里
  （不修改 ring 里的老条目，避免锁开销）。
