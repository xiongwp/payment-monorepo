#!/usr/bin/env python3
"""
Self-tests for gen_sharded_ddl.py.

Run:
    python3 tools/rotation-migration/test_gen_sharded_ddl.py
"""

import re
import sys
import unittest

# Import the generator (same directory)
sys.path.insert(0, __file__.rsplit("/", 1)[0])
import gen_sharded_ddl as g  # noqa: E402


class TestForwardDDL(unittest.TestCase):
    def setUp(self):
        self.sql = g.emit_forward()

    def test_covers_10_dbs(self):
        for i in range(10):
            self.assertIn(f"USE `accounting_db_{i}`;", self.sql)

    def test_creates_100_anchor_tables(self):
        for nn in range(100):
            nn_str = f"{nn:02d}"
            pattern = f"CREATE TABLE IF NOT EXISTS `tx_account_anchor_{nn_str}`"
            self.assertIn(pattern, self.sql,
                          f"missing CREATE for tx_account_anchor_{nn_str}")
        # exact count
        actual = self.sql.count("CREATE TABLE IF NOT EXISTS `tx_account_anchor_")
        self.assertEqual(actual, 100, "must create exactly 100 anchor tables")

    def test_alters_100_account_tables_all_columns(self):
        """Each of the 100 account_NN tables must add all 12 rotation columns."""
        expected_cols = [c[0] for c in g.ACCOUNT_COLS]
        self.assertEqual(len(expected_cols), 12)
        for nn in range(100):
            nn_str = f"{nn:02d}"
            for col in expected_cols:
                # CALL `__rotation_add_col_if_missing`('accounting_db_X', 'account_NN', 'col', ...)
                pattern = f"'account_{nn_str}', '{col}'"
                self.assertIn(pattern, self.sql,
                              f"missing ADD COLUMN {col} on account_{nn_str}")

    def test_alters_100_account_tables_all_indexes(self):
        expected_indexes = [i[0] for i in g.ACCOUNT_INDEXES]
        self.assertEqual(len(expected_indexes), 2)
        for nn in range(100):
            nn_str = f"{nn:02d}"
            for idx in expected_indexes:
                pattern = f"'account_{nn_str}', '{idx}'"
                self.assertIn(pattern, self.sql,
                              f"missing ADD INDEX {idx} on account_{nn_str}")

    def test_helper_procedures_dropped_at_end(self):
        # avoid leaking helper procedures after migration
        tail = self.sql[-500:]
        self.assertIn("DROP PROCEDURE IF EXISTS `__rotation_add_col_if_missing`", tail)
        self.assertIn("DROP PROCEDURE IF EXISTS `__rotation_add_idx_if_missing`", tail)

    def test_no_dangling_single_quotes(self):
        """Catch escaping bugs in column definitions: every odd-position quote
        would corrupt MySQL parsing. We count quote occurrences within
        each CALL statement and assert balance."""
        # Strip stored procedure body (delimiter $$) from the check — only check CALL statements
        call_re = re.compile(r"CALL `__rotation_add_(?:col|idx)_if_missing`\([^)]*\);")
        for m in call_re.findall(self.sql):
            n_quotes = m.count("'")
            self.assertEqual(n_quotes % 2, 0,
                             f"odd number of single quotes in: {m[:80]}")

    def test_uses_utf8mb4(self):
        self.assertIn("CHARSET=utf8mb4", self.sql)

    def test_uses_innodb(self):
        self.assertIn("ENGINE=InnoDB", self.sql)

    def test_anchor_has_correct_unique_index(self):
        # uk_req_logical must combine related_request_id + logical_account_id
        self.assertIn(
            "UNIQUE KEY `uk_req_logical`             (`related_request_id`, `logical_account_id`)",
            self.sql)


class TestRollbackDDL(unittest.TestCase):
    def setUp(self):
        self.sql = g.emit_rollback()

    def test_drops_100_anchor_tables(self):
        for nn in range(100):
            nn_str = f"{nn:02d}"
            pattern = f"DROP TABLE IF EXISTS `tx_account_anchor_{nn_str}`;"
            self.assertIn(pattern, self.sql,
                          f"missing DROP TABLE for tx_account_anchor_{nn_str}")

    def test_drops_100_account_tables_all_columns(self):
        expected_cols = [c[0] for c in g.ACCOUNT_COLS]
        for nn in range(100):
            nn_str = f"{nn:02d}"
            for col in expected_cols:
                pattern = f"'account_{nn_str}', '{col}'"
                self.assertIn(pattern, self.sql,
                              f"missing DROP COLUMN {col} on account_{nn_str}")

    def test_drops_indexes_before_columns(self):
        """Indexes referencing dropped columns must be removed first.
        Per shard: idx DROP statements must appear before the column drops
        that share names (e.g., logical_account_id is part of idx_logical_phase)."""
        # For each shard, find positions of idx_logical_phase drop vs logical_account_id drop
        for nn in range(100):
            nn_str = f"{nn:02d}"
            idx_drop = f"'account_{nn_str}', 'idx_logical_phase'"
            col_drop = f"'account_{nn_str}', 'logical_account_id'"
            pos_idx = self.sql.find(idx_drop)
            pos_col = self.sql.find(col_drop)
            self.assertGreater(pos_idx, 0, f"missing idx drop for {nn_str}")
            self.assertGreater(pos_col, 0, f"missing col drop for {nn_str}")
            self.assertLess(pos_idx, pos_col,
                            f"shard {nn_str}: idx drop must precede col drop")

    def test_helper_procedures_dropped(self):
        self.assertIn("DROP PROCEDURE IF EXISTS `__rotation_drop_col_if_exists`", self.sql)
        self.assertIn("DROP PROCEDURE IF EXISTS `__rotation_drop_idx_if_exists`", self.sql)


