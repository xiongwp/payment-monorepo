#!/usr/bin/env python3
"""
In-place injection of rotation DDL into existing init.sql files.

系统未上线，schema 直接落进现有 init 文件，不走 migration ALTER 路径。

Modifies:
  packages/accounting-system/database/accountingdb/init/N_init.sql       (N=0..9)
  packages/accounting-system/database/accountingdb/init/N_init_shadow.sql
  packages/accounting-system/database/metadb/init/init.sql
  packages/accounting-system/database/metadb/init/init_shadow.sql

Idempotent: re-running detects existing rotation fields/tables and skips.

Usage:
    python3 tools/rotation-migration/inject_rotation_ddl.py
"""

import os
import re
import sys

# Locate repo root: this script is at <repo>/tools/rotation-migration/...
ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
ACCT_INIT_DIR = os.path.join(ROOT, "packages/accounting-system/database/accountingdb/init")
META_INIT_DIR = os.path.join(ROOT, "packages/accounting-system/database/metadb/init")


# ============================================================================
# Replacement: rotation-aware account_NN CREATE TABLE
# 注意：保留原 schema 100% 的字段、注释、索引；只追加 rotation 列与索引。
# ============================================================================

ROTATION_COLS_BLOCK = """\
    -- 轮换字段（rotating-suspense-accounts 特性，见 docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md）
    -- 旧账户 logical_account_id=NULL 且 lifecycle_phase=0 (legacy)，写入路径与原行为一致。
    `logical_account_id` BIGINT UNSIGNED DEFAULT NULL COMMENT '关联 logical_account.id；NULL=legacy 账户',
    `period_start` DATETIME DEFAULT NULL COMMENT '本 instance 承接流量的周期起点（包含），UTC',
    `period_end` DATETIME DEFAULT NULL COMMENT '本 instance 承接流量的周期终点（不含），UTC',
    `lifecycle_phase` TINYINT NOT NULL DEFAULT 0 COMMENT '0=legacy 1=active 2=draining 3=frozen 4=archived 5=provisioned 9=quarantined',
    `draining_started_at` DATETIME DEFAULT NULL COMMENT '进入 draining 的时刻',
    `frozen_at` DATETIME DEFAULT NULL COMMENT '进入 frozen 的时刻',
    `archived_at` DATETIME DEFAULT NULL COMMENT '进入 archived 的时刻',
    `policy_version_at_birth` BIGINT DEFAULT NULL COMMENT '出生时锁定的 policy_version；后续策略变更不影响本 instance (E-28/E-29)',
    `effective_hard_timeout_secs` INT DEFAULT NULL COMMENT '策略覆盖：本 instance 专用 hard timeout (§11.X 紧急运维)',
    `override_reason` VARCHAR(255) DEFAULT NULL COMMENT 'override 原因',
    `override_by` VARCHAR(64) DEFAULT NULL COMMENT 'override 操作人',
    `override_at` DATETIME DEFAULT NULL COMMENT 'override 时刻',"""

ROTATION_INDEXES_BLOCK = """\
    KEY `idx_logical_phase` (`logical_account_id`, `lifecycle_phase`),
    KEY `idx_phase_period_end` (`lifecycle_phase`, `period_end`)"""


