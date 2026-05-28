# risk-manage ML training pipeline

Python 侧风控 ML 真训 pipeline。**ML 团队 cold-start 训练 + 部署链路目标 < 1 天**。

> Go 侧 `cmd/retrain/main.go` 是 baseline logistic regression（运维 fallback / 单测用）；
> 本目录是**正式版**：LightGBM GBDT + PyTorch LSTM + ONNX 导出 + S3 部署。

---

## 0. TL;DR — 复盘流程

```bash
# 一次性：装依赖
python -m venv .venv && source .venv/bin/activate
pip install -r requirements.txt

# 拉数据
export CLICKHOUSE_DSN="clickhouse://user:pass@ch-prod:9000/risk"
python data/fetch_training_data.py --out-dir ./data/out --window-days 90 --holdout-days 7

# 训 GBDT
python train_gbdt.py --input ./data/out/train.parquet --holdout ./data/out/holdout.parquet \
    --output-dir ./out/gbdt

# 训 LSTM（行为序列）
python train_lstm.py --input ./data/out/behavior_train.parquet --output-dir ./out/lstm

# 上传 + 触发 staging reload
python deploy/upload_artifact.py --artifact-dir ./out/gbdt --model-type gbdt \
    --s3-bucket risk-models-prod --admin-url http://risk-staging:9590 \
    --admin-token "$RISK_STAGING_TOKEN"

# 灰度（staging shadow 1 周后才推 prod；详见 §5）
curl -X POST http://risk-prod:9590/admin/ml/rollout/start \
    -H "Authorization: Bearer $RISK_PROD_TOKEN" \
    -d '{"initial_pct":5,"stages":[25,50,100],"auto_advance":false}'
```

---

## 1. 目录结构

```
ml-training/
├── requirements.txt           # 钉死版本，跨机可复现
├── README.md                  # 本文档（ML team SOP）
├── data/
│   └── fetch_training_data.py # CH 90d → train.parquet + holdout.parquet
├── train_gbdt.py              # LightGBM + skl2onnx → model.onnx + model.shap.json
├── train_lstm.py              # PyTorch LSTM → behavior_lstm.onnx
└── deploy/
    └── upload_artifact.py     # → S3 + POST /admin/ml/onnx/reload
```

调度：`deploy/airflow/dags/risk_retrain_dag.py`（仓库根目录 deploy/ 下）。
DAG 把上述四步串起来，weekly + on-PSI-drift 两种触发。

---

## 2. 环境

### 2.1 Python

* **Python 3.10 – 3.11**（3.12 上 lightgbm 4.1 偶发 segfault；3.9 lightgbm onnx convert 报 opset）
* CPU 训练即可。GBDT 10k 样本 < 5 分钟；LSTM 10k × 40 epoch < 30 分钟。

### 2.2 装依赖

```bash
python -m venv .venv && source .venv/bin/activate
pip install -r requirements.txt
# torch 国内推荐 CPU wheel：
pip install torch --index-url https://download.pytorch.org/whl/cpu
```

### 2.3 凭证

```bash
# .env（python-dotenv 自动读，也可手动 export）
CLICKHOUSE_DSN=clickhouse://user:pass@ch-prod:9000/risk
AWS_ACCESS_KEY_ID=...
AWS_SECRET_ACCESS_KEY=...
RISK_STAGING_TOKEN=...
RISK_PROD_TOKEN=...
RISK_MODELS_BUCKET=risk-models-prod
```

---

## 3. 数据

### 3.1 从 ClickHouse 拉

```bash
python data/fetch_training_data.py \
    --out-dir ./data/out \
    --window-days 90 \
    --holdout-days 7
```

* 90 天窗口：覆盖典型 chargeback 反馈周期（dispute 一般 30-60d 落地）
* time-based 切分：最后 7 天进 `holdout.parquet`，**防 leakage**
* 输出：`train.parquet`、`holdout.parquet`、`meta.json`（行数 / fraud 率 / 窗口）

### 3.2 特征列表（feature_order）

**数值（14）**：amount, hw_concurrency, time_to_checkout_ms, mouse_entropy,
click_interval_ms, typing_cv, keystroke_count, velocity_1h, velocity_24h,
distinct_ips_24h, bin_fraud_rate, geo_distance_km, account_age_days,
recent_disputes_30d

**分类（6，one-hot 展开）**：country, device_type, card_brand, channel, browser, os_family

完整 list 见 `data/fetch_training_data.py` 文件头注释，必须跟 `internal/mlscore/OnnxService.feature_order`
**严格一致**（admin reload 时显式传入校验）。

### 3.3 没 CH 怎么办（dev / CI）

```bash
# 把 prod 上同事下好的 sample 放本地
aws s3 cp s3://risk-models-prod/samples/2026q1_sample.parquet ./sample.parquet
python data/fetch_training_data.py --out-dir ./data/out --fallback-sample ./sample.parquet
```

CH 连不上会自动 fallback；CI 跑 smoke test 也走这条。

---

## 4. 训练

### 4.1 GBDT

