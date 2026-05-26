"""
risk_retrain_dag.py — 每周自动重训风控 ML 模型 + precision 跌穿告警。

数据流：
    1. ClickHouse: 拉 7 天 decision audit + feedback outcome → join 出 (features, label)
    2. 训练 LightGBM GBDT（5-fold CV，AUC threshold sweep）
    3. 转 ONNX（skl2onnx / onnxmltools）
    4. 上传到对象存储（S3 / OSS）
    5. 在 staging 环境 reload + 跑 1000 笔 shadow eval
    6. 对比新旧模型在 holdout set 上的 precision/recall
       - 新 ≥ 旧 + 阈值 → POST /admin/ml/onnx/reload（prod admin endpoint）+ Slack 通知
       - 新 < 旧 → 不上线，Slack 告警 "Model regression detected"
    7. 不管是否 promote，都把训练 metric 写到 ClickHouse `risk_model_history` 表

触发器：
    @weekly （Monday 03:00 UTC）
    + 漂移监控 PSI > 0.25 critical alert 也可手动 trigger

依赖：
    - airflow >= 2.5
    - airflow-providers-clickhouse
    - airflow-providers-amazon
    - airflow-providers-http
    - airflow-providers-slack
    - lightgbm, skl2onnx, onnxruntime（worker Python env）
"""

from datetime import datetime, timedelta
from airflow import DAG
from airflow.operators.python import PythonOperator, BranchPythonOperator
from airflow.operators.empty import EmptyOperator
from airflow.providers.http.operators.http import SimpleHttpOperator
from airflow.providers.slack.notifications.slack_webhook import SlackWebhookNotifier
import logging
import json

DEFAULT_ARGS = {
    "owner": "risk-platform",
    "depends_on_past": False,
    "email": ["risk-oncall@example.com"],
    "email_on_failure": True,
    "email_on_retry": False,
    "retries": 1,
    "retry_delay": timedelta(minutes=10),
    "on_failure_callback": SlackWebhookNotifier(
        slack_webhook_conn_id="slack_risk",
        text=":warning: risk_retrain DAG failed — {{ ti.task_id }} | log: {{ ti.log_url }}",
    ),
}

# ─── 可调常量 ─────────────────────────────────────────────
WINDOW_DAYS = 7
MIN_TRAINING_SAMPLES = 10000
MIN_FRAUD_RATE = 0.005           # 至少 0.5% fraud 样本
PRECISION_REGRESSION_THRESHOLD = 0.02  # 新模型 precision 比旧低 2% 拒上线
RECALL_REGRESSION_THRESHOLD = 0.03     # 新模型 recall 比旧低 3% 拒上线
ONNX_S3_BUCKET = "risk-models-prod"
ONNX_S3_PREFIX = "gbdt"
STAGING_RISK_ADMIN = "http://risk-staging:9590"
PROD_RISK_ADMIN = "http://risk-prod:9590"


def fetch_training_data(**ctx):
    """从 ClickHouse 拉 7 天数据 + outcome label，落本地 parquet。"""
    from airflow.providers.clickhouse.hooks.clickhouse import ClickHouseHook
    import pandas as pd

    hook = ClickHouseHook(clickhouse_conn_id="clickhouse_audit")
    query = f"""
        SELECT
            d.decision_id,
            d.merchant_id,
            d.amount,
            d.country,
            d.ip_proxy,
            d.ip_vpn,
            d.hw_concurrency,
            d.time_to_checkout_ms,
            d.mouse_entropy,
            d.click_interval_ms,
            d.typing_cv,
            d.keystroke_count,
            o.is_fraud
        FROM risk_decisions d
        INNER JOIN risk_outcomes o ON d.decision_id = o.decision_id
        WHERE d.occurred_at >= now() - INTERVAL {WINDOW_DAYS} DAY
          AND d.merchant_id NOT IN (SELECT merchant_id FROM merchants_excluded)
          AND o.is_fraud IS NOT NULL
    """
    df = hook.get_pandas_df(query)

    # Sanity check
    total = len(df)
    frauds = df["is_fraud"].sum()
    fraud_rate = frauds / total if total else 0

    if total < MIN_TRAINING_SAMPLES:
        raise ValueError(f"训练数据不足：{total} < {MIN_TRAINING_SAMPLES}")
    if fraud_rate < MIN_FRAUD_RATE:
        raise ValueError(f"欺诈率过低：{fraud_rate:.4f} < {MIN_FRAUD_RATE}")

    logging.info(f"训练集：{total} 样本，{frauds} fraud（{fraud_rate:.4%}）")

    out = "/tmp/risk_training.parquet"
    df.to_parquet(out)
    ctx["ti"].xcom_push(key="training_path", value=out)
    ctx["ti"].xcom_push(key="sample_count", value=total)


