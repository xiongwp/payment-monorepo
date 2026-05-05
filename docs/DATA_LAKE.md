# MySQL → 数据湖：CDC 接入设计

把 monorepo 内所有业务 MySQL 的数据增量同步到数据湖（默认 S3 + Apache Iceberg），
供报表 / 风控建模 / BI / 长期存档使用。

## 1. 为什么要做

| 问题 | 现状 | 目标 |
|---|---|---|
| 报表查询占 prod DB | 长期 Grafana / 财务跑大查询直打 prod MySQL，慢查询拖业务 | 报表全打数据湖，prod 只服务事务流量 |
| 长期存档 | 软删除 + retention 真删后数据没了，合规要求 5-10 年留存 | 数据湖按月分区永存，prod 只存近 1 年热数据 |
| 跨服务 join | order-core PI ↔ accounting gl_entry 跨库 join 在 prod 不可行（不同 DB instance） | 数据湖里全在一个 catalog，SQL join 直接跑 |
| ML / 风控建模 | 没数据集 | 数据湖 = 模型训练集来源 |

## 2. 架构

```
┌──────────────────────────────────────────────────────────────────────┐
│ Source MySQL（11 实例）                                                │
│   shared-meta：order_meta / accounting_meta / paychan_meta /          │
│                user_merchant_meta                                      │
│   shared-shard-0..9：order_db_N / accounting_db_N / paychan_db_N /    │
│                      user_merchant_db_N                                │
└──────────────────────────────────────────────────────────────────────┘
                            │ MySQL binlog (ROW format, FULL image)
                            ↓
┌──────────────────────────────────────────────────────────────────────┐
│ Debezium CDC Connectors（Kafka Connect on K8s）                        │
│   - 每个 MySQL 实例一个 connector，11 个                               │
│   - 输出到 Kafka topic：cdc.<dbname>.<tablename>                       │
│   - 每行一个 record：{op:c|u|d, before:{...}, after:{...}, ts_ms}     │
└──────────────────────────────────────────────────────────────────────┘
                            │ Kafka topic
                            ↓
┌──────────────────────────────────────────────────────────────────────┐
│ Sanitizer + Router (Flink / Spark Streaming)                          │
│   - PII 脱敏（card_number / email / phone / id_card 按规则替换）       │
│   - 高敏字段（cvv / track_data）整字段 drop                           │
│   - 按表的 sensitivity 级别决定写入哪个 lake 区                        │
│   - shadow 表：x.shadow.<table>_NN_shadow → 单独 namespace 不混        │
└──────────────────────────────────────────────────────────────────────┘
                            │
                            ↓
┌──────────────────────────────────────────────────────────────────────┐
│ Lake Storage（S3 + Apache Iceberg）                                   │
│   - bucket: pay-lake                                                   │
│   - partition: dt=YYYY-MM-DD / hr=HH                                   │
│   - format: Parquet（zstd 压缩）+ Iceberg manifest                    │
│   - lifecycle: 热区 1 年（standard）→ 温区 3 年（IA）→ 冷区永久（Glacier）│
└──────────────────────────────────────────────────────────────────────┘
                            │
                            ↓
┌──────────────────────────────────────────────────────────────────────┐
│ Query Engines                                                          │
│   - Trino / Presto：BI 报表，亚秒级交互                                │
│   - Spark：ML 训练 / 大批量 ETL                                        │
│   - Athena (AWS) / BigQuery (GCP)：临时查询                            │
└──────────────────────────────────────────────────────────────────────┘
```

## 3. 工具选型

### 3.1 CDC 引擎：**Debezium**（推荐）

| 候选 | 优 | 劣 |
|---|---|---|
| **Debezium** | 开源、社区大、binlog 解析稳、schema 演进自动跟、跟 Kafka Connect 生态完整 | JVM 占资源、初次接入需要 Kafka Connect 集群 |
| Canal | 国内常见、轻量 | 输出格式自定义、生态小、社区主要中文 |
| Maxwell | 极简 | 不支持 schema 变更、没 transactional consistency 信号 |

**结论：Debezium**。MySQL connector 直接接 binlog，输出 Kafka，schema registry 用 Avro。

### 3.2 流处理：**Flink** > Spark Streaming

| 候选 | 优 | 劣 |
|---|---|---|
| **Flink** | 真正的 streaming（per-event 处理）、有 exactly-once、状态管理强 | 学习曲线陡 |
| Spark Streaming | 团队可能熟、跟批处理同栈 | micro-batch 不是真 streaming、PII 脱敏延迟高 |
| Kafka Streams | 跟 Kafka 同栈、轻量 | JVM-only，跟我们 Go monorepo 风格不一致 |

