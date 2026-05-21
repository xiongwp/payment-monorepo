#!/usr/bin/env python3
"""
Generator for sharded DDL of the rotating-suspense-accounts feature.

Produces:
  packages/accounting-system/database/accountingdb/init/rotation.sql

The output covers 100 shards (10 DBs × 10 tables each):
  - ALTER TABLE account_NN  → add rotation columns (lifecycle_phase / period_* / ...)
  - CREATE TABLE tx_account_anchor_NN

The script is idempotent: re-run produces byte-identical output (modulo header
timestamp). The output uses `USE accounting_db_N;` blocks for clarity and is
safe to re-execute (ALTER uses IF NOT EXISTS, CREATE uses IF NOT EXISTS).

Usage:
    # forward DDL (production tables)
    python3 tools/rotation-migration/gen_sharded_ddl.py \
        > packages/accounting-system/database/accountingdb/init/rotation.sql

    # rollback DDL (production tables)
    python3 tools/rotation-migration/gen_sharded_ddl.py --rollback \
        > packages/accounting-system/database/rollback/rotation_rollback_sharded.sql

    # shadow forward (account_NN_shadow + tx_account_anchor_NN_shadow)
    python3 tools/rotation-migration/gen_sharded_ddl.py --shadow \
        > packages/accounting-system/database/accountingdb/init/rotation_shadow.sql

    # shadow rollback
    python3 tools/rotation-migration/gen_sharded_ddl.py --shadow --rollback \
        > packages/accounting-system/database/rollback/rotation_rollback_shadow_sharded.sql

Shadow tables：
  生产表通过 `CREATE TABLE LIKE` 一次性快照创建。`LIKE` 是快照，主表 ALTER 后
  shadow 不会跟着变。因此每次主表 schema 变更都必须同步 ALTER shadow 表。
  Anchor 表在主侧创建后，shadow 侧用 `LIKE` 直接复制最新 schema 即可。

Design refs:
  - docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §3.3, §3.4
  - packages/accounting-system/internal/domain/model/rotation.go
"""

from textwrap import dedent
import argparse
import sys

ADD_PROCEDURES = dedent("""\
    -- ----------------------------------------------------------------------------
    -- Helper procedure: add column to a table IF NOT EXISTS.
    -- MySQL 8.0+ supports `ALTER TABLE ... ADD COLUMN IF NOT EXISTS` natively in
    -- recent versions, but we use this procedure for broader compatibility
    -- (MySQL 5.7 / older 8.0 minor versions).
    --
    -- Usage: CALL add_col_if_missing('db_name', 'tbl', 'col', 'col_def');
    -- ----------------------------------------------------------------------------
    DROP PROCEDURE IF EXISTS `__rotation_add_col_if_missing`;
    DELIMITER $$
    CREATE PROCEDURE `__rotation_add_col_if_missing`(
        IN p_db   VARCHAR(64),
        IN p_tbl  VARCHAR(64),
        IN p_col  VARCHAR(64),
        IN p_def  TEXT
    )
    BEGIN
        DECLARE col_count INT;
        SELECT COUNT(*) INTO col_count
          FROM INFORMATION_SCHEMA.COLUMNS
         WHERE TABLE_SCHEMA = p_db
           AND TABLE_NAME   = p_tbl
           AND COLUMN_NAME  = p_col;
        IF col_count = 0 THEN
            SET @sql = CONCAT('ALTER TABLE `', p_db, '`.`', p_tbl,
                              '` ADD COLUMN `', p_col, '` ', p_def);
            PREPARE stmt FROM @sql;
            EXECUTE stmt;
            DEALLOCATE PREPARE stmt;
        END IF;
    END$$
    DELIMITER ;

    DROP PROCEDURE IF EXISTS `__rotation_add_idx_if_missing`;
    DELIMITER $$
    CREATE PROCEDURE `__rotation_add_idx_if_missing`(
        IN p_db   VARCHAR(64),
        IN p_tbl  VARCHAR(64),
        IN p_idx  VARCHAR(64),
        IN p_def  TEXT
    )
    BEGIN
        DECLARE idx_count INT;
        SELECT COUNT(*) INTO idx_count
          FROM INFORMATION_SCHEMA.STATISTICS
         WHERE TABLE_SCHEMA = p_db
           AND TABLE_NAME   = p_tbl
           AND INDEX_NAME   = p_idx;
        IF idx_count = 0 THEN
            SET @sql = CONCAT('ALTER TABLE `', p_db, '`.`', p_tbl,
                              '` ADD ', p_def);
            PREPARE stmt FROM @sql;
            EXECUTE stmt;
            DEALLOCATE PREPARE stmt;
        END IF;
    END$$
    DELIMITER ;
""")


