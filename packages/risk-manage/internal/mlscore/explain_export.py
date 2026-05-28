"""explain_export.py — 训练侧产 SHAP base value + feature contribution lookup table。

读者：ML 团队（Python 训练侧）。

用法（典型 LightGBM 流程）：

    from explain_export import export_shap_metadata
    import lightgbm as lgb

    model = lgb.LGBMClassifier(...).fit(X_train, y_train)
    export_shap_metadata(
        model=model,
        X_train=X_train,                 # pd.DataFrame，columns = feature 名
        out_path="model_v20260201.shap.json",
        model_version="gbdt_v20260201",
    )

输出 schema（跟 Go 侧 mlscore.shapMetadata 对齐）：

    {
      "base_value": 0.123,
      "model_version": "gbdt_v20260201",
      "feature_baseline_mean": {"amount": 1500.5, "ip_proxy": 0.05, ...},
      "feature_global_importance": {"amount": 0.18, "ip_proxy": 1.42, ...}
    }

跟 .onnx 文件同目录、同前缀（model_xxx.onnx ↔ model_xxx.shap.json）；
S3 上传时一起上去，Go 侧 OnnxShapExplainer 加载 .onnx 时旁路读 .shap.json。

依赖：
    pip install shap lightgbm pandas numpy

为什么用 TreeExplainer 而不是 KernelExplainer：
  - GBDT (LightGBM / XGBoost) 是树模型 → TreeExplainer 是精确的 Shapley
    值（满足 efficiency + symmetry + dummy + additivity 公理）
  - KernelExplainer 是模型无关近似，慢 100x +，且对树模型不必要
  - 输出的 `shap_values` shape = (n_samples, n_features)；我们对所有
    train 样本做平均算 global importance（= mean(|shap_value|)）

为什么导 global importance 而不是 per-sample shap_values：
  - Go 推理侧每秒处理 N 笔决策；不可能每次都跑 Python shap
  - 简化策略：把 global mean importance × (sample_value - baseline_mean)
    作为 deterministic attribution（详见 explain.go OnnxShapExplainer）
  - 真严格 Shapley 需要把整棵 forest 翻译给 Go 侧或起独立 shap-service
    （详见 EXPLAINABILITY.md "后续" 段落）
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any


def export_shap_metadata(
    model: Any,
    X_train: Any,  # pd.DataFrame
    out_path: str,
    model_version: str,
    background_size: int = 1000,
) -> dict:
    """跑 TreeExplainer 算 base value + per-feature global importance，落 JSON。

    Args:
        model: 训练好的 sklearn-compatible 树模型（LightGBM / XGBoost / sklearn GBDT）
        X_train: 训练集 DataFrame（columns 是 feature 名，跟 Go 侧 featureValueMap 对齐）
        out_path: 输出 .shap.json 路径（约定跟 .onnx 同前缀）
        model_version: 模型版本号（跟 ONNX 文件元数据同步）
        background_size: 算 baseline 用的样本数；> 1000 慢，< 100 不稳

    Returns:
        写出的 metadata dict（也作为函数返回值方便单测 / 日志）
    """
    try:
        import shap  # type: ignore
    except ImportError as e:
        raise RuntimeError(
            "shap library required: pip install shap"
        ) from e
    import numpy as np

    # background 子样本 — shap.TreeExplainer 用整个 train set 也行但慢；
    # 1000 样本对 base_value 估计已经足够稳。
    if hasattr(X_train, "sample") and len(X_train) > background_size:
        background = X_train.sample(n=background_size, random_state=42)
    else:
        background = X_train

    explainer = shap.TreeExplainer(model)
    shap_values = explainer.shap_values(background)

    # 二分类时 shap 0.42+ 返一个 ndarray（log-odds 空间）；旧版本返
    # [class0_shap, class1_shap]。统一取正类（fraud=1）的 shap。
    if isinstance(shap_values, list) and len(shap_values) == 2:
        shap_values = shap_values[1]

    # base_value 形状 / 类型同上 — 旧版本是 list[2]，新版本是 scalar。
    expected = explainer.expected_value
    if isinstance(expected, (list, np.ndarray)):
        try:
            base_value = float(expected[1])
        except (IndexError, TypeError):
            base_value = float(np.asarray(expected).item())
    else:
        base_value = float(expected)

    # global importance = mean(|shap_value|) per column，跨样本平均
    # 体现"这个特征平均贡献多少 logit"
    abs_shap = np.abs(shap_values)
    global_importance = abs_shap.mean(axis=0)

    feature_names = list(X_train.columns)
    baseline_mean = X_train.mean(numeric_only=True).to_dict()

    metadata = {
        "base_value": base_value,
        "model_version": model_version,
        "feature_baseline_mean": {
            k: float(v) for k, v in baseline_mean.items()
        },
        "feature_global_importance": {
            feature_names[i]: float(global_importance[i])
            for i in range(len(feature_names))
        },
    }

    out_file = Path(out_path)
    out_file.parent.mkdir(parents=True, exist_ok=True)
    with out_file.open("w", encoding="utf-8") as f:
        json.dump(metadata, f, indent=2, sort_keys=True)

    return metadata


if __name__ == "__main__":
    import argparse
    import pickle

    parser = argparse.ArgumentParser(description="Export SHAP metadata for ONNX GBDT model")
    parser.add_argument("--model", required=True, help="Path to pickled trained model")
    parser.add_argument("--train-csv", required=True, help="Path to training data CSV")
    parser.add_argument("--out", required=True, help="Output .shap.json path")
    parser.add_argument("--model-version", required=True)
    parser.add_argument("--background-size", type=int, default=1000)
    args = parser.parse_args()

    import pandas as pd  # type: ignore

    with open(args.model, "rb") as f:
        model = pickle.load(f)
    X_train = pd.read_csv(args.train_csv)

    meta = export_shap_metadata(
        model=model,
        X_train=X_train,
        out_path=args.out,
        model_version=args.model_version,
        background_size=args.background_size,
    )
    print(f"wrote {args.out}: base_value={meta['base_value']:.4f}, "
          f"features={len(meta['feature_global_importance'])}")
