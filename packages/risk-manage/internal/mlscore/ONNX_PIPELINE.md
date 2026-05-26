# ONNX GBDT Pipeline

本文档描述 risk-manage 接入 ML 团队产出的 ONNX 格式 GBDT 模型的端到端流程。
读者：ML 同学（Python 训练侧）+ 风控工程（Go 推理侧）+ 运维（部署 / reload）。

## 背景

当前默认 ML scorer 是 `mlscore.LogisticService`（手工先验权重 + Go 离线 retrain 工具支持训练 LR）。
GBDT (LightGBM / XGBoost) 在 fraud detection 上召回率通常比 LR 高 5-15%，
但 Go 侧没有原生 GBDT 实现。社区方案：

```
ML 团队（Python）训练 GBDT → 导出 ONNX → Go 用 onnxruntime_go 推理
```

ONNX 是开放神经网络交换格式，scikit-learn / LightGBM / XGBoost / TensorFlow / PyTorch
都能导出。Go 侧用 `github.com/yalue/onnxruntime_go`（依赖 native libonnxruntime）。

## 职责划分

| 阶段 | 谁负责 | 工具 |
|---|---|---|
| 特征生成（在线） | 风控工程 | `features.Chain` + `mlscore.Features` |
| 特征生成（离线） | ML 团队 | Spark / DuckDB query feature_snapshot stream |
| 特征定义/版本管理 | ML + 工程 共建 | Feast feature store（推荐）/ 同步表格 |
| 模型训练 | ML 团队 | Python notebook (LightGBM / XGBoost) |
| ONNX 转换 | ML 团队 | `onnxmltools` / `skl2onnx` |
| 模型存储 | 运维 | S3 / OSS / object store |
| 模型加载 | Go 服务 | `mlscore.OnnxService.Reload` |
| 推理 | Go 服务 | `OnnxService.Score(ctx, Features) → Result` |
| 灰度发布 | 工程 | `ChampionChallengerService` + rollout |
| 漂移监控 | 工程 | `DriftMonitor` (PSI / KS) |

## 一、ML 团队工作流（Python）

### 1.1 离线数据准备

从 risk-manage 落库的 `feature_snapshot` stream + `risk_outcome` 表 join：

```sql
-- DuckDB / Trino 示例
SELECT
  s.features,           -- JSON featureset
  o.outcome_label,      -- 0=legit / 1=fraud (chargeback 7+ days)
  s.snapshot_at
FROM feature_snapshots s
JOIN risk_outcomes o USING (decision_id)
WHERE s.snapshot_at BETWEEN '2026-01-01' AND '2026-04-30'
  AND o.outcome_known = TRUE
```

落 parquet（推荐）或 JSONL。

### 1.2 训练

```python
import lightgbm as lgb
import pandas as pd
from onnxmltools.convert import convert_lightgbm
from skl2onnx.common.data_types import FloatTensorType

df = pd.read_parquet("snapshots.parquet")

# 这个顺序是关键 contract — 跟 Go OnnxService featOrder 必须 1:1 对齐
FEATURE_ORDER = [
    "amount",
    "ip_proxy",
    "ip_vpn",
    "ip_datacenter",
    "ip_country_mismatch",
    "no_fingerprint",
    "headless_renderer",
    "low_concurrency",
    "rapid_checkout",
    "no_mouse_entropy",
    "bot_typing_rhythm",
    "no_keystrokes",
    "high_risk_country",
]

X = df[FEATURE_ORDER].values.astype("float32")
y = df["outcome_label"].values

clf = lgb.LGBMClassifier(
    num_leaves=31,
    n_estimators=200,
    learning_rate=0.05,
    class_weight="balanced",   # fraud 通常 <5%
)
clf.fit(X, y)

# 用 hold-out 评估
# (略：标准 sklearn train_test_split + roc_auc_score)
```

### 1.3 导出 ONNX

```python
onnx_model = convert_lightgbm(
    clf,
    initial_types=[("input", FloatTensorType([None, len(FEATURE_ORDER)]))],
    target_opset=12,
    zipmap=False,    # 重要：不要 zipmap，直接输出概率 tensor
)
sha = subprocess.check_output(["git", "rev-parse", "--short", "HEAD"]).decode().strip()
fname = f"risk_gbdt_v2.3_{sha}.onnx"
with open(fname, "wb") as f:
    f.write(onnx_model.SerializeToString())
```

### 1.4 上传

```bash
aws s3 cp risk_gbdt_v2.3_<sha>.onnx s3://risk-ml-models/onnx/
# 或：阿里云 OSS / 内部 model registry
```

