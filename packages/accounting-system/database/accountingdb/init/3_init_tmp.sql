SET NAMES utf8mb4;
SET CHARACTER SET utf8mb4;
USE `accounting_db_3`;

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (30)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (30)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 1000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_30` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('030_PLATFORM_PROFIT_REVENUE', 1000000 + 30, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_30` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('030_PLATFORM_TCHANNEL_RECEIVABLE', 1000000 + 30, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_30` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('030_PLATFORM_TCHANNEL_PAYABLE', 1000000 + 30, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_30` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('030_PLATFORM_TRANSACTION_FEE', 1000000 + 30, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_30` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('030_PLATFORM_CHARGE_FEE', 1000000 + 30, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_30` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('030_PLATFORM_TRANSIT', 1000000 + 30, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (31)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (31)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 1000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_31` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('031_PLATFORM_PROFIT_REVENUE', 1000000 + 31, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_31` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('031_PLATFORM_TCHANNEL_RECEIVABLE', 1000000 + 31, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_31` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('031_PLATFORM_TCHANNEL_PAYABLE', 1000000 + 31, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_31` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('031_PLATFORM_TRANSACTION_FEE', 1000000 + 31, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_31` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('031_PLATFORM_CHARGE_FEE', 1000000 + 31, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_31` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('031_PLATFORM_TRANSIT', 1000000 + 31, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (32)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (32)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 1000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_32` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('032_PLATFORM_PROFIT_REVENUE', 1000000 + 32, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_32` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('032_PLATFORM_TCHANNEL_RECEIVABLE', 1000000 + 32, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_32` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('032_PLATFORM_TCHANNEL_PAYABLE', 1000000 + 32, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_32` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('032_PLATFORM_TRANSACTION_FEE', 1000000 + 32, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_32` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('032_PLATFORM_CHARGE_FEE', 1000000 + 32, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_32` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('032_PLATFORM_TRANSIT', 1000000 + 32, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (33)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (33)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 1000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_33` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('033_PLATFORM_PROFIT_REVENUE', 1000000 + 33, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_33` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('033_PLATFORM_TCHANNEL_RECEIVABLE', 1000000 + 33, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_33` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('033_PLATFORM_TCHANNEL_PAYABLE', 1000000 + 33, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_33` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('033_PLATFORM_TRANSACTION_FEE', 1000000 + 33, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_33` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('033_PLATFORM_CHARGE_FEE', 1000000 + 33, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_33` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('033_PLATFORM_TRANSIT', 1000000 + 33, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (34)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (34)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 1000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_34` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('034_PLATFORM_PROFIT_REVENUE', 1000000 + 34, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_34` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('034_PLATFORM_TCHANNEL_RECEIVABLE', 1000000 + 34, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_34` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('034_PLATFORM_TCHANNEL_PAYABLE', 1000000 + 34, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_34` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('034_PLATFORM_TRANSACTION_FEE', 1000000 + 34, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_34` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('034_PLATFORM_CHARGE_FEE', 1000000 + 34, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_34` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('034_PLATFORM_TRANSIT', 1000000 + 34, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (35)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (35)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 1000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_35` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('035_PLATFORM_PROFIT_REVENUE', 1000000 + 35, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_35` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('035_PLATFORM_TCHANNEL_RECEIVABLE', 1000000 + 35, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_35` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('035_PLATFORM_TCHANNEL_PAYABLE', 1000000 + 35, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_35` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('035_PLATFORM_TRANSACTION_FEE', 1000000 + 35, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_35` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('035_PLATFORM_CHARGE_FEE', 1000000 + 35, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_35` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('035_PLATFORM_TRANSIT', 1000000 + 35, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (36)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (36)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 1000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_36` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('036_PLATFORM_PROFIT_REVENUE', 1000000 + 36, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_36` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('036_PLATFORM_TCHANNEL_RECEIVABLE', 1000000 + 36, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_36` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('036_PLATFORM_TCHANNEL_PAYABLE', 1000000 + 36, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_36` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('036_PLATFORM_TRANSACTION_FEE', 1000000 + 36, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_36` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('036_PLATFORM_CHARGE_FEE', 1000000 + 36, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_36` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('036_PLATFORM_TRANSIT', 1000000 + 36, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (37)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (37)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 1000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_37` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('037_PLATFORM_PROFIT_REVENUE', 1000000 + 37, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_37` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('037_PLATFORM_TCHANNEL_RECEIVABLE', 1000000 + 37, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_37` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('037_PLATFORM_TCHANNEL_PAYABLE', 1000000 + 37, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_37` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('037_PLATFORM_TRANSACTION_FEE', 1000000 + 37, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_37` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('037_PLATFORM_CHARGE_FEE', 1000000 + 37, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_37` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('037_PLATFORM_TRANSIT', 1000000 + 37, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (38)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (38)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 1000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_38` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('038_PLATFORM_PROFIT_REVENUE', 1000000 + 38, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_38` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('038_PLATFORM_TCHANNEL_RECEIVABLE', 1000000 + 38, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_38` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('038_PLATFORM_TCHANNEL_PAYABLE', 1000000 + 38, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_38` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('038_PLATFORM_TRANSACTION_FEE', 1000000 + 38, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_38` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('038_PLATFORM_CHARGE_FEE', 1000000 + 38, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_38` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('038_PLATFORM_TRANSIT', 1000000 + 38, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (39)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (39)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 1000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_39` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('039_PLATFORM_PROFIT_REVENUE', 1000000 + 39, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_39` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('039_PLATFORM_TCHANNEL_RECEIVABLE', 1000000 + 39, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_39` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('039_PLATFORM_TCHANNEL_PAYABLE', 1000000 + 39, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_39` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('039_PLATFORM_TRANSACTION_FEE', 1000000 + 39, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_39` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('039_PLATFORM_CHARGE_FEE', 1000000 + 39, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_39` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('039_PLATFORM_TRANSIT', 1000000 + 39, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