DROP_PROCEDURES = dedent("""\
    DROP PROCEDURE IF EXISTS `__rotation_drop_col_if_exists`;
    DELIMITER $$
    CREATE PROCEDURE `__rotation_drop_col_if_exists`(
        IN p_db   VARCHAR(64),
        IN p_tbl  VARCHAR(64),
        IN p_col  VARCHAR(64)
    )
    BEGIN
        DECLARE col_count INT;
        SELECT COUNT(*) INTO col_count
          FROM INFORMATION_SCHEMA.COLUMNS
         WHERE TABLE_SCHEMA = p_db
           AND TABLE_NAME   = p_tbl
           AND COLUMN_NAME  = p_col;
        IF col_count > 0 THEN
            SET @sql = CONCAT('ALTER TABLE `', p_db, '`.`', p_tbl,
                              '` DROP COLUMN `', p_col, '`');
            PREPARE stmt FROM @sql;
            EXECUTE stmt;
            DEALLOCATE PREPARE stmt;
        END IF;
    END$$
    DELIMITER ;

    DROP PROCEDURE IF EXISTS `__rotation_drop_idx_if_exists`;
    DELIMITER $$
    CREATE PROCEDURE `__rotation_drop_idx_if_exists`(
        IN p_db   VARCHAR(64),
        IN p_tbl  VARCHAR(64),
        IN p_idx  VARCHAR(64)
    )
    BEGIN
        DECLARE idx_count INT;
        SELECT COUNT(*) INTO idx_count
          FROM INFORMATION_SCHEMA.STATISTICS
         WHERE TABLE_SCHEMA = p_db
           AND TABLE_NAME   = p_tbl
           AND INDEX_NAME   = p_idx;
        IF idx_count > 0 THEN
            SET @sql = CONCAT('ALTER TABLE `', p_db, '`.`', p_tbl,
                              '` DROP INDEX `', p_idx, '`');
            PREPARE stmt FROM @sql;
            EXECUTE stmt;
            DEALLOCATE PREPARE stmt;
        END IF;
    END$$
    DELIMITER ;
""")


HEADER = dedent("""\
    -- ============================================================================
    -- Rotating Suspense / Receivable / Payable Accounts — sharded DDL
    -- Auto-generated by tools/rotation-migration/gen_sharded_ddl.py
    -- DO NOT edit by hand. Regenerate to update.
    --
    -- Covers 100 shards across 10 DBs (accounting_db_0..accounting_db_9).
    -- For each shard NN (00..99):
    --   1. ALTER TABLE account_NN        — add rotation columns
    --   2. CREATE TABLE tx_account_anchor_NN
    --
    -- Idempotent: re-running this script is safe (uses IF NOT EXISTS where supported;
    -- ALTER columns are wrapped in conditional procedure for compatibility).
    --
    -- Design refs:
    --   docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §3.3 / §3.4
    --   internal/domain/model/rotation.go
    -- ============================================================================

    SET NAMES utf8mb4;
    SET CHARACTER SET utf8mb4;

""") + ADD_PROCEDURES