def anchor_table_ddl(nn: str) -> str:
    """Generate CREATE TABLE for tx_account_anchor_NN."""
    return f"""\
-- ============================================
-- tx_account_anchor_{nn}: 资金流账户锚点表
-- 【方向 B 分片】按 account_no FNV-1a 哈希到 100 片（与 account_transaction、account 同片）
--   关键：anchor 与对应 entry 必然在同一物理分片 → 同一本地事务 atomic 写入，无需 TCC
-- 不变量：(flow_id, account_no) 唯一 — 同一资金流在同一 instance 上仅一条 anchor
-- 设计：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §3.4
--
-- 查找路径（业务方按 (flow_id, logical_account_id) 找 account_no）：
--   先查 flow_anchor_route_NN（按 flow_id 分片）拿到 account_no → 路由到本表（按 account_no 分片）
-- ============================================
CREATE TABLE IF NOT EXISTS `tx_account_anchor_{nn}` (
    `id` BIGINT UNSIGNED NOT NULL COMMENT '主键（Leaf 号段生成）',
    `flow_id` VARCHAR(64) NOT NULL COMMENT '资金流 ID（业务方 business_id）',
    `logical_account_id` BIGINT UNSIGNED NOT NULL COMMENT 'logical_account.id（per-shard 二级索引使用）',
    `account_no` VARCHAR(64) NOT NULL COMMENT '锚定到的 instance account_no（分片键）',
    `direction_mask` TINYINT NOT NULL DEFAULT 0 COMMENT 'bit0=曾借记 bit1=曾贷记',
    `anchored_at` DATETIME NOT NULL COMMENT '锚定时刻',
    `last_posting_at` DATETIME NOT NULL COMMENT '最近一笔分录时刻',
    `posting_count` INT NOT NULL DEFAULT 1 COMMENT '已写入的分录数',
    `migrated_to_account_no` VARCHAR(64) DEFAULT NULL COMMENT '若发生强制迁移，指向新 instance',
    `migration_voucher_no` VARCHAR(64) DEFAULT NULL COMMENT '迁移凭证号（审计）',
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '0=trying 1=active 2=settled 3=migrated 4=stuck',
    `reuse_source` TINYINT NOT NULL DEFAULT 0 COMMENT '0=primary 1=refund-of 2=reverse-of',
    `reuse_source_anchor_id` BIGINT UNSIGNED DEFAULT NULL COMMENT '复用了哪条 anchor（审计）',
    `migration_chain_depth` TINYINT NOT NULL DEFAULT 0 COMMENT '已发生过多少次强制迁移；>5 进 quarantined (E-50)',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_flow_account` (`flow_id`, `account_no`),
    KEY `idx_flow_logical` (`flow_id`, `logical_account_id`),
    KEY `idx_account_status` (`account_no`, `status`),
    KEY `idx_status_chain_depth` (`status`, `migration_chain_depth`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='资金流账户锚点表（方向B：按 account_no 分片）';
"""


def route_table_ddl(nn: str) -> str:
    """Generate CREATE TABLE for flow_anchor_route_NN — routing index for direction B."""
    return f"""\
-- ============================================
-- flow_anchor_route_{nn}: 资金流 → instance 路由索引表
-- 【方向 B 关键】按 flow_id FNV-1a 哈希到 100 片
-- 作用：通过 (flow_id, logical_account_id) → account_no 解析，让 router 知道
--      "本资金流在某 logical_account 上锁定到了哪个 instance"
-- 不变量：(flow_id, logical_account_id) 唯一 — 一个 flow 在同一 LA 上仅锁定一个 instance（I0）
--
-- 写入：
--   - 首次锚定（router 决定 account_no）→ INSERT 一行（在 flow_id shard 本地事务）
--   - 强制迁移：UPDATE account_no 字段（chain_depth++）
--   - 其他情况绝不修改 — immutable lookup record
--
-- 读取：路由层热路径每次都查（5s 缓存）
-- ============================================
CREATE TABLE IF NOT EXISTS `flow_anchor_route_{nn}` (
    `id` BIGINT UNSIGNED NOT NULL COMMENT '主键（Leaf）',
    `flow_id` VARCHAR(64) NOT NULL COMMENT '资金流 ID（分片键）',
    `logical_account_id` BIGINT UNSIGNED NOT NULL COMMENT 'logical_account.id',
    `account_no` VARCHAR(64) NOT NULL COMMENT 'flow 在本 LA 上锁定的 account_no',
    `migration_chain_depth` TINYINT NOT NULL DEFAULT 0 COMMENT '迁移跳数（与 anchor 表一致）',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_flow_logical` (`flow_id`, `logical_account_id`),
    KEY `idx_account_no` (`account_no`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='资金流路由索引（方向B）';
"""


# ============================================================================
# 注入逻辑
# ============================================================================

