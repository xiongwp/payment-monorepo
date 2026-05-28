"""
ml-training/train_gbdt.py
=========================

LightGBM GBDT 训练 → ONNX + SHAP artifact。

特性
----
* 5-fold StratifiedKFold CV（fraud-imbalance-safe）
* class_weight 处理欺诈率 <1% 的极度不均衡
* 多阈值评估：[0.30, 0.50, 0.70, 0.85] 的 precision / recall / F1
* 早停：100 round 没提升 → break
* 产物：
    - model.txt        LightGBM native（debug / serving fallback）
    - model.onnx       skl2onnx 转换，跟 internal/mlscore/OnnxService 对齐
    - model.shap.json  TreeExplainer 算的 base_value + 全局 importance + per-feature baseline mean
    - metrics.json     train/holdout AUC、precision@k、CV 指标
    - feature_order.json  按训练时矩阵列顺序，给 admin reload 用

用法
----
    python train_gbdt.py \\
        --input ./data/out/train.parquet \\
        --holdout ./data/out/holdout.parquet \\
        --output-dir ./out/gbdt \\
        --model-ver gbdt_v20260601

CLI flags（全部 optional 默认值合理）：
    --n-estimators 200 --learning-rate 0.05 --num-leaves 31 --min-data-in-leaf 50
    --early-stopping-rounds 100 --random-state 42
"""

from __future__ import annotations

import argparse
import json
import logging
import os
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path

import numpy as np
import pandas as pd

# 局部 import 大依赖：让 --help 不卡
def _lazy_imports():
    import lightgbm as lgb
    from sklearn.model_selection import StratifiedKFold
    from sklearn.metrics import roc_auc_score, precision_score, recall_score, f1_score
    from sklearn.preprocessing import OneHotEncoder
    return lgb, StratifiedKFold, roc_auc_score, precision_score, recall_score, f1_score, OneHotEncoder


# 必须跟 data/fetch_training_data.py 同步
NUMERIC_FEATURES = [
    "amount", "hw_concurrency", "time_to_checkout_ms",
    "mouse_entropy", "click_interval_ms", "typing_cv", "keystroke_count",
    "velocity_1h", "velocity_24h", "distinct_ips_24h", "bin_fraud_rate",
    "geo_distance_km", "account_age_days", "recent_disputes_30d",
]
CATEGORICAL_FEATURES = [
    "country", "device_type", "card_brand", "channel", "browser", "os_family",
]
LABEL_COL = "is_fraud"
EVAL_THRESHOLDS = [0.30, 0.50, 0.70, 0.85]

logger = logging.getLogger("train_gbdt")


def _git_sha() -> str:
    try:
        sha = subprocess.check_output(
            ["git", "rev-parse", "--short", "HEAD"], stderr=subprocess.DEVNULL
        ).decode().strip()
        return sha or "nogit"
    except Exception:
        return "nogit"


def build_feature_matrix(df: pd.DataFrame, encoder=None):
    """数值列直拼 + 分类列 one-hot。返回 (X, feature_order, encoder)。

    encoder 训练时 None → 新建 OneHotEncoder；holdout 时传入 train 训出来的 encoder。
    """
    _, _, _, _, _, _, OneHotEncoder = _lazy_imports()
    cat = df[CATEGORICAL_FEATURES].astype(str).fillna("unknown")
    if encoder is None:
        encoder = OneHotEncoder(handle_unknown="ignore", sparse_output=False, dtype=np.float32)
        cat_enc = encoder.fit_transform(cat)
    else:
        cat_enc = encoder.transform(cat)

    cat_names = []
    for i, col in enumerate(CATEGORICAL_FEATURES):
        for cat_val in encoder.categories_[i]:
            cat_names.append(f"{col}={cat_val}")

    num = df[NUMERIC_FEATURES].astype(np.float32).fillna(0.0).values
    X = np.hstack([num, cat_enc.astype(np.float32)])
    feature_order = list(NUMERIC_FEATURES) + cat_names
    return X, feature_order, encoder


def evaluate_thresholds(y_true: np.ndarray, y_score: np.ndarray) -> dict:
    """multi-threshold precision / recall / F1"""
    _, _, _, precision_score, recall_score, f1_score, _ = _lazy_imports()
    out = {}
    for t in EVAL_THRESHOLDS:
        y_pred = (y_score >= t).astype(int)
        out[f"@{t:.2f}"] = {
            "precision": float(precision_score(y_true, y_pred, zero_division=0)),
            "recall": float(recall_score(y_true, y_pred, zero_division=0)),
            "f1": float(f1_score(y_true, y_pred, zero_division=0)),
            "predicted_positive": int(y_pred.sum()),
        }
    return out


