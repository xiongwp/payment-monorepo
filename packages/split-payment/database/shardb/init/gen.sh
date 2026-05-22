#!/usr/bin/env bash
# ============================================================================
# 生成 10 个 shard init SQL：split_payment_db_0.sql .. split_payment_db_9.sql
#
# 用法：./gen.sh
#
# 输出：N_init.sql (N=0..9)
# 每个 N_init.sql 创建 split_payment_db_N 库 + 80 张子表（8 families × 10 表/库）。
# 表名：moneyflow_runs_NN, transfers_NN, ... 其中 NN = N*10 .. N*10+9。
# ============================================================================

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEMPLATE="${HERE}/_template.sql"

# 8 个流水表族 + 各自的 schema (column list)
# 故意把每族 schema 写在 case 里集中维护，改字段一次就行
table_schema() {
  local family="$1"
  local tbl="$2"   # 全名，e.g. transfers_07
  case "${family}" in
    moneyflow_runs)
      cat <<EOF
CREATE TABLE IF NOT EXISTS ${tbl} (
    id              BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    graph_id        BIGINT NOT NULL,
    graph_version   VARCHAR(32),
    trigger_event   VARCHAR(64)  NOT NULL,
    charge_id       VARCHAR(128) DEFAULT NULL,
    merchant_id     VARCHAR(128) DEFAULT NULL,
    amount_minor    BIGINT NOT NULL DEFAULT 0,
    currency        VARCHAR(8)   DEFAULT NULL,
    attributes_json JSON         DEFAULT NULL,
    movements_json  JSON         DEFAULT NULL,
    status          VARCHAR(32)  NOT NULL DEFAULT 'created',
    voucher_no      VARCHAR(64)  DEFAULT NULL,
    error_msg       TEXT         DEFAULT NULL,
    trace_id        VARCHAR(64)  DEFAULT NULL,
    hold_until      DATETIME     DEFAULT NULL,
    hold_released   TINYINT(1)   NOT NULL DEFAULT 0,
    created_at      DATETIME     NOT NULL,
    KEY idx_charge (charge_id),
    KEY idx_graph (graph_id),
    KEY idx_event_created (trigger_event, created_at),
    KEY idx_hold_expired (hold_released, hold_until)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
EOF
      ;;
    transfers)
      cat <<EOF