**结论：Flink**（PyFlink / Java SQL 方式）做脱敏 + 路由。

### 3.3 Lake Format：**Apache Iceberg**

| 候选 | 优 | 劣 |
|---|---|---|
| **Iceberg** | ACID、schema 演进、time travel、跨引擎兼容（Spark/Trino/Flink） | 社区生态相对新 |
| Delta Lake | Databricks 主推、ACID、time travel | 跟 Spark 绑定深 |
| Hudi | upsert 强、近实时 | 复杂、调优难 |

**结论：Iceberg**。CDC 表天然 upsert 模式（Iceberg V2 row-level delete 直接吃）。

### 3.4 Object Storage：**S3** / **OSS**（按云厂商）

按生产部署的云决定。所有 lake format 都支持。

### 3.5 查询：**Trino**

部署 Trino on K8s + Iceberg connector，给 Grafana / Metabase / Redash 接 BI。

## 4. 表 sensitivity 分级 + 脱敏规则

每张业务表按 PII 含量分 3 级：

```yaml
# config/data_lake/table_sensitivity.yaml
tables:
  # P1 高敏：含 PII，必须脱敏后入湖
  - name: user_merchant_db_*.users_*
    level: P1
    drop_columns: [password_hash, totp_secret]
    fake_columns: [email, phone, username]
  - name: user_merchant_db_*.user_auths_*
    level: P1
    drop_columns: [credential]
    fake_columns: [identifier]
  - name: paychan_db_*.acquirer_tx_*
    level: P1
    drop_columns: [request_body, response_body]   # 可能含卡数据
    fake_columns: [external_ref_no]
  - name: paychan_db_*.channel_token_*
    level: P0_BLOCKED                              # 含 PCI 数据 token，整表禁止入湖
    block: true

  # P2 中敏：业务字段，原样入湖
  - name: order_db_*.payment_intent_*
    level: P2
    drop_columns: [client_metadata]
  - name: order_db_*.charge_*
    level: P2
  - name: accounting_db_*.account_transaction_*
    level: P2

  # P3 低敏：聚合 / 字典，无 PII
  - name: user_merchant_meta.merchant_kyc_audit
    level: P3
  - name: order_meta.gl_account
    level: P3

  # 影子表：单独 namespace，永远不入湖（避免压测脏数据混进报表）
  - pattern: '*_shadow'
    block: true
```

**P0_BLOCKED**：包含 PCI 卡数据 token 映射等极敏感的表，**永远不进数据湖**。即便 P0 表也建议
直接在 Debezium connector 的 `column.exclude.list` 里黑掉，连 Kafka 都不进。

## 5. 落地步骤

### Phase 1（2 周）：基建 + 1 张表试水

1. K8s 起 Kafka（3 broker）+ Schema Registry + Kafka Connect 集群
2. 部署 Debezium MySQL connector，监听 `shared-meta` 上的 `order_meta`
3. 跑一张表：`order_meta.webhook_deliveries` 全量 + 增量入湖
4. Trino 连 Iceberg，跑一条 `SELECT count(*) FROM lake.webhook_deliveries`，验证数据一致

成功标准：
- 老数据 1 小时内全量同步
- 新写入 30s 内可在 lake 查到
- count(*) prod 与 lake 一致

### Phase 2（1 月）：所有表入湖 + 脱敏

1. 部署 11 个 MySQL connector（meta + 10 shard）
2. Flink 脱敏作业上线，按 table_sensitivity.yaml 配置
3. 数据湖目录：
   ```
   s3://pay-lake/order/payment_intent/dt=2024-05-05/
   s3://pay-lake/order/charge/dt=2024-05-05/
   s3://pay-lake/accounting/account_transaction/dt=2024-05-05/
   s3://pay-lake/user_merchant/users/dt=2024-05-05/
   ...
   ```
4. 报表团队接 Trino，把当前打 prod 的 dashboard 一个个迁过来
5. SLO 目标：
   - 端到端延迟（binlog → lake 可查）p99 < 60s
   - 完整性：每日对账 prod row count == lake row count，差异 < 0.001%

### Phase 3（3 月）：长期归档 + 合规

1. lifecycle 规则：1 年后转 IA，3 年后转 Glacier
2. 给监管报送 / 合规审计专门的 read-only 账号 + 审计日志
3. 数据保留 7 年（PCI / 当地金融监管）
4. 退役 prod MySQL 上的报表查询账号，强制走 lake

### Phase 4（6 月）：ML / 风控建模

1. 风控团队基于 lake 训练模型
2. 实时风控特征查询走 lake（feature store on lake）