ACCOUNT_COLS = [
    # (col_name, definition)
    ("logical_account_id",
     "BIGINT UNSIGNED DEFAULT NULL COMMENT '关联 logical_account.id；NULL=旧账户（rotation_enabled=0）'"),
    ("period_start",
     "DATETIME DEFAULT NULL COMMENT '本 instance 承接流量的周期起点（包含），UTC'"),
    ("period_end",
     "DATETIME DEFAULT NULL COMMENT '本 instance 承接流量的周期终点（不含），UTC'"),
    ("lifecycle_phase",
     "TINYINT NOT NULL DEFAULT 0 COMMENT '0=legacy 1=active 2=draining 3=frozen 4=archived 5=provisioned 9=quarantined'"),
    ("draining_started_at",
     "DATETIME DEFAULT NULL COMMENT '进入 draining 的时刻'"),
    ("frozen_at",
     "DATETIME DEFAULT NULL COMMENT '进入 frozen 的时刻'"),
    ("archived_at",
     "DATETIME DEFAULT NULL COMMENT '进入 archived 的时刻'"),
    ("policy_version_at_birth",
     "BIGINT DEFAULT NULL COMMENT '出生时锁定的 policy_version；后续策略变更不影响本 instance（E-28/E-29）'"),
    ("effective_hard_timeout_secs",
     "INT DEFAULT NULL COMMENT '策略覆盖：本 instance 专用 hard timeout，紧急运维通道（§11.X）'"),
    ("override_reason",
     "VARCHAR(255) DEFAULT NULL COMMENT 'override 原因（运维填写）'"),
    ("override_by",
     "VARCHAR(64) DEFAULT NULL COMMENT 'override 操作人'"),
    ("override_at",
     "DATETIME DEFAULT NULL COMMENT 'override 时刻'"),
]

ACCOUNT_INDEXES = [
    ("idx_logical_phase",
     "KEY `idx_logical_phase` (`logical_account_id`, `lifecycle_phase`)"),
    ("idx_phase_period_end",
     "KEY `idx_phase_period_end` (`lifecycle_phase`, `period_end`)"),
]

ANCHOR_TABLE_TEMPLATE = dedent("""\
    -- tx_account_anchor_{nn}: 交易锚点注册表
    -- 分片：按 related_request_id 哈希到 100 片，与 account_transaction 对齐
    -- 不变量：(related_request_id, logical_account_id) 唯一
    CREATE TABLE IF NOT EXISTS `tx_account_anchor_{nn}` (
        `id`                       BIGINT UNSIGNED NOT NULL                            COMMENT '主键（Leaf）',
        `related_request_id`       VARCHAR(64)     NOT NULL                            COMMENT '业务幂等键（=TCC tx 标识）',
        `logical_account_id`       BIGINT UNSIGNED NOT NULL                            COMMENT 'logical_account.id',
        `account_no`               VARCHAR(64)     NOT NULL                            COMMENT '锚定到的 instance account_no',
        `direction_mask`           TINYINT         NOT NULL DEFAULT 0                  COMMENT 'bit0=曾借记 bit1=曾贷记',
        `anchored_at`              DATETIME        NOT NULL                            COMMENT '锚定时刻',
        `last_posting_at`          DATETIME        NOT NULL                            COMMENT '最近一笔分录时刻',
        `posting_count`            INT             NOT NULL DEFAULT 1                  COMMENT '已写入的分录数',
        `migrated_to_account_no`   VARCHAR(64)     DEFAULT NULL                        COMMENT '若发生强制迁移，指向新 instance',
        `migration_voucher_no`     VARCHAR(64)     DEFAULT NULL                        COMMENT '迁移凭证号（审计）',
        `status`                   TINYINT         NOT NULL DEFAULT 1                  COMMENT '0=trying 1=active 2=settled 3=migrated 4=stuck',
        `reuse_source`             TINYINT         NOT NULL DEFAULT 0                  COMMENT '0=primary 1=refund-of 2=reverse-of',
        `reuse_source_anchor_id`   BIGINT UNSIGNED DEFAULT NULL                        COMMENT '复用了哪条 anchor（审计）',
        `migration_chain_depth`    TINYINT         NOT NULL DEFAULT 0                  COMMENT '已发生过多少次强制迁移；>5 进 quarantined (E-50)',
        `created_at`               DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP  COMMENT '创建时间',
        `updated_at`               DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
        `version`                  BIGINT UNSIGNED NOT NULL DEFAULT 0                  COMMENT '乐观锁版本号',
        PRIMARY KEY (`id`),
        UNIQUE KEY `uk_req_logical`             (`related_request_id`, `logical_account_id`),
        KEY         `idx_account_status`        (`account_no`, `status`),
        KEY         `idx_logical_status_lastpost` (`logical_account_id`, `status`, `last_posting_at`),
        KEY         `idx_status_chain_depth`    (`status`, `migration_chain_depth`)
    ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='交易锚点表（轮换路由用）';
""")