也写一份 metadata：
```json
{
  "model_ver": "risk_gbdt_v2.3_a1b2c3d",
  "feature_order": ["amount", "ip_proxy", ...],
  "trained_at": "2026-05-25T10:00:00Z",
  "training_samples": 1234567,
  "metrics": {"auc": 0.94, "precision@recall=0.9": 0.62},
  "git_sha": "a1b2c3d"
}
```

## 二、Go 服务端职责

### 2.1 配置启用

`config/config.yaml`：

```yaml
mlscore:
  enabled: true
  onnx:
    model_path: "/var/models/risk_gbdt_v2.3_a1b2c3d.onnx"
    feature_order:
      - amount
      - ip_proxy
      - ip_vpn
      - ip_datacenter
      - ip_country_mismatch
      - no_fingerprint
      - headless_renderer
      - low_concurrency
      - rapid_checkout
      - no_mouse_entropy
      - bot_typing_rhythm
      - no_keystrokes
      - high_risk_country
```

`model_path != ""` → 启动时尝试 `NewOnnxService`；失败 fallback 到 LogisticService。

### 2.2 Build

默认 build 不带 onnxruntime 依赖（保持镜像精简）。要启用 ONNX：

```bash
# 安装 onnxruntime native lib
# macOS: brew install onnxruntime
# Ubuntu: apt-get install libonnxruntime-dev
# 或下载二进制 release: https://github.com/microsoft/onnxruntime/releases

go build -tags onnx -o risk-server ./cmd/server
```

Docker 镜像需要在 Dockerfile 加：

```dockerfile
RUN apt-get update && apt-get install -y libonnxruntime-dev
# 或 COPY 一份预编 libonnxruntime.so.1.x.y 到 /usr/local/lib/
ENV ONNXRUNTIME_LIB_PATH=/usr/local/lib/libonnxruntime.so
```

### 2.3 运行时

`OnnxService.Score(ctx, Features)` 路径：

1. 读 `atomic.Pointer[onnxSession]`（无锁）
2. `featuresToFloat32(f, featOrder)` 按 contract 顺序构造 `[]float32`
3. `ort.AdvancedSession.Run()`
4. 取 output[0] → clamp [0,1] → `Result{Score, ModelVer}`

延时预算：100 维 GBDT 200 树 ~1-3ms（CPU），含特征转换 < 5ms total。

### 2.4 Admin Reload（不重启）

```bash
curl -X POST http://risk-host:8080/admin/ml/onnx/reload \
  -H 'Content-Type: application/json' \
  -d '{
    "model_path": "/var/models/risk_gbdt_v2.4_e4f5g6h.onnx",
    "feature_order": ["amount", "ip_proxy", ...]
  }'
```

响应：
```json
{
  "enabled": true,
  "model_path": "/var/models/risk_gbdt_v2.4_e4f5g6h.onnx",
  "model_ver": "risk_gbdt_v2.4_e4f5g6h",
  "feature_order": [...]
}
```

Reload 是原子的：用 `atomic.Pointer` swap session；旧 session 延迟 5s 释放
（等 in-flight 请求完成），全程 Score 不阻塞、不 panic。

### 2.5 状态查询

```bash
curl http://risk-host:8080/admin/ml/onnx/info
# {"enabled":true,"model_path":"...","model_ver":"...","feature_order":[...]}
```

## 三、特征 Contract

**最重要的一条**：`feature_order` 必须 ML / 工程双方约定，且每次模型升级时
**双方同时更新**。

工程侧的检查：
- `NewOnnxService` 加载时校验模型 input shape 最后一维 == `len(feature_order)`，
  不对就拒绝。
- 未知特征名（不在 `extractFeature` 的 switch case）→ 用 0 填 + log warn 一次。
- `nil` / `NaN` / `Inf` → 转 0。

新增特征流程：
1. ML 同学先在 PR 里列出新特征名（snake_case） + 含义
2. 工程侧加 `extractFeature` 的 case 实现
3. 双方 merge 后 ML 同学用包含新特征的 X 训练 → 导出 ONNX
4. 配置文件 `feature_order` 补齐 → admin reload

## 四、特征一致性（离线/在线）

最容易踩的坑：训练数据用的特征值跟生产推理算出来的不一样
（比如 `time_to_checkout_ms` 离线用 `event_finalize_at - event_start_at`，
在线用 `now() - session_start_at`，时区不同 → 模型 garbage in garbage out）。