## 6. 业务方代码层改动（极少）

CDC 是数据库层的，业务代码几乎不动。**唯一要做**的两件事：

### 6.1 binlog 配置（DBA / SRE）

每个 MySQL 实例：
```ini
[mysqld]
server-id            = <unique per instance>
log-bin              = mysql-bin
binlog-format        = ROW
binlog-row-image     = FULL
gtid-mode            = ON
enforce-gtid-consistency = ON
expire-logs-days     = 7
```

### 6.2 给 Debezium 一个 read-only 账号 + 权限

```sql
CREATE USER 'debezium'@'%' IDENTIFIED BY '<strong-password>';
GRANT SELECT, RELOAD, SHOW DATABASES, REPLICATION SLAVE, REPLICATION CLIENT
  ON *.* TO 'debezium'@'%';
```

把账号信息存 KMS / Vault，Debezium connector 启动时取。

### 6.3 schema 演进规约

- 加新字段：随便加（CDC 自动追上）
- 删字段：先把字段标 deprecated，180 天后真删（给下游报表 SQL 时间适配）
- 改字段类型：用 view 平滑过渡，不要直接 ALTER

CI 加一条 lint：禁止 `DROP COLUMN` 出现在 init.sql / migrate 文件，必须 admin 手动 review。

## 7. PII 脱敏规则（Flink 作业实现）

跟 `payment-util/replay/sanitize.go` 同模型：

```python
# Flink SQL UDF
@udf(result_type=DataTypes.STRING())
def deterministic_fake(real: str, hmac_key: bytes) -> str:
    return hmac.new(hmac_key, real.encode(), hashlib.sha256).hexdigest()[:16]

@udf(result_type=DataTypes.STRING())
def mask_pan(pan: str) -> str:
    if len(pan) < 12:
        return pan
    return pan[:6] + '*' * (len(pan) - 10) + pan[-4:]
```

部署在 Flink 集群，从配置 yaml 加载脱敏规则。

## 8. 数据湖访问审计

所有 Trino / Spark / Athena 查询：
- 强制走 SSO（Okta / Azure AD）
- 查询日志写入独立 audit bucket
- 含 PII 字段的 schema 标 `sensitive=true`，Trino 拦截器对未授权用户 redact 输出
- 季度审计：高频查询用户 + 大批量下载告警

## 9. 数据湖 vs 数据仓库

本设计选 lake（S3 + Iceberg）不选 warehouse（Redshift / Snowflake / BigQuery）：
- 成本低 10-100x（存储 + 计算分离）
- 跨引擎查询，不被单一 vendor 绑定
- schema-on-read，schema 演进容忍度高
- 同时支持 BI（Trino）+ ML（Spark）两类负载

如果团队已经在用 Snowflake / BigQuery，可以直接把 Iceberg 改成对应 warehouse 的 native table。

## 10. 风险 + 缓解

| 风险 | 缓解 |
|---|---|
| MySQL binlog 量太大压垮网络 | 配 ROW + FULL image 但只订阅必要表（exclude.list 把日志类大表移除） |
| 数据延迟 > SLA | Debezium / Flink 各自的 lag metric 上 dashboard，超 60s 告警 |
| schema 演进打破下游 SQL | Schema Registry 强制 backward-compatible，Flink 作业 deserialize 失败时 dead-letter 而不是阻塞 |
| PII 漏脱敏 | 入湖前 unit test：每张表 sample 100 行，正则扫所有字段，命中 PII pattern 就 fail |
| 影子表数据混进报表 | Sanitizer 看到 `_shadow` 后缀整批 drop，不进 lake |
| 数据湖被未授权访问 | S3 bucket policy 拒绝 non-VPC 访问，IAM 强制 SSO，CloudTrail 审计日志 |
| Debezium 故障导致丢 event | 用 GTID + Kafka exactly-once，重启从上次 offset 继续；同时 daily reconcile 作业算行数差 |

## 11. 时间表

```
Week 1-2   ：Kafka + Connect + Debezium 部署，1 张表跑通
Week 3-6   ：所有 ~150 张表入湖（meta + 11 base × 100 shard 算法上 1100，
            按 base 而非 shard 入湖逻辑表 = 11+ 张）
Week 7-10  ：Flink 脱敏作业 + 表 sensitivity 配置 + PII 校验
Week 11-12 ：Trino 部署 + 报表迁移 1 个 BI dashboard
Month 4-6  ：所有报表迁完，prod MySQL 报表账号下线
Month 6+   ：风控 / ML 团队接入
```

总投入：1 个数据工程师 6 个月主项目 + 0.5 SRE 配套（运维 Kafka / Flink）。