CREATE TABLE IF NOT EXISTS ${tbl} (
    id VARCHAR(64) NOT NULL PRIMARY KEY,
    transfer_group VARCHAR(64),
    source_account VARCHAR(64) NOT NULL,
    destination_account VARCHAR(64) NOT NULL,
    amount_minor BIGINT NOT NULL,
    currency VARCHAR(8) NOT NULL,
    description VARCHAR(256),
    source_transaction VARCHAR(64),
    application_fee VARCHAR(64),
    status VARCHAR(32) NOT NULL DEFAULT 'created',
    reversed_amount BIGINT NOT NULL DEFAULT 0,
    graph_run_id BIGINT,
    idempotency_key VARCHAR(128),
    metadata_json JSON,
    created_at DATETIME NOT NULL,
    posted_at DATETIME,
    UNIQUE KEY uk_idem (idempotency_key),
    KEY idx_group (transfer_group),
    KEY idx_src_tx (source_transaction),
    KEY idx_dest (destination_account),
    KEY idx_run (graph_run_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
EOF
      ;;
    application_fees)
      cat <<EOF
CREATE TABLE IF NOT EXISTS ${tbl} (
    id VARCHAR(64) NOT NULL PRIMARY KEY,
    charge VARCHAR(64) NOT NULL,
    account VARCHAR(64),
    amount_minor BIGINT NOT NULL,
    currency VARCHAR(8) NOT NULL,
    status VARCHAR(32) NOT NULL DEFAULT 'pending',
    refunded_amount BIGINT NOT NULL DEFAULT 0,
    graph_run_id BIGINT,
    idempotency_key VARCHAR(128),
    metadata_json JSON,
    created_at DATETIME NOT NULL,
    UNIQUE KEY uk_idem (idempotency_key),
    KEY idx_charge (charge),
    KEY idx_account (account)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
EOF
      ;;
    payouts)
      cat <<EOF
CREATE TABLE IF NOT EXISTS ${tbl} (
    id VARCHAR(64) NOT NULL PRIMARY KEY,
    account VARCHAR(64) NOT NULL,
    amount_minor BIGINT NOT NULL,
    currency VARCHAR(8) NOT NULL,
    method VARCHAR(16) NOT NULL DEFAULT 'standard',
    status VARCHAR(32) NOT NULL DEFAULT 'pending',
    arrival_date DATETIME,
    failure_code VARCHAR(64),
    failure_message TEXT,
    statement_descriptor VARCHAR(128),
    destination_json JSON,
    graph_run_id BIGINT,
    idempotency_key VARCHAR(128),
    metadata_json JSON,
    created_at DATETIME NOT NULL,
    UNIQUE KEY uk_idem (idempotency_key),
    KEY idx_account (account),
    KEY idx_status_arrival (status, arrival_date)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
EOF
      ;;
    reversals)
      cat <<EOF
CREATE TABLE IF NOT EXISTS ${tbl} (
    id VARCHAR(64) NOT NULL PRIMARY KEY,
    transfer VARCHAR(64) NOT NULL,
    amount_minor BIGINT NOT NULL,
    currency VARCHAR(8) NOT NULL,
    reason VARCHAR(64),
    status VARCHAR(32) NOT NULL DEFAULT 'pending',
    failure_message TEXT,
    idempotency_key VARCHAR(128),
    graph_run_id BIGINT,
    metadata_json JSON,
    created_at DATETIME NOT NULL,
    UNIQUE KEY uk_idem (idempotency_key),
    KEY idx_transfer (transfer)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
EOF
      ;;
    moneyflow_sagas)
      cat <<EOF
CREATE TABLE IF NOT EXISTS ${tbl} (
    saga_id        VARCHAR(64) NOT NULL PRIMARY KEY,
    graph_run_id   BIGINT,
    correlation_id VARCHAR(128),
    state          VARCHAR(32) NOT NULL,
    current_step   INT NOT NULL DEFAULT 0,
    steps_json     JSON NOT NULL,
    started_at     DATETIME NOT NULL,
    completed_at   DATETIME,
    updated_at     DATETIME NOT NULL,
    KEY idx_state (state),
    KEY idx_run (graph_run_id),
    KEY idx_started (started_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
EOF
      ;;
    event_outbox)
      cat <<EOF
CREATE TABLE IF NOT EXISTS ${tbl} (
    id            BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    event_type    VARCHAR(64) NOT NULL,
    payload_json  JSON NOT NULL,
    status        VARCHAR(32) NOT NULL DEFAULT 'pending',
    retry_count   INT NOT NULL DEFAULT 0,
    max_retry     INT NOT NULL DEFAULT 10,
    last_error    TEXT,
    next_retry_at DATETIME NOT NULL,
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    KEY idx_status_next (status, next_retry_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
EOF
      ;;
    reversal_retry_outbox)
      cat <<EOF
CREATE TABLE IF NOT EXISTS ${tbl} (
    id              BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    reversal_id     VARCHAR(64) NOT NULL,
    transfer_id     VARCHAR(64) NOT NULL,
    delta_minor     BIGINT NOT NULL,
    status          VARCHAR(32) NOT NULL DEFAULT 'pending',
    retry_count     INT NOT NULL DEFAULT 0,
    max_retry       INT NOT NULL DEFAULT 5,
    last_error      TEXT,
    next_retry_at   DATETIME NOT NULL,
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    UNIQUE KEY uk_reversal_id (reversal_id),
    KEY idx_status_next (status, next_retry_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
EOF
      ;;
    *)
      echo "ERROR: unknown family ${family}" >&2
      exit 1
      ;;
  esac
}

FAMILIES=(moneyflow_runs transfers application_fees payouts reversals moneyflow_sagas event_outbox reversal_retry_outbox)

for dbidx in 0 1 2 3 4 5 6 7 8 9; do
  outfile="${HERE}/${dbidx}_init.sql"
  echo ">>> 生成 ${outfile}"

  # 每个 db 含 10 张子表/族：db_N 拥有 _{N*10}..{N*10+9}
  # 每张子表同时生成 _shadow 镜像表（全链路压测影子流量用）
  tables_block=""
  for family in "${FAMILIES[@]}"; do
    tables_block+="-- ─── ${family} (10 张主表 + 10 张 _shadow 镜像) ───────────────────────"$'\n\n'
    for off in 0 1 2 3 4 5 6 7 8 9; do
      tbl_idx=$((dbidx * 10 + off))
      tbl_name=$(printf "${family}_%02d" "${tbl_idx}")
      tables_block+="$(table_schema "${family}" "${tbl_name}")"$'\n\n'
      # shadow 镜像表：schema 完全一样，名字加 _shadow
      shadow_name="${tbl_name}_shadow"
      tables_block+="$(table_schema "${family}" "${shadow_name}")"$'\n\n'
    done
  done

  # 写出文件：替换 _template.sql 的 {DBIDX} 和 {TABLES_BLOCK}
  awk -v dbidx="${dbidx}" -v tblblock="${tables_block}" '
    { gsub(/\{DBIDX\}/, dbidx); gsub(/\{TABLES_BLOCK\}/, tblblock); print }
  ' "${TEMPLATE}" > "${outfile}"
done

echo ">>> 生成完毕：$(ls "${HERE}"/[0-9]_init.sql | wc -l) 个文件"
echo ">>> 抽检 0_init.sql 前 30 行："
head -30 "${HERE}/0_init.sql"
echo "..."
echo ">>> 总行数：$(wc -l "${HERE}"/[0-9]_init.sql | tail -1)"
