"""
feature_views.py —— risk-manage 5 个核心 feature view + 4 个 entity 的声明。

Feast 概念回顾：
  - Entity：join key 的语义类型（customer_id / device_id / ip / merchant_id），
            类型化避免不同 view 用同一 string key 但意思不同。
  - DataSource：feature view 的"真值源"。dev 用 FileSource(parquet)，
                prod 切 BigQuerySource / SnowflakeSource。每个 view 必带
                timestamp_field 用于 point-in-time 防穿越 join。
  - FeatureView：一组 schema + ttl + 来源；apply 后注册到 registry，
                materialize 把 offline parquet 推到 online redis。
  - ttl：online cache 失效时间。超过 ttl 没新数据点 → online lookup 返 null
         （我们 Go 侧 fail-soft 兜底 0）。

设计原则：
  - 一个 entity 一个 view：避免 join 时 entity key 组合爆炸。
  - feature 命名 snake_case；带时间窗的带 `_<days>d` 后缀（如 paid_count_90d）。
  - 类型保守用 Float32 / Int32：online store 序列化省空间 + 推理直接 float。
"""
from datetime import timedelta

from feast import (
    Entity,
    FeatureView,
    Field,
    FileSource,
    ValueType,
    FeatureService,
)
from feast.types import Float32, Int32, Int64, String

# ─── Entities ──────────────────────────────────────────────────────────────
# 4 个 join key 类型。description 出现在 feast UI / `feast entities describe`。

customer = Entity(
    name="customer",
    join_keys=["customer_id"],
    value_type=ValueType.STRING,
    description="支付侧 customer，跨 merchant 唯一（payment-orchestrator 分配的 cust_xxx）",
)

device = Entity(
    name="device",
    join_keys=["device_id"],
    value_type=ValueType.STRING,
    description="端 SDK fingerprint hash（FingerprintJS / 自研 canvas+webgl 派生）",
)

ip = Entity(
    name="ip",
    join_keys=["ip"],
    value_type=ValueType.STRING,
    description="请求 IPv4 / IPv6 字符串；anonymize 后入 offline store",
)

merchant = Entity(
    name="merchant",
    join_keys=["merchant_id"],
    value_type=ValueType.STRING,
    description="商户 ID（merchant-service 分配的 m_xxx）",
)

# ─── Data sources ──────────────────────────────────────────────────────────
# dev 用 FileSource 指 /data/parquet。线上换 BigQuerySource(table="..." ）。
# event_timestamp_column 必填——所有 point-in-time join 都按它取最近一条。

customer_source = FileSource(
    name="customer_velocity_src",
    path="/data/parquet/customer_velocity.parquet",
    timestamp_field="event_timestamp",
    description="customer 30d/90d 聚合，ML pipeline 每小时 batch 产出",
)

device_source = FileSource(
    name="device_history_src",
    path="/data/parquet/device_history.parquet",
    timestamp_field="event_timestamp",
)

ip_source = FileSource(
    name="ip_risk_src",
    path="/data/parquet/ip_risk.parquet",
    timestamp_field="event_timestamp",
)

behavior_source = FileSource(
    name="behavior_signals_src",
    path="/data/parquet/behavior_signals.parquet",
    timestamp_field="event_timestamp",
)

merchant_source = FileSource(
    name="merchant_aggregates_src",
    path="/data/parquet/merchant_aggregates.parquet",
    timestamp_field="event_timestamp",
)

# ─── Feature views ─────────────────────────────────────────────────────────
# 5 个 view，覆盖架构图 ML 推理输入的主要风险信号大类。
# ttl 选 1d：超出 1 天没新 batch 写入 → 数据失新鲜，offline 应该补跑。

# 1. customer_velocity —— 客户行为频率（最关键的反欺诈特征家族）
customer_velocity_fv = FeatureView(
    name="customer_velocity",
    entities=[customer],
    ttl=timedelta(days=1),
    schema=[
        Field(name="paid_count_90d",        dtype=Int32),   # 90d 成功支付次数
        Field(name="chargeback_count_90d",  dtype=Int32),   # 90d chargeback 次数（高 → 黑产）
        Field(name="dispute_count_30d",     dtype=Int32),   # 30d dispute 次数（含 cb）
    ],
    source=customer_source,
    tags={"team": "risk-ds", "owner": "fraud-modeling"},
)

# 2. device_history —— 设备指纹相关
device_history_fv = FeatureView(
    name="device_history",
    entities=[device],
    ttl=timedelta(days=1),
    schema=[
        Field(name="first_seen_days",       dtype=Int32),   # 设备首次出现距今天数
        Field(name="distinct_customers_90d", dtype=Int32),  # 90d 关联不同 customer 数（多人共用 → 风险）
        Field(name="fp_simhash_neighbors",  dtype=Int32),   # SimHash 邻居数（指纹簇大小）
    ],
    source=device_source,
    tags={"team": "risk-ds"},
)

# 3. ip_risk —— IP 情报
ip_risk_fv = FeatureView(
    name="ip_risk",
    entities=[ip],
    ttl=timedelta(days=1),
    schema=[
        Field(name="ip_country",  dtype=String),   # ISO-3166 alpha-2，如 "US"
        Field(name="is_proxy",    dtype=Int32),    # 0/1
        Field(name="is_vpn",      dtype=Int32),    # 0/1
        Field(name="asn_score",   dtype=Float32),  # ASN 风险分 0-1（数据中心 / 高滥用网络 → 高）
    ],
    source=ip_source,
    tags={"team": "risk-ds"},
)

# 4. behavior_signals —— 行为生物识别派生
behavior_signals_fv = FeatureView(
    name="behavior_signals",
    entities=[customer],   # 跟 customer join 而非 device（同设备多人时区分）
    ttl=timedelta(days=1),
    schema=[
        Field(name="mouse_speed_var",      dtype=Float32),  # 鼠标速度方差；bot 趋 0
        Field(name="keystroke_dwell_cv",   dtype=Float32),  # 击键停留变异系数
        Field(name="pause_count",          dtype=Int32),    # 停顿次数；bot 常 0
    ],
    source=behavior_source,
    tags={"team": "risk-ds"},
)

# 5. merchant_aggregates —— 商户层风险信号
merchant_aggregates_fv = FeatureView(
    name="merchant_aggregates",
    entities=[merchant],
    ttl=timedelta(days=1),
    schema=[
        Field(name="merchant_30d_fraud_rate", dtype=Float32),  # 30d 欺诈率
        Field(name="merchant_avg_amount",     dtype=Float32),  # 平均订单金额
        Field(name="merchant_country_mix",    dtype=Float32),  # 国家熵 0-1（越分散越高）
    ],
    source=merchant_source,
    tags={"team": "risk-ds"},
)

# ─── Feature service ───────────────────────────────────────────────────────
# Feature service 是给在线推理用的"特征 bundle"——一次 gRPC 拉一组 feature
# 而不是 per-view 拉。risk-manage Go client 用 feature_service="risk_realtime_v1"
# 一次调用拿到所有 5 个 view 的特征。
risk_realtime_v1 = FeatureService(
    name="risk_realtime_v1",
    features=[
        customer_velocity_fv,
        device_history_fv,
        ip_risk_fv,
        behavior_signals_fv,
        merchant_aggregates_fv,
    ],
    description="risk-manage 实时推理用特征集；版本号升级时同步改 mlscore Service",
    tags={"environment": "all", "consumer": "risk-manage"},
)
