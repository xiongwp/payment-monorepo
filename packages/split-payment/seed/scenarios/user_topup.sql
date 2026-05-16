-- SP-AC-6 内置 user_topup 场景: 8 个 AccountType + 5 条 TransactionRule.
--
-- 业务: 用户用渠道（支付宝/微信/卡）充值 100 元, 渠道收手续费 0.6 元, 平台收手续费 0.4 元.
-- 用户最终 balance +100, 平台净收入 0.4, 渠道净收入 0.6 (T+N 后从应付清算).
--
-- 装入 accounting-system 的 meta 库:
--   mysql -h shared-meta -uroot -ppassword accounting < user_topup.sql
--
-- 装完后 split-payment 启动会自动加载 (5 min cache).

-- ─── 8 个 AccountTypeInfo ─────────────────────────────────────────────
INSERT INTO account_type_info (account_type, account_type_name, owner_type, is_platform, balance_direction, description)
VALUES
  ('PLATFORM_RECEIVABLE_CHANNEL',  '平台应收-渠道',          3, 1, 'D', '渠道清算前的应收债权 (asset). 100 个子账户分片避免热点.'),
  ('CHANNEL_INBOUND_SUSPENSE',     '渠道入金挂账户',         3, 1, 'C', '渠道结算回执到达后的中间挂账, 准备拆到用户 + 待清算.'),
  ('USER_WALLET',                  '用户钱包',               1, 0, 'C', '用户可用余额 (平台对用户的负债 liability).'),
  ('PLATFORM_FEE_CLEARING',        '平台待清算费用',         3, 1, 'C', '未拆分的手续费, 等待实时清算到 channel_payable + revenue.'),
  ('CHANNEL_FEE_PAYABLE',          '平台应付渠道手续费',     3, 1, 'C', '平台对渠道的应付 (liability), T+N 出账给渠道.'),
  ('PLATFORM_FEE_REVENUE',         '平台手续费收入',         4, 1, 'C', '平台净手续费收入 (revenue+).'),
  ('CHANNEL_PAYABLE_SETTLED',      '平台应付渠道-已清算',    3, 1, 'C', 'T+N 渠道对账后从 fee_payable 搬来, 等真实出账.'),
  ('CHANNEL_BANK_PAYOUT',          '渠道银行出账户',         3, 1, 'D', '真实银行出账动作, 平台真金白银付给渠道 (asset-).');

-- ─── 5 条 TransactionRule (user_topup scenario) ─────────────────────────
-- Phase 1: channel.settled 事件 (3 条 rule)
INSERT INTO transaction_rule (product_code, event_code, hash_key, debit_subject_id, credit_subject_id, from_direction, to_direction, transaction_type, bookkeeping_mode)
VALUES
  -- rule 1: 渠道收到的钱进平台应收 + 进渠道挂账
  ('user_topup', 'channel_settled_receivable',
   'user_topup:channel_settled_receivable',
   'PLATFORM_RECEIVABLE_CHANNEL', 'CHANNEL_INBOUND_SUSPENSE',
   'debit', 'credit', 1, 'standard'),

  -- rule 2: 挂账户给用户 balance 上钱
  ('user_topup', 'channel_settled_to_user',
   'user_topup:channel_settled_to_user',
   'CHANNEL_INBOUND_SUSPENSE', 'USER_WALLET',
   'debit', 'credit', 1, 'standard'),

  -- rule 3: 挂账户剩余进待清算
  ('user_topup', 'channel_settled_fee_pending',
   'user_topup:channel_settled_fee_pending',
   'CHANNEL_INBOUND_SUSPENSE', 'PLATFORM_FEE_CLEARING',
   'debit', 'credit', 1, 'standard'),

-- Phase 2: fee.cleared 事件 (2 条 rule, 实时清算 auto_clear=true)
  -- rule 4: 待清算拆出 → 平台应付渠道
  ('user_topup', 'fee_cleared_to_channel_payable',
   'user_topup:fee_cleared_to_channel_payable',
   'PLATFORM_FEE_CLEARING', 'CHANNEL_FEE_PAYABLE',
   'debit', 'credit', 1, 'standard'),

  -- rule 5: 待清算拆出 → 平台 fee 收入
  ('user_topup', 'fee_cleared_to_revenue',
   'user_topup:fee_cleared_to_revenue',
   'PLATFORM_FEE_CLEARING', 'PLATFORM_FEE_REVENUE',
   'debit', 'credit', 1, 'standard');

-- ─── (可选) Phase 3: T+N 真实出账 ────────────────────────────────────
-- INSERT INTO transaction_rule VALUES
--   ('user_topup', 'channel_payout_settled',  ... CHANNEL_FEE_PAYABLE → CHANNEL_PAYABLE_SETTLED ...);
--   ('user_topup', 'channel_bank_out',        ... CHANNEL_PAYABLE_SETTLED → CHANNEL_BANK_PAYOUT ...);

-- ─── 自检 ────────────────────────────────────────────────────────────
-- SELECT * FROM transaction_rule WHERE product_code='user_topup' ORDER BY id;
-- 期望 5 行.
