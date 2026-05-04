
-- ============================================
-- 初始化平台账户数据
-- ============================================
-- 平台损益账户（资产类账户）
INSERT IGNORE INTO  `account_00` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('PLATFORM_PROFIT_LOSS', 0, 1,3, 'EQUITY', 'PHP', 0.0000, 0.0000, 0.0000, 1);

-- 平台中间账户（用于复式记账过渡）
INSERT IGNORE INTO  `account_00` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('PLATFORM_TRANSIT', 0, 1,4, 'ASSET', 'PHP', 0.0000, 0.0000, 0.0000, 1);