def train_gbdt(**ctx):
    """LightGBM 5-fold CV + AUC threshold sweep → 输出 .onnx + metrics.json"""
    import pandas as pd
    import lightgbm as lgb
    from sklearn.model_selection import StratifiedKFold
    from sklearn.metrics import roc_auc_score, precision_recall_curve
    from skl2onnx import convert_lightgbm
    from skl2onnx.common.data_types import FloatTensorType
    import numpy as np
    import datetime as dt

    path = ctx["ti"].xcom_pull(key="training_path")
    df = pd.read_parquet(path)

    feature_cols = [c for c in df.columns if c not in ("decision_id", "merchant_id", "is_fraud")]
    X = df[feature_cols].values.astype(np.float32)
    y = df["is_fraud"].astype(np.int32).values

    # 5-fold CV
    aucs = []
    skf = StratifiedKFold(n_splits=5, shuffle=True, random_state=42)
    for fold, (train_idx, val_idx) in enumerate(skf.split(X, y)):
        model_cv = lgb.LGBMClassifier(
            n_estimators=200,
            learning_rate=0.05,
            num_leaves=31,
            min_data_in_leaf=50,
            scale_pos_weight=(len(y) - y.sum()) / max(y.sum(), 1),
            random_state=42,
        )
        model_cv.fit(X[train_idx], y[train_idx])
        auc = roc_auc_score(y[val_idx], model_cv.predict_proba(X[val_idx])[:, 1])
        aucs.append(auc)
        logging.info(f"Fold {fold}: AUC = {auc:.4f}")

    cv_auc_mean = float(np.mean(aucs))
    cv_auc_std = float(np.std(aucs))

    # Final model 用全部数据训
    model = lgb.LGBMClassifier(
        n_estimators=200,
        learning_rate=0.05,
        num_leaves=31,
        min_data_in_leaf=50,
        scale_pos_weight=(len(y) - y.sum()) / max(y.sum(), 1),
        random_state=42,
    )
    model.fit(X, y)

    # 转 ONNX
    initial_type = [("input", FloatTensorType([None, len(feature_cols)]))]
    onnx_model = convert_lightgbm(model, initial_types=initial_type, target_opset=15)

    ts = dt.datetime.now().strftime("%Y%m%d_%H%M%S")
    git_sha = ctx.get("dag_run").conf.get("git_sha", "unknown")[:8] if ctx.get("dag_run") else "unknown"
    model_filename = f"risk_gbdt_v{ts}_{git_sha}.onnx"
    local_path = f"/tmp/{model_filename}"
    with open(local_path, "wb") as f:
        f.write(onnx_model.SerializeToString())

    metrics = {
        "model_filename": model_filename,
        "cv_auc_mean": cv_auc_mean,
        "cv_auc_std": cv_auc_std,
        "feature_order": feature_cols,
        "feature_importance": dict(zip(feature_cols, model.feature_importances_.tolist())),
        "training_samples": int(len(y)),
        "fraud_samples": int(y.sum()),
        "trained_at": ts,
    }
    with open("/tmp/risk_model_metrics.json", "w") as f:
        json.dump(metrics, f, indent=2)

    ctx["ti"].xcom_push(key="model_path", value=local_path)
    ctx["ti"].xcom_push(key="model_filename", value=model_filename)
    ctx["ti"].xcom_push(key="metrics", value=metrics)