SHADOW_HEADER = dedent("""\
    -- ============================================================================
    -- Rotating Suspense / Receivable / Payable Accounts — SHADOW sharded DDL
    -- Auto-generated by tools/rotation-migration/gen_sharded_ddl.py --shadow
    -- DO NOT edit by hand. Regenerate to update.
    --
    -- 影子表（shadow / 压测）侧的 DDL 变更。
    -- 依赖：database/accountingdb/init/rotation.sql 必须已经导入（主表 ALTER 完成 +
    -- tx_account_anchor_NN 已建好）。本脚本：
    --   1. ALTER account_NN_shadow         （主表 ALTER 后 LIKE 副本不会跟随，必须显式 ALTER）
    --   2. CREATE tx_account_anchor_NN_shadow LIKE tx_account_anchor_NN
    -- ============================================================================

    SET NAMES utf8mb4;
    SET CHARACTER SET utf8mb4;

""") + ADD_PROCEDURES


SHADOW_ROLLBACK_HEADER = dedent("""\
    -- ============================================================================
    -- Rotating Suspense / Receivable / Payable Accounts — SHADOW sharded ROLLBACK
    -- Auto-generated by tools/rotation-migration/gen_sharded_ddl.py --shadow --rollback
    -- DO NOT edit by hand. Regenerate to update.
    --
    -- 影子表回滚：先于主表回滚执行，避免 shadow 引用主表已删除的列。
    -- ============================================================================

    SET NAMES utf8mb4;

""") + DROP_PROCEDURES


ROLLBACK_HEADER = dedent("""\
    -- ============================================================================
    -- Rotating Suspense / Receivable / Payable Accounts — SHARDED ROLLBACK
    -- Auto-generated by tools/rotation-migration/gen_sharded_ddl.py --rollback
    -- DO NOT edit by hand. Regenerate to update.
    --
    -- WARNING: DESTROYS rotation feature data on all 100 shards.
    -- Pre-flight (must all return 0):
    --   SELECT COUNT(*) FROM accounting_db_N.tx_account_anchor_NN;
    --   SELECT COUNT(*) FROM accounting_db_N.account_NN WHERE lifecycle_phase != 0;
    --
    -- Companion: database/rollback/rotation_rollback.sql (metadb side).
    -- ============================================================================

    SET NAMES utf8mb4;
    SET CHARACTER SET utf8mb4;

""") + DROP_PROCEDURES