def cv_train(X: np.ndarray, y: np.ndarray, args) -> list[float]:
    """5-fold StratifiedKFold 报告 AUC mean/std"""
    lgb, StratifiedKFold, roc_auc_score, *_ = _lazy_imports()
    skf = StratifiedKFold(n_splits=5, shuffle=True, random_state=args.random_state)
    pos = max(int(y.sum()), 1)
    neg = len(y) - pos
    scale_pos_weight = neg / pos
    aucs = []
    for fold, (ti, vi) in enumerate(skf.split(X, y)):
        m = lgb.LGBMClassifier(
            n_estimators=args.n_estimators,
            learning_rate=args.learning_rate,
            num_leaves=args.num_leaves,
            min_data_in_leaf=args.min_data_in_leaf,
            scale_pos_weight=scale_pos_weight,
            random_state=args.random_state,
            n_jobs=-1,
            verbose=-1,
        )
        m.fit(X[ti], y[ti],
              eval_set=[(X[vi], y[vi])],
              callbacks=[lgb.early_stopping(args.early_stopping_rounds, verbose=False)])
        auc = roc_auc_score(y[vi], m.predict_proba(X[vi])[:, 1])
        aucs.append(auc)
        logger.info("Fold %d: AUC=%.4f (best_iter=%d)", fold, auc, m.best_iteration_ or m.n_estimators)
    return aucs


def fit_final(X: np.ndarray, y: np.ndarray, args):
    """全量数据训 final model"""
    lgb, *_ = _lazy_imports()
    pos = max(int(y.sum()), 1)
    neg = len(y) - pos
    model = lgb.LGBMClassifier(
        n_estimators=args.n_estimators,
        learning_rate=args.learning_rate,
        num_leaves=args.num_leaves,
        min_data_in_leaf=args.min_data_in_leaf,
        scale_pos_weight=neg / pos,
        random_state=args.random_state,
        n_jobs=-1,
        verbose=-1,
    )
    model.fit(X, y)
    return model


def export_onnx(model, n_features: int, out_path: Path):
    """LightGBM → ONNX（skl2onnx convert_lightgbm）"""
    from skl2onnx import convert_sklearn
    from skl2onnx.common.data_types import FloatTensorType
    from onnxmltools.convert.lightgbm.convert import convert as convert_lightgbm

    initial_type = [("input", FloatTensorType([None, n_features]))]
    try:
        onnx_model = convert_lightgbm(model.booster_, initial_types=initial_type, target_opset=15)
    except TypeError:
        # 老版本签名兼容
        onnx_model = convert_lightgbm(model, initial_types=initial_type, target_opset=15)
    with open(out_path, "wb") as f:
        f.write(onnx_model.SerializeToString())
    logger.info("ONNX written: %s (%.2f KB)", out_path, out_path.stat().st_size / 1024)


