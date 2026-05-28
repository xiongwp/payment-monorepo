"""
ml-training/deploy/upload_artifact.py
=====================================

把 train_gbdt.py / train_lstm.py 产的 out/ 目录上传 S3，然后通过 risk-manage
admin endpoint 让 staging（或 prod challenger slot）拉新模型。

命名约定
--------
    <model_type>_v<YYYYMMDD>_<gitsha>.<ext>
        gbdt_v20260601_a1b2c3d4.onnx
        gbdt_v20260601_a1b2c3d4.shap.json
        gbdt_v20260601_a1b2c3d4.metadata.json
        lstm_v20260601_a1b2c3d4.onnx

S3 layout：
    s3://<bucket>/<model_type>/<version>/
        ├── model.onnx
        ├── model.shap.json    （gbdt 才有）
        └── metadata.json

用法
----
    # dry-run（不真上传，只打印计划）
    python upload_artifact.py --artifact-dir ./out/gbdt --model-type gbdt --dry-run

    # 真上传 + 触发 staging reload
    python upload_artifact.py \\
        --artifact-dir ./out/gbdt \\
        --model-type gbdt \\
        --s3-bucket risk-models-prod \\
        --admin-url http://risk-staging:9590 \\
        --admin-token "$RISK_STAGING_TOKEN"
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

logger = logging.getLogger("upload_artifact")


def git_sha() -> str:
    try:
        return subprocess.check_output(
            ["git", "rev-parse", "--short", "HEAD"], stderr=subprocess.DEVNULL
        ).decode().strip() or "nogit"
    except Exception:
        return "nogit"


def build_version(model_type: str, override: str = "") -> str:
    if override:
        return override
    return f"{model_type}_v{datetime.now(timezone.utc).strftime('%Y%m%d')}_{git_sha()}"


def discover_artifacts(artifact_dir: Path, model_type: str) -> dict[str, Path]:
    """artifact_dir 必须有 model.onnx + metadata.json；gbdt 额外有 model.shap.json"""
    out = {}
    onnx_candidates = list(artifact_dir.glob("*.onnx"))
    if not onnx_candidates:
        raise FileNotFoundError(f"no .onnx in {artifact_dir}")
    out["model.onnx"] = onnx_candidates[0]

    for opt in ("metadata.json", "model.shap.json", "feature_order.json", "metrics.json"):
        p = artifact_dir / opt
        if p.exists():
            out[opt] = p
    if "metadata.json" not in out:
        logger.warning("metadata.json 缺失，新建一个最简版")
    return out


def upload_to_s3(version: str, model_type: str, bucket: str, artifacts: dict[str, Path],
                 dry_run: bool) -> str:
    """上传每个 artifact 到 s3://bucket/<model_type>/<version>/...

    返回 model.onnx 的 s3 URI（admin reload 要这个）。
    """
    prefix = f"{model_type}/{version}"
    onnx_uri = f"s3://{bucket}/{prefix}/model.onnx"

    if dry_run:
        logger.info("[DRY-RUN] would upload:")
        for name, path in artifacts.items():
            logger.info("  s3://%s/%s/%s  ←  %s  (%.1f KB)",
                        bucket, prefix, name, path, path.stat().st_size / 1024)
        return onnx_uri

    import boto3
    s3 = boto3.client("s3")
    for name, path in artifacts.items():
        key = f"{prefix}/{name}"
        logger.info("uploading %s → s3://%s/%s", path, bucket, key)
        s3.upload_file(str(path), bucket, key,
                       ExtraArgs={"ServerSideEncryption": "AES256"})
    logger.info("upload complete: %s", onnx_uri)
    return onnx_uri


def call_admin_reload(admin_url: str, token: str, onnx_uri: str,
                      feature_order: list[str], as_challenger: bool,
                      dry_run: bool) -> dict:
    """POST <admin>/admin/ml/onnx/reload — 触发 risk-manage 拉新 ONNX。

    payload 跟 internal/mlscore/onnx_admin.go OnnxReloadRequest 对齐：
        {"model_path": "s3://...", "feature_order": [...], "as_challenger": bool}
    """
    if not admin_url:
        logger.info("--admin-url 留空，跳过 reload 调用")
        return {}
    if dry_run:
        logger.info("[DRY-RUN] would POST %s/admin/ml/onnx/reload  model_path=%s as_challenger=%s",
                    admin_url, onnx_uri, as_challenger)
        return {"dry_run": True}

    import requests
    headers = {}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    resp = requests.post(
        f"{admin_url.rstrip('/')}/admin/ml/onnx/reload",
        json={"model_path": onnx_uri, "feature_order": feature_order,
              "as_challenger": as_challenger},
        headers=headers,
        timeout=30,
    )
    if resp.status_code >= 400:
        logger.error("admin reload failed: %d %s", resp.status_code, resp.text)
        resp.raise_for_status()
    logger.info("admin reload OK: %s", resp.text[:200])
    return resp.json() if resp.text else {}


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--artifact-dir", required=True, help="train_*.py 的 --output-dir")
    ap.add_argument("--model-type", required=True, choices=["gbdt", "lstm"])
    ap.add_argument("--version", default="", help="留空自动 <type>_v<YYYYMMDD>_<gitsha>")
    ap.add_argument("--s3-bucket", default=os.environ.get("RISK_MODELS_BUCKET", "risk-models-prod"))
    ap.add_argument("--admin-url", default=os.environ.get("RISK_ADMIN_URL", ""),
                    help="留空跳过 reload；典型：http://risk-staging:9590")
    ap.add_argument("--admin-token", default=os.environ.get("RISK_ADMIN_TOKEN", ""))
    ap.add_argument("--as-challenger", action="store_true", default=True,
                    help="作为 challenger 槽加载（默认 True，prod 必须 True）")
    ap.add_argument("--dry-run", action="store_true",
                    help="不真上传、不调 admin，只打印计划")
    ap.add_argument("-v", "--verbose", action="store_true")
    args = ap.parse_args()

    logging.basicConfig(level=logging.DEBUG if args.verbose else logging.INFO,
                        format="%(asctime)s %(levelname)s %(message)s")

    artifact_dir = Path(args.artifact_dir)
    if not artifact_dir.exists():
        logger.error("artifact dir not found: %s", artifact_dir)
        sys.exit(2)

    version = build_version(args.model_type, args.version)
    logger.info("version = %s", version)

    artifacts = discover_artifacts(artifact_dir, args.model_type)
    logger.info("found %d artifacts: %s", len(artifacts), list(artifacts.keys()))

    # 读 feature_order（gbdt 必须有；lstm 不需要 — onnx_admin /reload 体里仍传空 list）
    feature_order = []
    if "feature_order.json" in artifacts:
        with open(artifacts["feature_order.json"]) as f:
            feature_order = json.load(f).get("feature_order", [])
    elif "metadata.json" in artifacts:
        with open(artifacts["metadata.json"]) as f:
            feature_order = json.load(f).get("feature_order", [])

    onnx_uri = upload_to_s3(version, args.model_type, args.s3_bucket, artifacts, args.dry_run)

    call_admin_reload(args.admin_url, args.admin_token, onnx_uri,
                      feature_order, args.as_challenger, args.dry_run)

    print(json.dumps({
        "version": version,
        "model_type": args.model_type,
        "s3_uri": onnx_uri,
        "dry_run": args.dry_run,
        "n_features": len(feature_order),
    }, indent=2))


if __name__ == "__main__":
    main()
