SET NAMES utf8mb4;
SET CHARACTER SET utf8mb4;
USE `accounting_db_6`;

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (60)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (60)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 1000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_60` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('060_PLATFORM_PROFIT_REVENUE', 1000000 + 60, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_60` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('060_PLATFORM_TCHANNEL_RECEIVABLE', 1000000 + 60, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_60` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('060_PLATFORM_TCHANNEL_PAYABLE', 1000000 + 60, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_60` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('060_PLATFORM_TRANSACTION_FEE', 1000000 + 60, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_60` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('060_PLATFORM_CHARGE_FEE', 1000000 + 60, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_60` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('060_PLATFORM_TRANSIT', 1000000 + 60, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (61)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (61)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 1000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_61` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('061_PLATFORM_PROFIT_REVENUE', 1000000 + 61, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_61` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('061_PLATFORM_TCHANNEL_RECEIVABLE', 1000000 + 61, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_61` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('061_PLATFORM_TCHANNEL_PAYABLE', 1000000 + 61, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_61` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('061_PLATFORM_TRANSACTION_FEE', 1000000 + 61, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_61` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('061_PLATFORM_CHARGE_FEE', 1000000 + 61, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_61` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('061_PLATFORM_TRANSIT', 1000000 + 61, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (62)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (62)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 1000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_62` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('062_PLATFORM_PROFIT_REVENUE', 1000000 + 62, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_62` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('062_PLATFORM_TCHANNEL_RECEIVABLE', 1000000 + 62, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_62` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('062_PLATFORM_TCHANNEL_PAYABLE', 1000000 + 62, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_62` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('062_PLATFORM_TRANSACTION_FEE', 1000000 + 62, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_62` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('062_PLATFORM_CHARGE_FEE', 1000000 + 62, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_62` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('062_PLATFORM_TRANSIT', 1000000 + 62, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (63)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (63)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 1000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_63` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('063_PLATFORM_PROFIT_REVENUE', 1000000 + 63, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_63` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('063_PLATFORM_TCHANNEL_RECEIVABLE', 1000000 + 63, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_63` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('063_PLATFORM_TCHANNEL_PAYABLE', 1000000 + 63, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_63` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('063_PLATFORM_TRANSACTION_FEE', 1000000 + 63, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_63` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('063_PLATFORM_CHARGE_FEE', 1000000 + 63, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_63` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('063_PLATFORM_TRANSIT', 1000000 + 63, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (64)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (64)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 1000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_64` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('064_PLATFORM_PROFIT_REVENUE', 1000000 + 64, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_64` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('064_PLATFORM_TCHANNEL_RECEIVABLE', 1000000 + 64, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_64` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('064_PLATFORM_TCHANNEL_PAYABLE', 1000000 + 64, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_64` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('064_PLATFORM_TRANSACTION_FEE', 1000000 + 64, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_64` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('064_PLATFORM_CHARGE_FEE', 1000000 + 64, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_64` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('064_PLATFORM_TRANSIT', 1000000 + 64, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (65)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (65)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 1000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_65` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('065_PLATFORM_PROFIT_REVENUE', 1000000 + 65, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_65` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('065_PLATFORM_TCHANNEL_RECEIVABLE', 1000000 + 65, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_65` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('065_PLATFORM_TCHANNEL_PAYABLE', 1000000 + 65, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_65` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('065_PLATFORM_TRANSACTION_FEE', 1000000 + 65, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_65` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('065_PLATFORM_CHARGE_FEE', 1000000 + 65, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_65` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('065_PLATFORM_TRANSIT', 1000000 + 65, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (66)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (66)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 1000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_66` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('066_PLATFORM_PROFIT_REVENUE', 1000000 + 66, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_66` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('066_PLATFORM_TCHANNEL_RECEIVABLE', 1000000 + 66, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_66` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('066_PLATFORM_TCHANNEL_PAYABLE', 1000000 + 66, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_66` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('066_PLATFORM_TRANSACTION_FEE', 1000000 + 66, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_66` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('066_PLATFORM_CHARGE_FEE', 1000000 + 66, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_66` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('066_PLATFORM_TRANSIT', 1000000 + 66, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (67)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (67)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 1000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_67` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('067_PLATFORM_PROFIT_REVENUE', 1000000 + 67, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_67` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('067_PLATFORM_TCHANNEL_RECEIVABLE', 1000000 + 67, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_67` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('067_PLATFORM_TCHANNEL_PAYABLE', 1000000 + 67, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_67` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('067_PLATFORM_TRANSACTION_FEE', 1000000 + 67, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_67` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('067_PLATFORM_CHARGE_FEE', 1000000 + 67, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_67` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('067_PLATFORM_TRANSIT', 1000000 + 67, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (68)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (68)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 1000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_68` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('068_PLATFORM_PROFIT_REVENUE', 1000000 + 68, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_68` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('068_PLATFORM_TCHANNEL_RECEIVABLE', 1000000 + 68, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_68` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('068_PLATFORM_TCHANNEL_PAYABLE', 1000000 + 68, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_68` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('068_PLATFORM_TRANSACTION_FEE', 1000000 + 68, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_68` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('068_PLATFORM_CHARGE_FEE', 1000000 + 68, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_68` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('068_PLATFORM_TRANSIT', 1000000 + 68, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (69)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (69)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 1000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_69` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('069_PLATFORM_PROFIT_REVENUE', 1000000 + 69, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_69` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('069_PLATFORM_TCHANNEL_RECEIVABLE', 1000000 + 69, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_69` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('069_PLATFORM_TCHANNEL_PAYABLE', 1000000 + 69, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_69` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('069_PLATFORM_TRANSACTION_FEE', 1000000 + 69, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_69` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('069_PLATFORM_CHARGE_FEE', 1000000 + 69, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_69` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('069_PLATFORM_TRANSIT', 1000000 + 69, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