推荐方案：
1. **Feast feature store**：所有特征定义在 feast yaml，离线 / 在线共用同一份
   feature retrieval 逻辑。
2. **Feature snapshot 落库 + 回灌训练**：生产推理每条决策都把当时算出的
   `Features` 落 `feature_snapshot` stream → 训练直接消费 snapshot，天然一致。
   `risk-manage` 当前走这条路径（见 `internal/featurestore`）。

ML 同学训练时**优先用 snapshot 字段**，不要自己从原始事件重算！

## 五、模型版本管理

文件名约定（强建议）：

```
<service>_<algo>_<ver>_<git_sha>.onnx

例：risk_gbdt_v2.3_a1b2c3d.onnx
    risk_xgb_v3.0_e4f5g6h.onnx
```

- `<service>` = "risk"（fraud detection 主路径）/ "chargeback_predict" 等
- `<algo>` = "gbdt" / "xgb" / "lr" / "nn"
- `<ver>` = 业务侧版本（v2.3 = 第 2 大版本第 3 次迭代）
- `<git_sha>` = 训练代码的 git short sha（可重现 + 审计）

`OnnxService.ModelVersion()` 自动从文件名派生（去扩展名），写到每条 audit 行的
`model_ver` 字段，方便 PostMortem："这条误拦截是哪个模型版本"。

## 六、灰度 / 上线流程

不要直接换 champion！标准流程：

1. **Shadow / Challenger** 阶段（1-2 周）：
   - 把新 `OnnxService` 注册为 `ChampionChallengerService.RegisterChallenger`
   - 旧 LogisticService / 老 ONNX 仍是 champion，决策走它
   - 新模型的 score 写到 audit 但不影响决策
   - 看 side-by-side：新模型在 chargeback 数据上的召回 / 误伤对比

2. **Rollout** 阶段：
   - `StartRollout` 5% → 25% → 50% → 100%（每个 stage 24-72h）
   - 按 `sha256(customer_id) % 100` 切流，同一商户行为稳定
   - 监控关键指标：拦截率 / 误伤率 / 客诉

3. **Promote**：
   - `PromoteChallenger("risk_gbdt_v2.3_a1b2c3d")` → 新模型变 champion
   - 老模型降级为 challenger 留观一周

4. **Rollback**（应急）：
   - 业务指标恶化 → admin reload 回老模型（旧 .onnx 文件保留）
   - 或 ChampionChallenger `PromoteChallenger("logistic-v1")` 切回 LR

## 七、漂移监控

`mlscore.DriftMonitor` 在线 reservoir-sample 每个特征 + 模型输出 score，
定期算 PSI / KS 跟 baseline 对比：

- `GET /admin/ml/drift/status` → 全特征 PSI / KS / verdict
- PSI > 0.1 warning，> 0.25 critical
- 新模型上线后第一周用新 score baseline：`POST /admin/ml/drift/baseline`

漂移 critical 时：
1. 检查最近是否新流量来源（merchant onboarding / 地域扩张）
2. ML 团队拿最新 snapshot 触发重训
3. 走 §六 灰度流程上新模型

## 八、风险点 / Known Issues

1. **onnxruntime native 依赖**：CGO 调用，崩了会带掉整个进程。
   `OnnxService.Reload` 用 5s 延迟释放兜底，在线大流量场景再调大。
2. **Tensor buffer 复用 vs 并发**：当前实现 `Score` 走 `mu` 串行化
   （单 session shared input buffer）。QPS > 5k 时换成 per-goroutine session pool。
3. **冷启动**：第一次 Score 比稳态慢 10-50x（CPU cache + onnx graph 优化）。
   `cmd/server` 的 `startWarmup` 用 5-10 条 synthetic 流量预热。
4. **模型文件分发**：admin reload 假设 `model_path` 已经在本地。
   集群部署需要先把 .onnx push 到每台机器（k8s configmap 太大装不下 GBDT，
   推荐 sidecar 从 S3 sync 到 `/var/models/` + admin reload）。

## 九、Roadmap

短期（Q3）：
- [ ] Dockerfile 加 libonnxruntime（默认 build 仍不带）
- [ ] ML 团队产出第一版 GBDT.onnx（v2.0）
- [ ] CI 加 `-tags onnx` 测试 job（nightly）

中期（Q4）：
- [ ] 接 Feast feature store
- [ ] Batch inference (`ScoreBatch`) 给 offline backfill 用
- [ ] GPU inference 选项（onnxruntime CUDAExecutionProvider）

长期：
- [ ] 自动 retrain pipeline（每周 cron + champion-challenger 自动 promote 阈值）