def emit_forward(shadow: bool = False):
    out = [HEADER if not shadow else SHADOW_HEADER]
    for db_idx in range(10):
        out.append(f"\n-- ===== Shard DB: accounting_db_{db_idx} =====\n")
        out.append(f"USE `accounting_db_{db_idx}`;\n")
        for tbl_idx in range(10):
            nn = db_idx * 10 + tbl_idx
            nn_str = f"{nn:02d}"
            db_name = f"accounting_db_{db_idx}"
            account_tbl = f"account_{nn_str}_shadow" if shadow else f"account_{nn_str}"

            out.append(f"\n-- account_{nn_str} (DB={db_name}): add rotation columns\n")
            for col, defn in ACCOUNT_COLS:
                defn_escaped = defn.replace("'", "''")
                out.append(
                    f"CALL `__rotation_add_col_if_missing`('{db_name}', '{account_tbl}', "
                    f"'{col}', '{defn_escaped}');\n"
                )
            for idx_name, idx_def in ACCOUNT_INDEXES:
                idx_def_escaped = idx_def.replace("'", "''")
                out.append(
                    f"CALL `__rotation_add_idx_if_missing`('{db_name}', '{account_tbl}', "
                    f"'{idx_name}', '{idx_def_escaped}');\n"
                )

            out.append("\n")
            if shadow:
                # Shadow anchor table is a snapshot of the production anchor table.
                # Note: this requires production rotation.sql to have run FIRST so
                # tx_account_anchor_NN exists. The init order is documented in
                # database/DB.md (init files are loaded sequentially).
                out.append(
                    f"CREATE TABLE IF NOT EXISTS `tx_account_anchor_{nn_str}_shadow` "
                    f"LIKE `tx_account_anchor_{nn_str}`;\n"
                )
            else:
                out.append(ANCHOR_TABLE_TEMPLATE.format(nn=nn_str))

    out.append(dedent("""

        -- ============================================================================
        -- Cleanup helper procedures (no longer needed after migration completes)
        -- ============================================================================
        DROP PROCEDURE IF EXISTS `__rotation_add_col_if_missing`;
        DROP PROCEDURE IF EXISTS `__rotation_add_idx_if_missing`;
    """))
    return "".join(out)


def emit_rollback(shadow: bool = False):
    out = [ROLLBACK_HEADER if not shadow else SHADOW_ROLLBACK_HEADER]
    # rollback in REVERSE order: drop anchor table first, then drop indexes, then drop columns
    # column drops in reverse-of-add order for symmetry
    cols_reversed = list(reversed(ACCOUNT_COLS))
    for db_idx in range(10):
        out.append(f"\n-- ===== Rollback Shard DB: accounting_db_{db_idx} =====\n")
        out.append(f"USE `accounting_db_{db_idx}`;\n")
        for tbl_idx in range(10):
            nn = db_idx * 10 + tbl_idx
            nn_str = f"{nn:02d}"
            db_name = f"accounting_db_{db_idx}"
            account_tbl = f"account_{nn_str}_shadow" if shadow else f"account_{nn_str}"
            anchor_tbl = f"tx_account_anchor_{nn_str}_shadow" if shadow else f"tx_account_anchor_{nn_str}"

            out.append(f"\n-- {account_tbl} (DB={db_name}): drop rotation indexes + columns\n")
            # drop indexes first (some DBs forbid dropping columns referenced by indexes)
            for idx_name, _ in ACCOUNT_INDEXES:
                out.append(
                    f"CALL `__rotation_drop_idx_if_exists`('{db_name}', '{account_tbl}', "
                    f"'{idx_name}');\n"
                )
            for col, _ in cols_reversed:
                out.append(
                    f"CALL `__rotation_drop_col_if_exists`('{db_name}', '{account_tbl}', "
                    f"'{col}');\n"
                )
            # drop anchor table
            out.append(f"DROP TABLE IF EXISTS `{anchor_tbl}`;\n")

    out.append(dedent("""

        -- ============================================================================
        -- Cleanup rollback helper procedures
        -- ============================================================================
        DROP PROCEDURE IF EXISTS `__rotation_drop_col_if_exists`;
        DROP PROCEDURE IF EXISTS `__rotation_drop_idx_if_exists`;
    """))
    return "".join(out)


def main():
    parser = argparse.ArgumentParser(description="Generate rotating-accounts sharded DDL.")
    parser.add_argument("--rollback", action="store_true",
                        help="Generate rollback DDL instead of forward DDL.")
    parser.add_argument("--shadow", action="store_true",
                        help="Generate DDL for shadow tables (account_NN_shadow / tx_account_anchor_NN_shadow).")
    args = parser.parse_args()
    if args.rollback:
        sys.stdout.write(emit_rollback(shadow=args.shadow))
    else:
        sys.stdout.write(emit_forward(shadow=args.shadow))


if __name__ == "__main__":
    main()
