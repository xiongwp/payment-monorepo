#!/usr/bin/env bash
# ============================================================================
# 生成 10 个 shard init SQL：0_init.sql .. 9_init.sql
#
# DB-split Batch 7 极简版（split-payment 不再存业务账本，只存事件触发流水）：
#   - 唯一 family: moneyflow_event_NN（按 hash(event_id) % 100 分片）
#   - 每个 shard 含 10 张主表（NN = dbIdx*10 .. dbIdx*10+9）+ 10 张 _shadow 镜像
#
# 调用：./gen.sh
# ============================================================================

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEMPLATE="${HERE}/_template.sql"

# moneyflow_event：split-payment 唯一事件流水表
#
# 字段设计原则：
#   - 通用执行状态（status / retry / error）— 让事件机制可重放、可重试
#   - graph_id + event_id 主路由 + 唯一约束（同一事件不重复触发）
#   - trigger_payload_json: 原始 event attributes
#   - plan_json: translator 翻译产物（debits/credits 列表）
#   - accounting_voucher_no: accounting 返回的凭证号（可对账）
#   - hold_until / hold_released: marketplace 类延迟结算场景预留
#
# 注：split-payment 不再维护 transfers / payouts / fees / reversals 等业务账本
#    —— 这些 accounting-system 已有完整双分录账本。
table_schema() {
  local tbl="$1"
  cat <<EOF
CREATE TABLE IF NOT EXISTS ${tbl} (
    id                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,

    -- 路由 + 幂等 key
    event_id              VARCHAR(128) NOT NULL COMMENT '业务事件唯一 ID (charge_id / external event id)',
    graph_id              BIGINT       NOT NULL COMMENT '关联 moneyflow_graphs.id',
    graph_version         VARCHAR(32)  DEFAULT NULL,
    graph_key             VARCHAR(128) DEFAULT NULL COMMENT '冗余存 graph_key 便于查询',

    -- 业务上下文
    trigger_event         VARCHAR(64)  NOT NULL COMMENT 'graph 内的 event 节点名 (e.g. charge.succeeded)',
    merchant_id           VARCHAR(128) DEFAULT NULL,
    amount_minor          BIGINT       NOT NULL DEFAULT 0,
    currency              VARCHAR(8)   DEFAULT NULL,

    -- 输入 / 翻译产物
    trigger_payload_json  JSON         DEFAULT NULL COMMENT 'event attrs 原始 payload',
    plan_json             JSON         DEFAULT NULL COMMENT 'translator 翻译产物 (账户操作 leg 列表)',

    -- 执行状态机
    status                VARCHAR(32)  NOT NULL DEFAULT 'pending'
                          COMMENT 'pending / translating / booking / success / failed / cancelled',
    retry_count           INT          NOT NULL DEFAULT 0,
    max_retry             INT          NOT NULL DEFAULT 5,
    next_retry_at         DATETIME     DEFAULT NULL,
    error_msg             TEXT         DEFAULT NULL,

    -- 输出（accounting 返回）
    accounting_voucher_no VARCHAR(64)  DEFAULT NULL COMMENT 'accounting 落账后的凭证号',
    trace_id              VARCHAR(64)  DEFAULT NULL,

    -- marketplace / hold-period 场景（可选）
    hold_until            DATETIME     DEFAULT NULL,
    hold_released         TINYINT(1)   NOT NULL DEFAULT 0,

    -- 时间戳
    created_at            DATETIME     NOT NULL,
    updated_at            DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    completed_at          DATETIME     DEFAULT NULL,

    UNIQUE KEY uk_event_id (event_id),
    KEY idx_graph (graph_id),
    KEY idx_status_next (status, next_retry_at),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
EOF
}

for dbidx in 0 1 2 3 4 5 6 7 8 9; do
  outfile="${HERE}/${dbidx}_init.sql"
  echo ">>> 生成 ${outfile}"

  # 每个 db 含 10 张子表：moneyflow_event_{dbidx*10}..{dbidx*10+9}
  # 每张子表同时生成 _shadow 镜像表（全链路压测影子流量用）
  tables_block="-- ─── moneyflow_event (10 张主表 + 10 张 _shadow 镜像) ──────────────────────"$'\n\n'
  for off in 0 1 2 3 4 5 6 7 8 9; do
    tbl_idx=$((dbidx * 10 + off))
    tbl_name=$(printf "moneyflow_event_%02d" "${tbl_idx}")
    tables_block+="$(table_schema "${tbl_name}")"$'\n\n'
    # shadow 镜像表
    shadow_name="${tbl_name}_shadow"
    tables_block+="$(table_schema "${shadow_name}")"$'\n\n'
  done

  # 用 bash 拼装替代 awk（awk 不支持多行字符串）
  before_block=$(sed -n '1,/{TABLES_BLOCK}/p' "${TEMPLATE}" | sed '$d')
  after_block=$(sed -n '/{TABLES_BLOCK}/,$p' "${TEMPLATE}" | sed '1d')

  {
    echo "${before_block}" | sed "s/{DBIDX}/${dbidx}/g"
    printf "%s" "${tables_block}"
    echo "${after_block}" | sed "s/{DBIDX}/${dbidx}/g"
  } > "${outfile}"
done

echo ">>> 生成完毕：$(ls "${HERE}"/[0-9]_init.sql | wc -l) 个文件"
echo ">>> 0_init.sql:"
echo "    行数: $(wc -l < "${HERE}/0_init.sql")"
echo "    CREATE USER: $(grep -c "CREATE USER IF NOT EXISTS 'split_user'" "${HERE}/0_init.sql")"
echo "    CREATE TABLE: $(grep -c 'CREATE TABLE IF NOT EXISTS' "${HERE}/0_init.sql") (期望 20 = 10 主表 + 10 shadow)"
