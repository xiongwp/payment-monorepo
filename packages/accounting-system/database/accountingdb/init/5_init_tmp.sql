SET NAMES utf8mb4;
SET CHARACTER SET utf8mb4;
USE `accounting_db_5`;

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (50)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (50)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 1000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_50` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('050_PLATFORM_PROFIT_REVENUE', 1000000 + 50, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_50` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('050_PLATFORM_TCHANNEL_RECEIVABLE', 1000000 + 50, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_50` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('050_PLATFORM_TCHANNEL_PAYABLE', 1000000 + 50, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_50` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('050_PLATFORM_TRANSACTION_FEE', 1000000 + 50, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_50` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('050_PLATFORM_CHARGE_FEE', 1000000 + 50, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_50` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('050_PLATFORM_TRANSIT', 1000000 + 50, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (51)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (51)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 1000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_51` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('051_PLATFORM_PROFIT_REVENUE', 1000000 + 51, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_51` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('051_PLATFORM_TCHANNEL_RECEIVABLE', 1000000 + 51, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_51` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('051_PLATFORM_TCHANNEL_PAYABLE', 1000000 + 51, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_51` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('051_PLATFORM_TRANSACTION_FEE', 1000000 + 51, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_51` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('051_PLATFORM_CHARGE_FEE', 1000000 + 51, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_51` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('051_PLATFORM_TRANSIT', 1000000 + 51, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (52)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (52)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 1000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_52` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('052_PLATFORM_PROFIT_REVENUE', 1000000 + 52, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_52` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('052_PLATFORM_TCHANNEL_RECEIVABLE', 1000000 + 52, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_52` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('052_PLATFORM_TCHANNEL_PAYABLE', 1000000 + 52, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_52` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('052_PLATFORM_TRANSACTION_FEE', 1000000 + 52, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_52` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('052_PLATFORM_CHARGE_FEE', 1000000 + 52, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_52` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('052_PLATFORM_TRANSIT', 1000000 + 52, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (53)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (53)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 1000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_53` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('053_PLATFORM_PROFIT_REVENUE', 1000000 + 53, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_53` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('053_PLATFORM_TCHANNEL_RECEIVABLE', 1000000 + 53, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_53` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('053_PLATFORM_TCHANNEL_PAYABLE', 1000000 + 53, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_53` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('053_PLATFORM_TRANSACTION_FEE', 1000000 + 53, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_53` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('053_PLATFORM_CHARGE_FEE', 1000000 + 53, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_53` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('053_PLATFORM_TRANSIT', 1000000 + 53, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (54)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (54)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 1000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_54` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('054_PLATFORM_PROFIT_REVENUE', 1000000 + 54, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_54` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('054_PLATFORM_TCHANNEL_RECEIVABLE', 1000000 + 54, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_54` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('054_PLATFORM_TCHANNEL_PAYABLE', 1000000 + 54, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_54` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('054_PLATFORM_TRANSACTION_FEE', 1000000 + 54, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_54` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('054_PLATFORM_CHARGE_FEE', 1000000 + 54, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_54` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('054_PLATFORM_TRANSIT', 1000000 + 54, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (55)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (55)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 1000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_55` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('055_PLATFORM_PROFIT_REVENUE', 1000000 + 55, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_55` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('055_PLATFORM_TCHANNEL_RECEIVABLE', 1000000 + 55, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_55` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('055_PLATFORM_TCHANNEL_PAYABLE', 1000000 + 55, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_55` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('055_PLATFORM_TRANSACTION_FEE', 1000000 + 55, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_55` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('055_PLATFORM_CHARGE_FEE', 1000000 + 55, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_55` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('055_PLATFORM_TRANSIT', 1000000 + 55, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (56)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (56)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 1000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_56` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('056_PLATFORM_PROFIT_REVENUE', 1000000 + 56, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_56` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('056_PLATFORM_TCHANNEL_RECEIVABLE', 1000000 + 56, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_56` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('056_PLATFORM_TCHANNEL_PAYABLE', 1000000 + 56, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_56` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('056_PLATFORM_TRANSACTION_FEE', 1000000 + 56, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_56` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('056_PLATFORM_CHARGE_FEE', 1000000 + 56, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_56` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('056_PLATFORM_TRANSIT', 1000000 + 56, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (57)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (57)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 1000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_57` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('057_PLATFORM_PROFIT_REVENUE', 1000000 + 57, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_57` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('057_PLATFORM_TCHANNEL_RECEIVABLE', 1000000 + 57, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_57` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('057_PLATFORM_TCHANNEL_PAYABLE', 1000000 + 57, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_57` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('057_PLATFORM_TRANSACTION_FEE', 1000000 + 57, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_57` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('057_PLATFORM_CHARGE_FEE', 1000000 + 57, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_57` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('057_PLATFORM_TRANSIT', 1000000 + 57, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (58)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (58)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 1000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_58` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('058_PLATFORM_PROFIT_REVENUE', 1000000 + 58, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_58` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('058_PLATFORM_TCHANNEL_RECEIVABLE', 1000000 + 58, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_58` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('058_PLATFORM_TCHANNEL_PAYABLE', 1000000 + 58, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_58` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('058_PLATFORM_TRANSACTION_FEE', 1000000 + 58, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_58` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('058_PLATFORM_CHARGE_FEE', 1000000 + 58, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_58` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('058_PLATFORM_TRANSIT', 1000000 + 58, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (59)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (59)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 1000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_59` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('059_PLATFORM_PROFIT_REVENUE', 1000000 + 59, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_59` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('059_PLATFORM_TCHANNEL_RECEIVABLE', 1000000 + 59, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_59` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('059_PLATFORM_TCHANNEL_PAYABLE', 1000000 + 59, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_59` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('059_PLATFORM_TRANSACTION_FEE', 1000000 + 59, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_59` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('059_PLATFORM_CHARGE_FEE', 1000000 + 59, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_59` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('059_PLATFORM_TRANSIT', 1000000 + 59, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

