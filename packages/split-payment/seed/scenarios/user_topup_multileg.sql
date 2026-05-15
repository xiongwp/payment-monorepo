-- user_topup_multileg.sql — SP-AC-7 multi-leg 版本的 TransactionRule.
--
-- 跟 user_topup.sql (5 条 single-leg rule) 对比:
--   user_topup.sql                     → 5 个 event_code, 每个 1 条 leg, accounting 落 5 个 voucher
--   user_topup_multileg.sql (此文件)   → 2 个 event_code, 每个多条 leg, accounting 落 2 个 voucher
--
-- 装入 accounting-system 的 meta 库:
--   mysql -h shared-meta -uroot -ppassword accounting < user_topup_multileg.sql
--
-- 注意: 在 SP-AC-7 multi-leg 设计下, TransactionRule.debit_subject_id / credit_subject_id 退化为
--       *元数据/校验提示*; 真实落账分录由 translator 输出的 legs 决定. 这里填的 subject 选 "第一条
--       leg 的代表性 subject", 用于 admin UI 展示和 sanity-check.

-- ─── 8 个 AccountTypeInfo (跟 user_topup.sql 完全一致, 不重复 INSERT) ──
-- 见 user_topup.sql; 如果 meta 库还没装, 先装 user_topup.sql 再装这个.

-- ─── 2 条 TransactionRule (合并版) ───────────────────────────────────
--
-- 关键点: 一个 event_code 现在对应 N 条 edge (N >= 1) 的原子操作, 不再 1:1.
-- accounting-system 的 GetRulesByProductAndEvent 只用来校验 event_code 是否合法,
-- legs 由 split-payment translator 提供, executeBookkeeping 拆 2N entries 一次 DoubleEntryBooking.

INSERT INTO transaction_rule
  (product_code, event_code, hash_key, debit_subject_id, credit_subject_id, from_direction, to_direction, transaction_type, bookkeeping_mode)
VALUES
  -- Phase 1: channel.settled 触发, 3 条 edge 一次原子落账
  --   leg 1: PLATFORM_RECEIVABLE_CHANNEL → CHANNEL_INBOUND_SUSPENSE (¥100)
  --   leg 2: CHANNEL_INBOUND_SUSPENSE    → USER_WALLET              (¥99)
  --   leg 3: CHANNEL_INBOUND_SUSPENSE    → PLATFORM_FEE_CLEARING    (¥1)
  ('user_topup', 'channel_settled',
   'user_topup:channel_settled',
   'PLATFORM_RECEIVABLE_CHANNEL', 'USER_WALLET',
   'debit', 'credit', 1, 'standard'),

  -- Phase 2: fee.cleared 触发, 2 条 edge 一次原子清算
  --   leg 1: PLATFORM_FEE_CLEARING → CHANNEL_FEE_PAYABLE (¥0.60)
  --   leg 2: PLATFORM_FEE_CLEARING → PLATFORM_FEE_REVENUE (¥0.40)
  ('user_topup', 'fee_cleared',
   'user_topup:fee_cleared',
   'PLATFORM_FEE_CLEARING', 'PLATFORM_FEE_REVENUE',
   'debit', 'credit', 1, 'standard');

-- ─── 自检 ────────────────────────────────────────────────────────────
-- SELECT product_code, event_code, debit_subject_id, credit_subject_id
--   FROM transaction_rule
--  WHERE product_code='user_topup'
--    AND event_code IN ('channel_settled', 'fee_cleared')
--  ORDER BY id;
-- 期望 2 行.