def inject_account_columns(sql: str, nn: str) -> tuple[str, bool]:
    """
    Inject rotation columns + indexes into CREATE TABLE `account_NN` block.
    Returns (new_sql, changed). Idempotent: detects 'lifecycle_phase' presence.
    """
    table = f"account_{nn}"
    # Find CREATE TABLE block. We match from `CREATE TABLE IF NOT EXISTS \`account_NN\``
    # through the closing `) ENGINE=...;` line for that table.
    create_re = re.compile(
        rf"(CREATE TABLE IF NOT EXISTS `{re.escape(table)}` \([\s\S]*?\) ENGINE=[^\n]+;)",
        re.MULTILINE,
    )
    m = create_re.search(sql)
    if not m:
        raise RuntimeError(f"could not locate CREATE TABLE for {table}")
    block = m.group(1)

    # Idempotency check
    if "`lifecycle_phase`" in block:
        return sql, False

    # Inject columns: insert ROTATION_COLS_BLOCK BEFORE the `PRIMARY KEY (\`id\`)` line.
    # Inject indexes: append AFTER the last existing `KEY` line (last `idx_type_status`).
    new_block = block
    new_block = re.sub(
        r"(    PRIMARY KEY \(`id`\),)",
        ROTATION_COLS_BLOCK + r"\n\1",
        new_block,
        count=1,
    )
    # Append indexes BEFORE the closing `) ENGINE=` line.
    # Find the last index line (which has trailing comma already? or not? actually the
    # last index in original has NO trailing comma — we need to add a comma to it
    # then add our two new indexes without trailing comma).
    new_block = re.sub(
        r"(    KEY `idx_type_status` \(`account_type`, `status`\))\n(\) ENGINE=)",
        r"\1,\n" + ROTATION_INDEXES_BLOCK + r"\n\2",
        new_block,
        count=1,
    )

    if new_block == block:
        raise RuntimeError(
            f"injection did not modify CREATE TABLE for {table}; pattern mismatch?\n"
            f"--- block ---\n{block}\n--- end ---"
        )
    return sql.replace(block, new_block, 1), True


def append_anchor_tables(sql: str, db_idx: int) -> tuple[str, int]:
    """
    Append CREATE TABLE tx_account_anchor_NN + flow_anchor_route_NN blocks at end of file.
    Returns (new_sql, n_appended). Idempotent.

    方向 B：每个分片同时包含 anchor 表（按 account_no 分片）和 route 表（按 flow_id 分片）。
    虽然两者分片键不同，但 schema-wise 我们让每个 init.sql 文件包含同 NN 的两张表
    便于运维（按 NN 编号能找到这一分片的所有相关表）。
    """
    appended = 0
    pieces = []
    for tbl_idx in range(10):
        nn = f"{db_idx * 10 + tbl_idx:02d}"
        if f"`tx_account_anchor_{nn}`" not in sql:
            pieces.append(anchor_table_ddl(nn))
            appended += 1
        if f"`flow_anchor_route_{nn}`" not in sql:
            pieces.append(route_table_ddl(nn))
            appended += 1
    if not pieces:
        return sql, 0
    # ensure trailing newline before appending
    if not sql.endswith("\n"):
        sql += "\n"
    sql += "\n" + "\n".join(pieces)
    return sql, appended


def append_shadow_anchor_likes(sql: str, db_idx: int) -> tuple[str, int]:
    """Append shadow LIKE rows for tx_account_anchor_NN and flow_anchor_route_NN."""
    appended = 0
    lines = []
    for tbl_idx in range(10):
        nn = f"{db_idx * 10 + tbl_idx:02d}"
        for base in (f"tx_account_anchor_{nn}", f"flow_anchor_route_{nn}"):
            line = f"CREATE TABLE IF NOT EXISTS `{base}_shadow` LIKE `{base}`;"
            if line in sql:
                continue
            lines.append(line)
            appended += 1
    if not lines:
        return sql, 0
    if not sql.endswith("\n"):
        sql += "\n"
    sql += "\n" + "\n".join(lines) + "\n"
    return sql, appended


# ============================================================================
# metadb side
# ============================================================================