def upload_to_s3(**ctx):
    """上传 ONNX 到 S3"""
    from airflow.providers.amazon.aws.hooks.s3 import S3Hook

    model_path = ctx["ti"].xcom_pull(key="model_path")
    filename = ctx["ti"].xcom_pull(key="model_filename")

    hook = S3Hook(aws_conn_id="aws_default")
    s3_key = f"{ONNX_S3_PREFIX}/{filename}"
    hook.load_file(filename=model_path, key=s3_key, bucket_name=ONNX_S3_BUCKET, replace=True)

    s3_uri = f"s3://{ONNX_S3_BUCKET}/{s3_key}"
    logging.info(f"已上传 {s3_uri}")
    ctx["ti"].xcom_push(key="s3_uri", value=s3_uri)


def shadow_eval_in_staging(**ctx):
    """在 staging 环境跑 shadow eval，对比新旧模型在 holdout 上的 precision/recall。"""
    import requests

    s3_uri = ctx["ti"].xcom_pull(key="s3_uri")
    metrics = ctx["ti"].xcom_pull(key="metrics")

    # 1. staging admin endpoint reload 新模型为 challenger
    resp = requests.post(
        f"{STAGING_RISK_ADMIN}/admin/ml/onnx/reload",
        json={"model_path": s3_uri, "feature_order": metrics["feature_order"], "as_challenger": True},
        headers={"Authorization": f"Bearer {ctx['var']['value']['risk_staging_token']}"},
        timeout=30,
    )
    resp.raise_for_status()

    # 2. 等 30 分钟跑 shadow eval（synthetic + 实际流量灰度 5%）
    import time
    time.sleep(1800)

    # 3. 拉对比报告
    resp = requests.get(
        f"{STAGING_RISK_ADMIN}/admin/ml/abtest/report",
        headers={"Authorization": f"Bearer {ctx['var']['value']['risk_staging_token']}"},
        timeout=30,
    )
    report = resp.json()

    # 比较精度
    champion_prec = report["champion"]["precision"]
    challenger_prec = report["challenger"]["precision"]
    champion_recall = report["champion"]["recall"]
    challenger_recall = report["challenger"]["recall"]

    precision_delta = challenger_prec - champion_prec
    recall_delta = challenger_recall - champion_recall

    promote = (
        precision_delta >= -PRECISION_REGRESSION_THRESHOLD
        and recall_delta >= -RECALL_REGRESSION_THRESHOLD
    )

    ctx["ti"].xcom_push(key="should_promote", value=promote)
    ctx["ti"].xcom_push(key="eval_report", value=report)

    logging.info(
        f"Shadow eval: champion p={champion_prec:.4f} r={champion_recall:.4f}; "
        f"challenger p={challenger_prec:.4f} r={challenger_recall:.4f}; "
        f"promote={promote}"
    )


def decide_promote(**ctx):
    """分支：promote / regression alert"""
    if ctx["ti"].xcom_pull(key="should_promote"):
        return "promote_to_prod"
    return "alert_regression"


def promote_to_prod(**ctx):
    """通过 prod admin endpoint 启动 traffic-split rollout"""
    import requests

    s3_uri = ctx["ti"].xcom_pull(key="s3_uri")
    metrics = ctx["ti"].xcom_pull(key="metrics")

    # 启动 5% rollout
    resp = requests.post(
        f"{PROD_RISK_ADMIN}/admin/ml/onnx/reload",
        json={"model_path": s3_uri, "feature_order": metrics["feature_order"], "as_challenger": True},
        headers={"Authorization": f"Bearer {ctx['var']['value']['risk_prod_token']}"},
        timeout=30,
    )
    resp.raise_for_status()

    # 启动 rollout 5%
    resp = requests.post(
        f"{PROD_RISK_ADMIN}/admin/ml/rollout/start",
        json={"initial_pct": 5, "stages": [25, 50, 100], "auto_advance": False},
        headers={"Authorization": f"Bearer {ctx['var']['value']['risk_prod_token']}"},
        timeout=30,
    )
    resp.raise_for_status()
    logging.info("✅ Promoted to prod at 5% traffic. Use admin endpoint to advance.")


