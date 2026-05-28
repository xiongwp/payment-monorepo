"""
ml-training/data/fetch_training_data.py
=======================================

从 ClickHouse `risk_decisions` JOIN `risk_outcomes` 拉最近 90 天数据，
做 time-based train / holdout 切分（最后 7d 进 holdout，防 leakage）。

用法
----
    # 默认 90d，最后 7d 做 holdout
    python fetch_training_data.py --out-dir ./data --window-days 90 --holdout-days 7

    # CH 连不上 → 用本地 sample.parquet fallback（dev / CI 环境）
    python fetch_training_data.py --out-dir ./data --fallback-sample ./sample.parquet

输出
----
    <out-dir>/train.parquet     # 最近 (window - holdout) 天
    <out-dir>/holdout.parquet   # 最后 holdout-days 天
    <out-dir>/meta.json         # 行数 / fraud rate / 时间窗口

特征列表（feature_order 同步给 internal/mlscore/OnnxService）
----------------------------------------------------------
数值特征（14）：
    amount, hw_concurrency, time_to_checkout_ms, mouse_entropy,
    click_interval_ms, typing_cv, keystroke_count, velocity_1h,
    velocity_24h, distinct_ips_24h, bin_fraud_rate,
    geo_distance_km, account_age_days, recent_disputes_30d

分类特征（6，会 one-hot 展开成多列）：
    country, device_type, card_brand, channel, browser, os_family

label：
    is_fraud ∈ {0, 1}（来自 chargeback / manual review feedback）
"""

from __future__ import annotations

import argparse
import json
import logging
import os
import sys
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Optional

import pandas as pd

# ── 列定义（跟 cmd/retrain Go 端 + OnnxService feature_order 严格对齐）──
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
TIME_COL = "occurred_at"

logger = logging.getLogger("fetch_training_data")


def _build_query(window_days: int) -> str:
    """构造 CH 查询；feature_order 必须跟 NUMERIC + CATEGORICAL 一致。"""
    cols = ", ".join(["d.decision_id", "d.occurred_at"]
                     + [f"d.{c}" for c in NUMERIC_FEATURES]
                     + [f"d.{c}" for c in CATEGORICAL_FEATURES]
                     + ["o.is_fraud"])
    return f"""
        SELECT {cols}
        FROM risk_decisions d
        INNER JOIN risk_outcomes o USING (decision_id)
        WHERE d.occurred_at >= now() - INTERVAL {window_days} DAY
          AND o.is_fraud IS NOT NULL
          AND d.merchant_id NOT IN (SELECT merchant_id FROM merchants_excluded)
        ORDER BY d.occurred_at ASC
    """


def fetch_from_clickhouse(window_days: int, ch_dsn: str) -> pd.DataFrame:
    """从 CH 拉数据。失败抛异常给上层 catch 做 fallback。"""
    from clickhouse_driver import Client  # 局部 import：fallback 路径不需要它装上
    logger.info("connecting to ClickHouse: %s", _redact(ch_dsn))
    client = Client.from_url(ch_dsn)
    query = _build_query(window_days)
    rows, cols = client.execute(query, with_column_types=True)
    df = pd.DataFrame(rows, columns=[c[0] for c in cols])
    logger.info("fetched %d rows from CH", len(df))
    return df


def load_fallback(path: str) -> pd.DataFrame:
    """dev 环境 / CI 跑：本地 sample.parquet。"""
    logger.warning("FALLBACK: loading local sample from %s", path)
    if not os.path.exists(path):
        raise FileNotFoundError(
            f"fallback sample not found: {path}. "
            f"Run notebooks/EDA_TEMPLATE.md 'generate synthetic sample' cell first."
        )
    return pd.read_parquet(path)


def time_split(df: pd.DataFrame, holdout_days: int) -> tuple[pd.DataFrame, pd.DataFrame]:
    """time-based 切分；df 必须已按 occurred_at 升序。"""
    if df.empty:
        return df, df
    if TIME_COL not in df.columns:
        # fallback sample 可能没时间列：按行号切
        cut = int(len(df) * (1 - holdout_days / 90))
        return df.iloc[:cut].reset_index(drop=True), df.iloc[cut:].reset_index(drop=True)

    cutoff = df[TIME_COL].max() - timedelta(days=holdout_days)
    train = df[df[TIME_COL] < cutoff].reset_index(drop=True)
    holdout = df[df[TIME_COL] >= cutoff].reset_index(drop=True)
    return train, holdout


def write_meta(out_dir: Path, train: pd.DataFrame, holdout: pd.DataFrame, window_days: int):
    meta = {
        "generated_at": datetime.now(timezone.utc).isoformat(),
        "window_days": window_days,
        "train_rows": len(train),
        "holdout_rows": len(holdout),
        "train_fraud_rate": float(train[LABEL_COL].mean()) if len(train) else 0.0,
        "holdout_fraud_rate": float(holdout[LABEL_COL].mean()) if len(holdout) else 0.0,
        "numeric_features": NUMERIC_FEATURES,
        "categorical_features": CATEGORICAL_FEATURES,
    }
    with open(out_dir / "meta.json", "w") as f:
        json.dump(meta, f, indent=2)
    logger.info("meta written: train=%d (fraud=%.3f%%), holdout=%d (fraud=%.3f%%)",
                meta["train_rows"], meta["train_fraud_rate"] * 100,
                meta["holdout_rows"], meta["holdout_fraud_rate"] * 100)


def _redact(dsn: str) -> str:
    """log 里把 password 抹掉"""
    import re
    return re.sub(r"://([^:]+):([^@]+)@", r"://\1:***@", dsn)


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--out-dir", default="./data/out", help="输出目录")
    parser.add_argument("--window-days", type=int, default=90, help="窗口长度（天）")
    parser.add_argument("--holdout-days", type=int, default=7, help="末尾几天做 holdout")
    parser.add_argument("--ch-dsn", default=os.environ.get("CLICKHOUSE_DSN", ""),
                        help="ClickHouse DSN（默认读 $CLICKHOUSE_DSN）")
    parser.add_argument("--fallback-sample", default="./sample.parquet",
                        help="CH 连不上时的 fallback parquet 路径")
    parser.add_argument("-v", "--verbose", action="store_true")
    args = parser.parse_args()

    logging.basicConfig(level=logging.DEBUG if args.verbose else logging.INFO,
                        format="%(asctime)s %(levelname)s %(message)s")

    out_dir = Path(args.out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)

    df: Optional[pd.DataFrame] = None
    if args.ch_dsn:
        try:
            df = fetch_from_clickhouse(args.window_days, args.ch_dsn)
        except Exception as e:
            logger.error("CH fetch failed: %s — falling back", e)

    if df is None or df.empty:
        df = load_fallback(args.fallback_sample)

    if df.empty:
        logger.error("no data fetched / loaded — exiting")
        sys.exit(2)

    train, holdout = time_split(df, args.holdout_days)
    train.to_parquet(out_dir / "train.parquet", index=False)
    holdout.to_parquet(out_dir / "holdout.parquet", index=False)
    write_meta(out_dir, train, holdout, args.window_days)
    logger.info("done. files in %s", out_dir)


if __name__ == "__main__":
    main()
