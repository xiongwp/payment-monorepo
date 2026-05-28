#!/usr/bin/env python3
"""
gen_sample.py —— 给 dev / staging Feast 灌 5 份 sample parquet。

用法：
    python gen_sample.py /data/parquet

每份 10K 行；entity key 用 `<prefix>_<i>` 形式（cust_0 .. cust_9999）。
event_timestamp 平均分布在最近 7 天，覆盖 ttl 窗口。

特征值分布刻意贴近真实风控场景：
  - 90d paid count：长尾，mean ~5，少数 >100 老客户
  - chargeback：稀疏，95% 客户 0 次
  - asn_score：bimodal（普通住宅 ~0.1，VPN / 数据中心 ~0.8）
  - merchant fraud rate：log-normal，多数 <1%，长尾到 10%+

idempotent：parquet 已存在则跳过（dev 反复重启不重写）。
"""
import os
import sys
from datetime import datetime, timedelta, timezone

import numpy as np
import pandas as pd

N = 10_000
SEED = 42
WINDOW_DAYS = 7


def random_timestamps(n: int, rng: np.random.Generator) -> pd.Series:
    """最近 WINDOW_DAYS 天均匀采样 + 加 UTC 时区。"""
    now = datetime.now(timezone.utc)
    offsets = rng.uniform(0, WINDOW_DAYS * 86400, size=n)
    return pd.Series(
        [now - timedelta(seconds=float(s)) for s in offsets],
        name="event_timestamp",
    )


def gen_customer_velocity(rng: np.random.Generator) -> pd.DataFrame:
    """customer_velocity：paid / chargeback / dispute。"""
    return pd.DataFrame({
        "customer_id":        [f"cust_{i}" for i in range(N)],
        "event_timestamp":    random_timestamps(N, rng),
        # NegativeBinomial 长尾：多数低、少数高频
        "paid_count_90d":     rng.negative_binomial(5, 0.5, N).astype(np.int32),
        # 95% 客户 0 次 chargeback；剩 5% Poisson(2)
        "chargeback_count_90d": np.where(
            rng.random(N) < 0.95, 0, rng.poisson(2, N)
        ).astype(np.int32),
        "dispute_count_30d":  np.where(
            rng.random(N) < 0.93, 0, rng.poisson(1, N)
        ).astype(np.int32),
    })


def gen_device_history(rng: np.random.Generator) -> pd.DataFrame:
    """device_history：first_seen / distinct_customers / simhash neighbors。"""
    return pd.DataFrame({
        "device_id":          [f"dev_{i}" for i in range(N)],
        "event_timestamp":    random_timestamps(N, rng),
        # 设备年龄 0-720 天对数分布
        "first_seen_days":    rng.integers(0, 720, N).astype(np.int32),
        # 90% 设备 1-2 个用户；10% 共享设备（高风险）
        "distinct_customers_90d": np.where(
            rng.random(N) < 0.9,
            rng.integers(1, 3, N),
            rng.integers(3, 20, N),
        ).astype(np.int32),
        # SimHash 邻居：多数 0-2；模拟器集群可能 50+
        "fp_simhash_neighbors": rng.poisson(1.5, N).astype(np.int32),
    })


def gen_ip_risk(rng: np.random.Generator) -> pd.DataFrame:
    """ip_risk：country / proxy / vpn / asn_score。"""
    countries = ["US", "CN", "JP", "DE", "GB", "IN", "BR", "FR", "CA", "AU"]
    return pd.DataFrame({
        "ip":               [f"203.0.{i // 256}.{i % 256}" for i in range(N)],
        "event_timestamp":  random_timestamps(N, rng),
        "ip_country":       rng.choice(countries, N),
        # 5% proxy，3% vpn（部分重叠真实情况）
        "is_proxy":         (rng.random(N) < 0.05).astype(np.int32),
        "is_vpn":           (rng.random(N) < 0.03).astype(np.int32),
        # bimodal：80% 住宅 ~ N(0.1,0.05)，20% 数据中心 ~ N(0.8,0.1)
        "asn_score":        np.where(
            rng.random(N) < 0.8,
            np.clip(rng.normal(0.1, 0.05, N), 0, 1),
            np.clip(rng.normal(0.8, 0.1, N), 0, 1),
        ).astype(np.float32),
    })


def gen_behavior_signals(rng: np.random.Generator) -> pd.DataFrame:
    """behavior_signals：鼠标 / 击键 / 停顿。"""
    return pd.DataFrame({
        "customer_id":          [f"cust_{i}" for i in range(N)],
        "event_timestamp":      random_timestamps(N, rng),
        # 真实用户鼠标速度方差 50-500；bot ~ <10
        "mouse_speed_var":      np.where(
            rng.random(N) < 0.97,
            rng.uniform(50, 500, N),
            rng.uniform(0, 10, N),    # 3% bot 信号
        ).astype(np.float32),
        # 击键 dwell CV：真实 0.2-0.5；bot 接近 0
        "keystroke_dwell_cv":   np.where(
            rng.random(N) < 0.97,
            rng.uniform(0.2, 0.5, N),
            rng.uniform(0, 0.05, N),
        ).astype(np.float32),
        # 停顿次数：Poisson(3) 真人；bot 总是 0
        "pause_count":          np.where(
            rng.random(N) < 0.97,
            rng.poisson(3, N),
            0,
        ).astype(np.int32),
    })


def gen_merchant_aggregates(rng: np.random.Generator) -> pd.DataFrame:
    """merchant_aggregates：商户层风险信号；M < N（只取 200 个商户）。"""
    M = 200
    return pd.DataFrame({
        "merchant_id":              [f"m_{i}" for i in range(M)],
        "event_timestamp":          random_timestamps(M, rng),
        # log-normal：多数商户 < 1%，长尾 5-15% 高风险
        "merchant_30d_fraud_rate":  np.clip(
            rng.lognormal(-5, 1.2, M), 0, 0.5
        ).astype(np.float32),
        # log-normal 金额：median ~$50；长尾大额商户 $5000+
        "merchant_avg_amount":      np.clip(
            rng.lognormal(4, 1.5, M), 5, 50_000
        ).astype(np.float32),
        # 国家熵 0-1
        "merchant_country_mix":     rng.uniform(0, 1, M).astype(np.float32),
    })


def main() -> int:
    out_dir = sys.argv[1] if len(sys.argv) > 1 else "/data/parquet"
    os.makedirs(out_dir, exist_ok=True)
    rng = np.random.default_rng(SEED)

    targets = {
        "customer_velocity":   gen_customer_velocity,
        "device_history":      gen_device_history,
        "ip_risk":             gen_ip_risk,
        "behavior_signals":    gen_behavior_signals,
        "merchant_aggregates": gen_merchant_aggregates,
    }
    for name, fn in targets.items():
        path = os.path.join(out_dir, f"{name}.parquet")
        if os.path.exists(path):
            print(f"[skip] {path} already exists")
            continue
        df = fn(rng)
        df.to_parquet(path, index=False, compression="snappy")
        print(f"[write] {path}  rows={len(df)}  cols={list(df.columns)}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