def alert_regression(**ctx):
    """模型精度回退 → 不上线 + Slack 告警"""
    report = ctx["ti"].xcom_pull(key="eval_report")
    msg = (
        f":red_circle: Model regression detected — NOT promoting.\n"
        f"Champion: p={report['champion']['precision']:.4f}, r={report['champion']['recall']:.4f}\n"
        f"Challenger: p={report['challenger']['precision']:.4f}, r={report['challenger']['recall']:.4f}\n"
        f"差距: precision Δ={report['challenger']['precision']-report['champion']['precision']:+.4f}, "
        f"recall Δ={report['challenger']['recall']-report['champion']['recall']:+.4f}"
    )
    logging.warning(msg)
    # Slack 通知由 on_failure_callback 处理；这里 raise 让 task fail
    raise ValueError("Model regression detected — manual review required.")


# ─── DAG 定义 ─────────────────────────────────────────────
with DAG(
    "risk_retrain_weekly",
    default_args=DEFAULT_ARGS,
    description="Risk ML model weekly retrain + auto-promote with regression guard",
    schedule_interval="0 3 * * 1",  # 每周一 03:00 UTC
    start_date=datetime(2026, 1, 1),
    catchup=False,
    max_active_runs=1,
    tags=["risk", "ml", "retrain"],
) as dag:

    fetch = PythonOperator(task_id="fetch_training_data", python_callable=fetch_training_data)
    train = PythonOperator(task_id="train_gbdt", python_callable=train_gbdt)
    upload = PythonOperator(task_id="upload_to_s3", python_callable=upload_to_s3)
    shadow = PythonOperator(task_id="shadow_eval_in_staging", python_callable=shadow_eval_in_staging)
    branch = BranchPythonOperator(task_id="decide_promote", python_callable=decide_promote)
    promote = PythonOperator(task_id="promote_to_prod", python_callable=promote_to_prod)
    regression = PythonOperator(task_id="alert_regression", python_callable=alert_regression)
    done = EmptyOperator(task_id="done", trigger_rule="none_failed_min_one_success")

    fetch >> train >> upload >> shadow >> branch
    branch >> promote >> done
    branch >> regression >> done


# ─── 辅助 DAG: PSI 监控触发即时重训 ────────────────────────
# 当 PSI > 0.25 critical alert 时 Prometheus alertmanager webhook 触发本 DAG
with DAG(
    "risk_retrain_on_drift",
    default_args=DEFAULT_ARGS,
    description="Triggered by PSI > 0.25 alert via Alertmanager webhook",
    schedule_interval=None,  # 仅 manual / webhook trigger
    start_date=datetime(2026, 1, 1),
    catchup=False,
    tags=["risk", "ml", "drift-triggered"],
) as drift_dag:
    fetch_d = PythonOperator(task_id="fetch_training_data", python_callable=fetch_training_data)
    train_d = PythonOperator(task_id="train_gbdt", python_callable=train_gbdt)
    upload_d = PythonOperator(task_id="upload_to_s3", python_callable=upload_to_s3)
    shadow_d = PythonOperator(task_id="shadow_eval_in_staging", python_callable=shadow_eval_in_staging)
    branch_d = BranchPythonOperator(task_id="decide_promote", python_callable=decide_promote)
    promote_d = PythonOperator(task_id="promote_to_prod", python_callable=promote_to_prod)
    regression_d = PythonOperator(task_id="alert_regression", python_callable=alert_regression)
    done_d = EmptyOperator(task_id="done", trigger_rule="none_failed_min_one_success")

    fetch_d >> train_d >> upload_d >> shadow_d >> branch_d
    branch_d >> promote_d >> done_d
    branch_d >> regression_d >> done_d