METADB_NEW_TABLES = """\

-- ============================================
-- Rotating Suspense / Receivable / Payable Accounts
-- 设计文档：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md
-- 域模型：internal/domain/model/rotation.go
--
-- logical_account：跨周期稳定的逻辑账户。多个 account instance 在不同周期承接其流量。
-- I1 不变量：同一 logical_account_id 下任意时刻至多一个 instance phase=active
--           （由 scheduler 切换事务 + invariant_audit_job 巡检保证）
-- 反范式化：current_active_account_no/period_end 由 scheduler 在切换事务原子更新，
--           路由层热路径只查本表即可，避免跨片 account 表 scan。
-- ============================================
CREATE TABLE IF NOT EXISTS `logical_account` (
    `id` BIGINT UNSIGNED NOT NULL COMMENT '主键（Leaf 号段生成）',
    `logical_account_key` VARCHAR(64) NOT NULL COMMENT '业务稳定 key（命名前缀白名单见 rotation.go AllowedKeyPrefixes）',
    `account_type` TINYINT NOT NULL COMMENT '复用 AccountType (期望值 5/6/9)',
    `account_business_type` SMALLINT NOT NULL COMMENT '复用 AccountBusinessType (1-999)',
    `currency` CHAR(3) NOT NULL COMMENT 'ISO 4217',
    `description` VARCHAR(255) DEFAULT NULL,
    `rotation_enabled` TINYINT NOT NULL DEFAULT 0 COMMENT '0=不轮换(legacy) 1=轮换',
    `current_active_account_no` VARCHAR(64) DEFAULT NULL COMMENT '反范式化：当期 active 的 account_no',
    `current_active_period_end` DATETIME DEFAULT NULL,
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '0=disabled 1=enabled',
    `registered_by` VARCHAR(64) NOT NULL COMMENT '注册者（审计；禁止 lazy create）',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_lak` (`logical_account_key`),
    KEY `idx_type_biz_currency` (`account_type`, `account_business_type`, `currency`),
    KEY `idx_rotation_enabled` (`rotation_enabled`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='逻辑账户（跨周期稳定）';


-- ============================================
-- logical_account_rotation_policy：单个逻辑账户的轮换策略
-- 关键字段：
--   drain_p99_seconds       — draining 保留下限（业务 P99 生命周期）
--   drain_hard_timeout_secs — draining 保留上限，超过强制迁移（§8）
--   archive_grace_secs      — frozen → archived 缓冲
--   provision_lead_secs     — scheduler 提前多久预创建下一期（默认 24h）
--   config_version          — 配置版本号，路由层用它判断缓存是否过期 (E-30)
-- 旧 instance 走出生时锁定的 policy_version_at_birth 而非最新策略 (E-28/E-29)。
-- ============================================
CREATE TABLE IF NOT EXISTS `logical_account_rotation_policy` (
    `logical_account_id` BIGINT UNSIGNED NOT NULL COMMENT 'logical_account.id',
    `period_unit` VARCHAR(8) NOT NULL COMMENT 'DAY(测试) / MONTH(应付应收) / QUARTER(通用中间)',
    `period_count` INT NOT NULL DEFAULT 1 COMMENT '周期倍数',
    `rotation_anchor_tz` VARCHAR(32) NOT NULL COMMENT 'IANA 时区',
    `drain_p99_seconds` INT NOT NULL COMMENT 'draining 最短保留',
    `drain_hard_timeout_secs` INT NOT NULL COMMENT 'draining 最长保留；超过强制迁移',
    `archive_grace_secs` INT NOT NULL DEFAULT 604800 COMMENT 'frozen → archived 缓冲（默认 7 天）',
    `provision_lead_secs` INT NOT NULL DEFAULT 86400 COMMENT '提前预创建下一期（默认 24h）',
    `config_version` BIGINT NOT NULL DEFAULT 1 COMMENT '配置版本号；每次变更 +1',
    `effective_from` DATETIME NOT NULL,
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`logical_account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='轮换策略';
"""