```bash
python train_gbdt.py \
    --input ./data/out/train.parquet \
    --holdout ./data/out/holdout.parquet \
    --output-dir ./out/gbdt
```

**超参（钉死，调要走 PR）**：

| 超参 | 值 | 备注 |
|------|-----|------|
| n_estimators | 200 | + early-stopping 100 round |
| learning_rate | 0.05 | 经典稳态值 |
| num_leaves | 31 | 数据 < 100k 再大就过拟 |
| min_data_in_leaf | 50 | 防小叶过拟（fraud rate < 1%）|
| scale_pos_weight | n_neg / n_pos | 自动算，对抗 imbalance |
| n_splits (CV) | 5 | StratifiedKFold |

**评估**（stdout 打印）：

* CV AUC ± std（5-fold）
* train + holdout 各自的 precision/recall/F1 @ thresholds [0.30, 0.50, 0.70, 0.85]
* full feature importance

**上线门槛**：

* CV AUC ≥ 0.85
* Holdout AUC 比 CV mean 跌 ≤ 0.02（否则过拟）
* precision @ 0.70 ≥ 0.80（不然误杀太多）

### 4.2 行为 LSTM

```bash
python train_lstm.py \
    --input ./data/out/behavior_train.parquet \
    --output-dir ./out/lstm \
    --epochs 40 --batch-size 256 --lr 1e-3
```

**网络结构**（跟 `internal/mlscore/BEHAVIOR_LSTM.md` 严格对齐）：

```
mouse  (B, 200, 3) → LSTM(64, 2-layer, dropout=0.3) → last_h ┐
                                                              ├→ concat(128) → Dense(64,ReLU) → Dense(1)
key    (B,  50, 3) → LSTM(64, 2-layer, dropout=0.3) → last_h ┘
```

**输入预处理**（serving / training 必须一致）：

| 序列 | shape | 归一化 |
|------|-------|--------|
| 鼠标 | (1, 200, 3) | x/screen_w, y/screen_h, t_ms/session_ms → clamp[0,1] |
| 按键 | (1, 50, 3)  | keycode/255, dwell/1000ms, flight/2000ms → clamp[0,1] |

* 不足 max_len **左 pad 0**（LSTM 末态看最近活动）
* 超长截尾保留**最近** max_len 点

**损失**：BCEWithLogitsLoss + `pos_weight=n_neg/n_pos`

**早停**：val_auc 5 个 epoch 没提升 → break。

**上线门槛**：val AUC ≥ 0.88（来自 BEHAVIOR_LSTM.md §2）。

### 4.3 产物清单

GBDT 跑完 `./out/gbdt/`：

```
model.txt              # LightGBM native（serving fallback / debug）
model.onnx             # 生产 serving（OnnxService 拉这个）
model.shap.json        # base_value + 全局 importance + per-feature baseline mean
                       #   → internal/mlscore/explain.go 拿来生成 SHAP-like 解释
feature_order.json     # 严格列顺序，admin reload 必传
metadata.json          # model_ver, model_type, feature_order
metrics.json           # CV AUC、precision@k、feature_importance
```

LSTM 跑完 `./out/lstm/`：

```
behavior_lstm.onnx     # 生产 serving（BehaviorLSTMService 拉）
best.pt                # PyTorch checkpoint（rerun export 用）
metadata.json          # input shapes + normalization constants + hyperparams
metrics.json           # 训练 history、best epoch、val AUC
```

---

## 5. 部署

### 5.1 上传 S3

```bash
# dry-run（先看计划）
python deploy/upload_artifact.py \
    --artifact-dir ./out/gbdt --model-type gbdt --dry-run

# 真上传 + staging reload（challenger 槽）
python deploy/upload_artifact.py \
    --artifact-dir ./out/gbdt --model-type gbdt \
    --s3-bucket risk-models-prod \
    --admin-url http://risk-staging:9590 \
    --admin-token "$RISK_STAGING_TOKEN"
```

**命名约定**：`<type>_v<YYYYMMDD>_<gitsha>`，例：`gbdt_v20260601_a1b2c3d4`

S3 layout：

```
s3://risk-models-prod/
└── gbdt/
    └── gbdt_v20260601_a1b2c3d4/
        ├── model.onnx
        ├── model.shap.json
        └── metadata.json
```

`upload_artifact.py` 上传完会调 `POST /admin/ml/onnx/reload`（`internal/mlscore/onnx_admin.go`），
risk-manage 拉 S3 文件 → atomic.Pointer hot-swap session。

### 5.2 灰度（必走流程）

1. **staging shadow 1 周**：challenger 跟 champion 并行打分，不影响线上决策。
   监控 `/admin/ml/abtest/report`：precision/recall delta 收敛。
2. **prod 5%**：
   ```bash
   curl -X POST http://risk-prod:9590/admin/ml/rollout/start \
       -H "Authorization: Bearer $RISK_PROD_TOKEN" \
       -d '{"initial_pct":5,"stages":[25,50,100],"auto_advance":false}'
   ```
3. **观察 24h**：dashboards/risk-ml/precision-drift 没红线 → 推 25%
4. **逐步推进**：25% → 50% → 100%，每档观察 24h
5. **回滚**：任意阶段 `POST /admin/ml/rollout/abort`，原子切回 champion