class TestSymmetry(unittest.TestCase):
    """Forward and rollback must touch the exact same column / index set."""

    def test_same_column_set(self):
        fwd_cols = {c[0] for c in g.ACCOUNT_COLS}
        # rollback iterates the same list (reversed) — verify nothing was forgotten
        for col in fwd_cols:
            self.assertIn(f", '{col}'", g.emit_rollback())

    def test_same_index_set(self):
        fwd_idx = {i[0] for i in g.ACCOUNT_INDEXES}
        for idx in fwd_idx:
            self.assertIn(f", '{idx}'", g.emit_rollback())


class TestRegeneration(unittest.TestCase):
    """Re-running the generator must produce byte-identical output."""

    def test_forward_idempotent(self):
        a = g.emit_forward()
        b = g.emit_forward()
        self.assertEqual(a, b)

    def test_rollback_idempotent(self):
        a = g.emit_rollback()
        b = g.emit_rollback()
        self.assertEqual(a, b)

    def test_shadow_forward_idempotent(self):
        a = g.emit_forward(shadow=True)
        b = g.emit_forward(shadow=True)
        self.assertEqual(a, b)

    def test_shadow_rollback_idempotent(self):
        a = g.emit_rollback(shadow=True)
        b = g.emit_rollback(shadow=True)
        self.assertEqual(a, b)


class TestShadowForwardDDL(unittest.TestCase):
    """Shadow tables must ALTER 100 shadow account tables + CREATE 100 shadow anchor tables.

    Critical: production tables go through CREATE TABLE LIKE → shadow tables don't
    inherit subsequent ALTERs. We must explicitly ALTER each shadow.
    """

    def setUp(self):
        self.sql = g.emit_forward(shadow=True)

    def test_does_not_touch_production_tables(self):
        # No reference to non-shadow tables in shadow forward
        # (every account_NN reference must end with _shadow)
        for nn in range(100):
            nn_str = f"{nn:02d}"
            # `account_NN'` (with closing quote, not followed by _shadow) is forbidden
            forbidden = f"'account_{nn_str}',"
            self.assertNotIn(
                forbidden, self.sql,
                f"shadow forward leaked production table account_{nn_str}"
            )
            forbidden_anchor = f"`tx_account_anchor_{nn_str}` "
            self.assertNotIn(
                forbidden_anchor + "ADD", self.sql,
                f"shadow forward leaked production anchor tx_account_anchor_{nn_str}",
            )

    def test_alters_100_shadow_account_tables(self):
        expected_cols = [c[0] for c in g.ACCOUNT_COLS]
        for nn in range(100):
            nn_str = f"{nn:02d}"
            for col in expected_cols:
                pattern = f"'account_{nn_str}_shadow', '{col}'"
                self.assertIn(pattern, self.sql,
                              f"missing ADD COLUMN {col} on account_{nn_str}_shadow")

    def test_creates_100_shadow_anchor_tables_via_LIKE(self):
        for nn in range(100):
            nn_str = f"{nn:02d}"
            pattern = (f"CREATE TABLE IF NOT EXISTS `tx_account_anchor_{nn_str}_shadow` "
                       f"LIKE `tx_account_anchor_{nn_str}`")
            self.assertIn(pattern, self.sql,
                          f"missing LIKE clone for tx_account_anchor_{nn_str}_shadow")

    def test_includes_helper_procedures(self):
        # bug we just fixed: SHADOW_HEADER must also bring in ADD_PROCEDURES,
        # otherwise CALL statements fail.
        self.assertIn("CREATE PROCEDURE `__rotation_add_col_if_missing`", self.sql)
        self.assertIn("CREATE PROCEDURE `__rotation_add_idx_if_missing`", self.sql)
        # and they must be dropped at the end
        tail = self.sql[-500:]
        self.assertIn("DROP PROCEDURE IF EXISTS `__rotation_add_col_if_missing`", tail)


class TestShadowRollbackDDL(unittest.TestCase):
    def setUp(self):
        self.sql = g.emit_rollback(shadow=True)

    def test_drops_100_shadow_anchor_tables(self):
        for nn in range(100):
            nn_str = f"{nn:02d}"
            pattern = f"DROP TABLE IF EXISTS `tx_account_anchor_{nn_str}_shadow`;"
            self.assertIn(pattern, self.sql,
                          f"missing DROP for tx_account_anchor_{nn_str}_shadow")

    def test_drops_columns_on_100_shadow_account_tables(self):
        expected_cols = [c[0] for c in g.ACCOUNT_COLS]
        for nn in range(100):
            nn_str = f"{nn:02d}"
            for col in expected_cols:
                pattern = f"'account_{nn_str}_shadow', '{col}'"
                self.assertIn(pattern, self.sql,
                              f"missing DROP COLUMN {col} on account_{nn_str}_shadow")

    def test_does_not_touch_production_tables(self):
        for nn in range(100):
            nn_str = f"{nn:02d}"
            self.assertNotIn(
                f"DROP TABLE IF EXISTS `tx_account_anchor_{nn_str}`;",
                self.sql,
                f"shadow rollback leaked production drop for tx_account_anchor_{nn_str}"
            )


if __name__ == "__main__":
    unittest.main(verbosity=2)