METADB_NEW_BUSINESS_TYPES = """\

-- 轮换特性新增预置（10/11/12/13）
INSERT IGNORE INTO `account_business_type_info`
    (`business_type`, `business_type_code`, `account_type`, `description`, `enabled`)
VALUES
    (10, 'ROTATION_MIGRATION_SUSPENSE',  9, '跨期强制迁移过渡科目，余额恒为 0', 1),
    (11, 'ROTATION_RESIDUAL_WRITEOFF',   4, '轮换归档残值核销账户', 1),
    (12, 'ROTATION_OPS_ADJUST',          4, '轮换人工运维调整账户', 1),
    (13, 'ROTATION_CARRYFORWARD',        2, '轮换跨期结转科目，余额恒为 0', 1);
"""

METADB_SHADOW_TABLES = """\

-- Rotation feature shadow tables
CREATE TABLE IF NOT EXISTS `logical_account_shadow`                  LIKE `logical_account`;
CREATE TABLE IF NOT EXISTS `logical_account_rotation_policy_shadow`  LIKE `logical_account_rotation_policy`;

-- shadow 流量同步新增字典 seed
INSERT IGNORE INTO `account_business_type_info_shadow`
    SELECT * FROM `account_business_type_info`
     WHERE `business_type` IN (10, 11, 12, 13);
"""


def inject_metadb_main(sql: str) -> tuple[str, bool]:
    changed = False
    if "`logical_account`" not in sql:
        sql += METADB_NEW_TABLES
        changed = True
    if "'ROTATION_MIGRATION_SUSPENSE'" not in sql:
        sql += METADB_NEW_BUSINESS_TYPES
        changed = True
    return sql, changed


def inject_metadb_shadow(sql: str) -> tuple[str, bool]:
    changed = False
    if "`logical_account_shadow`" not in sql:
        sql += METADB_SHADOW_TABLES
        changed = True
    return sql, changed


# ============================================================================
# Top-level driver
# ============================================================================

def process_main_init(db_idx: int) -> dict:
    path = os.path.join(ACCT_INIT_DIR, f"{db_idx}_init.sql")
    with open(path, "r", encoding="utf-8") as f:
        sql = f.read()
    orig = sql

    cols_changed = 0
    for tbl_idx in range(10):
        nn = f"{db_idx * 10 + tbl_idx:02d}"
        sql, changed = inject_account_columns(sql, nn)
        if changed:
            cols_changed += 1

    sql, anchors_added = append_anchor_tables(sql, db_idx)

    if sql != orig:
        with open(path, "w", encoding="utf-8") as f:
            f.write(sql)
    return {
        "file": path,
        "columns_modified": cols_changed,
        "anchor_tables_added": anchors_added,
    }


def process_shadow_init(db_idx: int) -> dict:
    path = os.path.join(ACCT_INIT_DIR, f"{db_idx}_init_shadow.sql")
    with open(path, "r", encoding="utf-8") as f:
        sql = f.read()
    orig = sql

    sql, likes_added = append_shadow_anchor_likes(sql, db_idx)

    if sql != orig:
        with open(path, "w", encoding="utf-8") as f:
            f.write(sql)
    return {"file": path, "shadow_likes_added": likes_added}


def process_metadb_main() -> dict:
    path = os.path.join(META_INIT_DIR, "init.sql")
    with open(path, "r", encoding="utf-8") as f:
        sql = f.read()
    orig = sql
    sql, changed = inject_metadb_main(sql)
    if sql != orig:
        with open(path, "w", encoding="utf-8") as f:
            f.write(sql)
    return {"file": path, "changed": changed}


def process_metadb_shadow() -> dict:
    path = os.path.join(META_INIT_DIR, "init_shadow.sql")
    with open(path, "r", encoding="utf-8") as f:
        sql = f.read()
    orig = sql
    sql, changed = inject_metadb_shadow(sql)
    if sql != orig:
        with open(path, "w", encoding="utf-8") as f:
            f.write(sql)
    return {"file": path, "changed": changed}


def main():
    results = []
    for i in range(10):
        results.append(process_main_init(i))
    for i in range(10):
        results.append(process_shadow_init(i))
    results.append(process_metadb_main())
    results.append(process_metadb_shadow())

    for r in results:
        print(r)


if __name__ == "__main__":
    main()
