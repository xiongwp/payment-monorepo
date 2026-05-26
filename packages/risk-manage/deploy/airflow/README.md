# Airflow Retrain Pipeline — risk-manage

每周自动重训 GBDT 模型 + 漂移触发重训 + 自动 promote / 回退告警。

## DAG 列表

| DAG | 触发 | 用途 |
|---|---|---|
| `risk_retrain_weekly` | `@weekly`（周一 03:00 UTC）| 计划内重训，shadow eval → 5% rollout |
| `risk_retrain_on_drift` | Alertmanager webhook | PSI > 0.25 critical 时即时重训 |

## 数据流

```
ClickHouse        Airflow worker         S3              risk-manage prod
─────────       ────────────────       ─────             ─────────────────
risk_decisions    fetch_training_data
risk_outcomes  →  train_gbdt (LightGBM 5-fold CV)
                  upload_to_s3       →  s3://risk-models-prod/gbdt/risk_gbdt_v20260201_a1b2c3d4.onnx
                  shadow_eval_in_staging
                          │
                          ├─ promote: POST /admin/ml/onnx/reload + rollout 5%
                          └─ regress: Slack alert, NOT promoted
```

## 依赖

### Airflow 集群
- Airflow >= 2.5
- providers: clickhouse, amazon, http, slack

### Worker Python 环境
```bash
pip install lightgbm==4.1.0 \
            scikit-learn==1.4.0 \
            skl2onnx==1.16.0 \
            onnxruntime==1.17.0 \
            pyarrow==15.0.0
```

### Airflow Connections（需在 UI 配）
- `clickhouse_audit` — ClickHouse 集群（读 risk_decisions + risk_outcomes）
- `aws_default` — S3 access key（写 risk-models-prod bucket）
- `slack_risk` — Slack webhook（incident 通知）

### Airflow Variables（需在 UI 配）
- `risk_staging_token` — risk-manage staging admin token
- `risk_prod_token`    — risk-manage prod admin token

## 触发机制

### 计划内（每周）
DAG `risk_retrain_weekly` 自动跑。

### 漂移触发
1. Prometheus alert `RiskDriftPSICritical`（PSI > 0.25 持续 1h）触发
2. Alertmanager 配 webhook 路由：
   ```yaml
   route:
     match:
       alertname: RiskDriftPSICritical
     receiver: airflow_retrain
   receivers:
     - name: airflow_retrain
       webhook_configs:
         - url: https://airflow.example.com/api/v1/dags/risk_retrain_on_drift/dagRuns
           http_config:
             basic_auth:
               username: alertmanager
               password_file: /etc/secrets/airflow_token
   ```
3. DAG `risk_retrain_on_drift` 立刻跑

## Promote 规则

新模型上线条件（要全部满足）：
1. CV AUC ≥ 0.85
2. 训练样本 ≥ 10K
3. 欺诈率 ≥ 0.5%
4. Shadow eval 在 holdout set 上：
   - precision Δ ≥ -2%（不能比 champion 低 2% 以上）
   - recall Δ ≥ -3%

不满足任何一条 → 不 promote + Slack 告警 + 人工 review

## Promote 后的灰度路径

DAG 推 5% → 由人工通过 admin endpoint 推进：
```bash
# 看当前 rollout 状态
curl -H "Authorization: Bearer $TOKEN" \
     http://risk-prod:9590/admin/ml/rollout/status

# 推进到 25%（确认 5% 24h 指标 OK 后）
curl -X POST -H "Authorization: Bearer $TOKEN" \
     http://risk-prod:9590/admin/ml/rollout/advance

# 50%（一周后）
# 100%（再一周）
```

如果任一 stage 上指标跌穿，AdaptiveAutoRollback 会自动回退到上一 stage。

## 故障排查

| 现象 | 检查 |
|---|---|
| `fetch_training_data` 报训练数据不足 | ClickHouse 数据保留期 / outcome join 比例 |
| `train_gbdt` LightGBM OOM | worker 资源 / 减 n_estimators |
| `upload_to_s3` 403 | aws_default connection IAM 权限 |
| `shadow_eval_in_staging` 卡住 30 分钟 | staging 流量是否在跑（synthetic + 灰度） |
| `promote_to_prod` 401 | risk_prod_token 是否过期 |

## TODO

- [ ] 模型版本表 `risk_model_history`（promoted_at / metrics / s3_uri）写 ClickHouse
- [ ] Feature drift 监控融入训练（PSI > 0.1 的特征自动加 alert）
- [ ] Multi-region: 各 region 独立 retrain pipeline，避免跨洋数据移动
- [ ] LLM-assisted feature engineering：自动从 audit 找候选特征
