#!/usr/bin/env python3
"""
Verify that rotation DDL has been correctly injected into init.sql files.

Run after inject_rotation_ddl.py to assert:
  - All 100 account_NN tables have all 12 rotation columns + 2 indexes
  - All 100 tx_account_anchor_NN tables exist (main)
  - All 100 tx_account_anchor_NN_shadow LIKE statements exist (shadow)
  - metadb has logical_account / logical_account_rotation_policy + 4 new business types

Run:
    python3 tools/rotation-migration/verify_rotation_ddl.py
    # exit 0 = all good; non-zero = drift detected
"""

import os
import sys

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
ACCT_INIT_DIR = os.path.join(ROOT, "packages/accounting-system/database/accountingdb/init")
META_INIT_DIR = os.path.join(ROOT, "packages/accounting-system/database/metadb/init")

REQUIRED_ACCOUNT_COLUMNS = [
    "logical_account_id",
    "period_start",
    "period_end",
    "lifecycle_phase",
    "draining_started_at",
    "frozen_at",
    "archived_at",
    "policy_version_at_birth",
    "effective_hard_timeout_secs",
    "override_reason",
    "override_by",
    "override_at",
]

REQUIRED_ACCOUNT_INDEXES = [
    "idx_logical_phase",
    "idx_phase_period_end",
]

REQUIRED_ANCHOR_COLUMNS = [
    "related_request_id",
    "logical_account_id",
    "account_no",
    "direction_mask",
    "anchored_at",
    "last_posting_at",
    "posting_count",
    "migrated_to_account_no",
    "migration_voucher_no",
    "status",
    "reuse_source",
    "reuse_source_anchor_id",
    "migration_chain_depth",
    "version",
]


def slice_create_table(sql: str, table: str) -> str:
    """Return the CREATE TABLE block for the named table, or '' if missing."""
    marker = f"CREATE TABLE IF NOT EXISTS `{table}` ("
    start = sql.find(marker)
    if start < 0:
        return ""
    # Match through the trailing semicolon after `) ENGINE=...`
    end = sql.find(";", start)
    if end < 0:
        return ""
    return sql[start:end + 1]


def main():
    errors = []

    for db_idx in range(10):
        path = os.path.join(ACCT_INIT_DIR, f"{db_idx}_init.sql")
        with open(path, "r", encoding="utf-8") as f:
            sql = f.read()

        for tbl_idx in range(10):
            nn = f"{db_idx * 10 + tbl_idx:02d}"

            # account_NN must have all 12 rotation columns + 2 indexes
            account_block = slice_create_table(sql, f"account_{nn}")
            if not account_block:
                errors.append(f"missing CREATE TABLE account_{nn} in {path}")
                continue
            for col in REQUIRED_ACCOUNT_COLUMNS:
                if f"`{col}`" not in account_block:
                    errors.append(f"account_{nn}: missing column `{col}`")
            for idx in REQUIRED_ACCOUNT_INDEXES:
                if f"`{idx}`" not in account_block:
                    errors.append(f"account_{nn}: missing index `{idx}`")

            # tx_account_anchor_NN must exist
            anchor_block = slice_create_table(sql, f"tx_account_anchor_{nn}")
            if not anchor_block:
                errors.append(f"missing CREATE TABLE tx_account_anchor_{nn} in {path}")
                continue
            for col in REQUIRED_ANCHOR_COLUMNS:
                if f"`{col}`" not in anchor_block:
                    errors.append(f"tx_account_anchor_{nn}: missing column `{col}`")
            if "uk_req_logical" not in anchor_block:
                errors.append(f"tx_account_anchor_{nn}: missing UNIQUE KEY uk_req_logical")

        # shadow side must have all 10 LIKE statements
        shadow_path = os.path.join(ACCT_INIT_DIR, f"{db_idx}_init_shadow.sql")
        with open(shadow_path, "r", encoding="utf-8") as f:
            shadow_sql = f.read()
        for tbl_idx in range(10):
            nn = f"{db_idx * 10 + tbl_idx:02d}"
            line = f"`tx_account_anchor_{nn}_shadow` LIKE `tx_account_anchor_{nn}`"
            if line not in shadow_sql:
                errors.append(f"missing shadow LIKE for tx_account_anchor_{nn} in {shadow_path}")

    # metadb checks
    meta_path = os.path.join(META_INIT_DIR, "init.sql")
    with open(meta_path, "r", encoding="utf-8") as f:
        meta_sql = f.read()

    if not slice_create_table(meta_sql, "logical_account"):
        errors.append(f"missing CREATE TABLE logical_account in {meta_path}")
    if not slice_create_table(meta_sql, "logical_account_rotation_policy"):
        errors.append(f"missing CREATE TABLE logical_account_rotation_policy in {meta_path}")
    for bt in ("ROTATION_MIGRATION_SUSPENSE", "ROTATION_RESIDUAL_WRITEOFF",
               "ROTATION_OPS_ADJUST", "ROTATION_CARRYFORWARD"):
        if bt not in meta_sql:
            errors.append(f"missing predefined business_type {bt} in {meta_path}")

    meta_shadow_path = os.path.join(META_INIT_DIR, "init_shadow.sql")
    with open(meta_shadow_path, "r", encoding="utf-8") as f:
        meta_shadow_sql = f.read()
    for line in ("logical_account_shadow", "logical_account_rotation_policy_shadow"):
        if f"`{line}`" not in meta_shadow_sql:
            errors.append(f"missing shadow table {line} in {meta_shadow_path}")

    if errors:
        print(f"FAIL: {len(errors)} drift(s) detected:")
        for e in errors:
            print(f"  - {e}")
        sys.exit(1)

    # All good — print summary
    print("PASS: rotation DDL verified across all shards")
    print(f"  - 100 account_NN tables × 12 columns × 2 indexes")
    print(f"  - 100 tx_account_anchor_NN tables (main)")
    print(f"  - 100 tx_account_anchor_NN_shadow LIKE statements")
    print(f"  - metadb: logical_account, logical_account_rotation_policy, 4 new business_types")
    print(f"  - metadb_shadow: shadow LIKE for logical_account & policy")


if __name__ == "__main__":
    main()