### 5.3 Airflow 自动化

每周一 03:00 UTC 自动触发 `risk_retrain_weekly` DAG，详见
`deploy/airflow/dags/risk_retrain_dag.py`。DAG 把 fetch → train → upload → shadow eval →
auto-promote 串起来，**precision/recall regression > 2% 拒上线 + Slack 告警**。

PSI > 0.25 critical 也会触发 `risk_retrain_on_drift` DAG（同样的工作流，但 manual approve）。

---

## 6. 性能 baseline

| 工作量 | GBDT | LSTM |
|--------|------|------|
| 训练 10k 样本 | < 5 分钟 | < 30 分钟（CPU）|
| 训练 100k 样本 | < 30 分钟 | < 4 小时（CPU）|
| 推理 / 笔 | < 5 ms | < 10 ms |
| ONNX 模型 size | ~ 500 KB | ~ 800 KB |

> 推理时延来自 `onnxruntime` CPU 单线程；prod CPU 8 核会更快。
> 训练超过上述基线 2× 要去查 — 通常是 numpy 装错版本（拖 lightgbm 慢 10×）。

---

## 7. 模型版本约定

```
<model_type>_v<YYYYMMDD>_<gitsha>
    gbdt_v20260601_a1b2c3d4
    lstm_v20260601_a1b2c3d4
```

* `YYYYMMDD` = UTC 训练日期
* `gitsha` = `ml-training/` 当前目录的 git short SHA（让模型可追溯到代码）
* 同一天多次重训 → 把 build 编号附 suffix：`gbdt_v20260601_a1b2c3d4_1`

---

## 8. 故障排查

| 现象 | 排查 |
|------|------|
| `pip install lightgbm` 报 OpenMP 错（macOS） | `brew install libomp` |
| `pip install torch` 慢 | 用 `--index-url https://download.pytorch.org/whl/cpu` |
| `clickhouse_driver.errors.NetworkError` | 检查 VPN / `CLICKHOUSE_DSN`；用 `--fallback-sample` 应急 |
| `botocore.exceptions.ClientError: 403 AccessDenied` | 检查 `AWS_*` 凭证 + IAM `s3:PutObject` 给 `risk-models-prod/*` 权限 |
| `onnx.checker.ValidationError: Op type X not in opset 15` | 升级 onnxmltools / 降 lightgbm；或 `--target-opset 17` |
| `ValueError: Found input variables with inconsistent numbers` | encoder.transform 在 holdout 上见到新分类值 — 检查 `--holdout-days` 是否合理 |
| Admin reload 返 400 `input dim mismatch` | feature_order 跟 ONNX 模型 input shape 不一致 — 检查 `feature_order.json`，确认上传到 admin 的 list 长度 == model.onnx 的 [None, N] 中的 N |
| Admin reload 返 503 `rebuild with -tags onnx` | risk-manage server 没用 onnx build tag — `go build -tags onnx ./cmd/server`（详见 `internal/mlscore/ONNX_PIPELINE.md`）|
| LSTM val AUC < 0.7 收敛慢 | 检查归一化是否一致；坐标越界 clamp 没生效；lr 调到 5e-4 重试 |
| SHAP 跑爆内存 | 已经按 `sample 1000` 限流；如果还爆，把 `train_gbdt.py:export_shap` 的 1000 调到 500 |

---

## 9. 跟其他文档的关系

* **`cmd/retrain/main.go`** —— Go 端 baseline logistic regression。不动；保留作为 dev / fallback / 客户没 Python 环境时的紧急训练工具。**正式训请用本目录。**
* **`internal/mlscore/ONNX_PIPELINE.md`** —— Go serving 侧 ONNX 集成 + admin endpoint 设计文档。本 pipeline 产 `.onnx` → 对应那边消费。
* **`internal/mlscore/BEHAVIOR_LSTM.md`** —— LSTM 输入预处理 + 模型架构权威文档，本 pipeline 的 `train_lstm.py` 严格遵守。
* **`internal/mlscore/EXPLAINABILITY.md`** —— `model.shap.json` schema + Go 侧解释生成。
* **`deploy/airflow/dags/risk_retrain_dag.py`** —— Airflow 调度本目录的脚本，prod 周期重训走这条。

---

## 10. 已知限制 / TODO

* 本目录的脚本在没装 ML deps 的 CI 上跑会失败 — `requirements.txt` 必须先装。建议加一个 `tests/test_smoke.py` 用 mock numpy array 跑 fetch + 训练全链路。
* LSTM 真训样本（行为数据）目前来自 web-SDK 上报，覆盖率 ~ 30%；mobile-SDK 同等上报通道还在排期，到位前 LSTM 只对 web checkout 流量启用。
* SHAP 全局 importance 是抽样 1000 算的；千万行级数据要换成 streaming reservoir 算法。
* upload_artifact.py 没做幂等检查：同 version 重传会覆盖。如果生产怕误覆盖，加 `--no-overwrite` flag（boto3 head_object 先查）。