def export_shap(model, X: np.ndarray, feature_order: list[str], out_path: Path):
    """SHAP TreeExplainer → base_value + 全局 importance + per-feature baseline mean。

    给 internal/mlscore/explain.go 读出来生成 SHAP-like 解释。
    """
    import shap
    # 抽样 1000 行算 base_value（全量太慢）
    sample = X[np.random.RandomState(42).choice(len(X), size=min(1000, len(X)), replace=False)]
    explainer = shap.TreeExplainer(model)
    sv = explainer.shap_values(sample)
    # LightGBM binary classification: shap_values 可能 [neg, pos] 或者直接 pos
    if isinstance(sv, list):
        sv = sv[1]
    global_importance = np.abs(sv).mean(axis=0).tolist()
    baseline_mean = sample.mean(axis=0).tolist()
    base_value = float(explainer.expected_value if np.isscalar(explainer.expected_value)
                       else explainer.expected_value[-1])

    payload = {
        "base_value": base_value,
        "feature_order": feature_order,
        "global_importance": dict(zip(feature_order, global_importance)),
        "baseline_mean": dict(zip(feature_order, baseline_mean)),
        "n_sample": int(len(sample)),
    }
    with open(out_path, "w") as f:
        json.dump(payload, f, indent=2)
    logger.info("SHAP artifact written: %s", out_path)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--input", required=True, help="train.parquet")
    ap.add_argument("--holdout", default="", help="holdout.parquet（可选）")
    ap.add_argument("--output-dir", required=True)
    ap.add_argument("--model-ver", default="", help="留空自动 gbdt_v<YYYYMMDD>_<gitsha>")
    ap.add_argument("--n-estimators", type=int, default=200)
    ap.add_argument("--learning-rate", type=float, default=0.05)
    ap.add_argument("--num-leaves", type=int, default=31)
    ap.add_argument("--min-data-in-leaf", type=int, default=50)
    ap.add_argument("--early-stopping-rounds", type=int, default=100)
    ap.add_argument("--random-state", type=int, default=42)
    ap.add_argument("-v", "--verbose", action="store_true")
    args = ap.parse_args()

    logging.basicConfig(level=logging.DEBUG if args.verbose else logging.INFO,
                        format="%(asctime)s %(levelname)s %(message)s")

    out_dir = Path(args.output_dir)
    out_dir.mkdir(parents=True, exist_ok=True)

    model_ver = args.model_ver or f"gbdt_v{datetime.now(timezone.utc).strftime('%Y%m%d')}_{_git_sha()}"
    logger.info("model_ver=%s", model_ver)

    # ── load ──────────────────────────────────────────────
    df = pd.read_parquet(args.input)
    logger.info("train df: %d rows", len(df))
    if df[LABEL_COL].nunique() < 2:
        logger.error("training data has only one class — aborting")
        sys.exit(2)

    X, feature_order, encoder = build_feature_matrix(df)
    y = df[LABEL_COL].astype(np.int32).values
    logger.info("X shape=%s, fraud rate=%.4f%%", X.shape, 100 * y.mean())

    # ── CV ────────────────────────────────────────────────
    aucs = cv_train(X, y, args)
    cv_auc_mean = float(np.mean(aucs))
    cv_auc_std = float(np.std(aucs))
    logger.info("CV AUC: %.4f ± %.4f", cv_auc_mean, cv_auc_std)

    # ── final fit on all data ─────────────────────────────
    model = fit_final(X, y, args)
    train_score = model.predict_proba(X)[:, 1]
    train_thr = evaluate_thresholds(y, train_score)
    logger.info("train precision/recall:")
    for k, v in train_thr.items():
        logger.info("  %s  p=%.4f  r=%.4f  f1=%.4f", k, v["precision"], v["recall"], v["f1"])

    # ── holdout eval ──────────────────────────────────────
    holdout_metrics = None
    if args.holdout and os.path.exists(args.holdout):
        _, _, _, _, _, _, _ = _lazy_imports()
        from sklearn.metrics import roc_auc_score
        hdf = pd.read_parquet(args.holdout)
        Xh, _, _ = build_feature_matrix(hdf, encoder=encoder)
        yh = hdf[LABEL_COL].astype(np.int32).values
        if Xh.shape[1] != X.shape[1]:
            logger.warning("holdout feature dim mismatch (%d vs %d) — encoder unseen cats", Xh.shape[1], X.shape[1])
        hscore = model.predict_proba(Xh)[:, 1]
        holdout_metrics = {
            "auc": float(roc_auc_score(yh, hscore)),
            "thresholds": evaluate_thresholds(yh, hscore),
            "rows": int(len(yh)),
            "fraud_rate": float(yh.mean()),
        }
        logger.info("Holdout AUC=%.4f", holdout_metrics["auc"])

    # ── save artifacts ────────────────────────────────────
    model.booster_.save_model(str(out_dir / "model.txt"))
    export_onnx(model, X.shape[1], out_dir / "model.onnx")
    export_shap(model, X, feature_order, out_dir / "model.shap.json")

    with open(out_dir / "feature_order.json", "w") as f:
        json.dump({"feature_order": feature_order, "n_features": len(feature_order)}, f, indent=2)

    metrics = {
        "model_ver": model_ver,
        "trained_at": datetime.now(timezone.utc).isoformat(),
        "train_rows": int(len(y)),
        "fraud_rate": float(y.mean()),
        "cv_auc_mean": cv_auc_mean,
        "cv_auc_std": cv_auc_std,
        "cv_auc_per_fold": aucs,
        "train_thresholds": train_thr,
        "holdout": holdout_metrics,
        "hyperparams": {
            "n_estimators": args.n_estimators,
            "learning_rate": args.learning_rate,
            "num_leaves": args.num_leaves,
            "min_data_in_leaf": args.min_data_in_leaf,
            "early_stopping_rounds": args.early_stopping_rounds,
        },
        "feature_importance": dict(zip(
            feature_order, model.feature_importances_.tolist()
        )),
    }
    with open(out_dir / "metrics.json", "w") as f:
        json.dump(metrics, f, indent=2)
    with open(out_dir / "metadata.json", "w") as f:
        json.dump({"model_ver": model_ver, "model_type": "gbdt",
                   "feature_order": feature_order}, f, indent=2)

    logger.info("done. artifacts in %s", out_dir)
    logger.info("  CV AUC = %.4f ± %.4f", cv_auc_mean, cv_auc_std)
    if holdout_metrics:
        logger.info("  Holdout AUC = %.4f", holdout_metrics["auc"])


if __name__ == "__main__":
    main()
