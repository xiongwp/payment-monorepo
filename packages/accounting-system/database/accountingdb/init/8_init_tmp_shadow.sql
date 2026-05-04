-- accounting_db_8 的影子 fleet 平台账户 seed
-- 依赖：8_init_shadow.sql 必须已经导入完成（_shadow 表已建）
-- fleet user_id 段：[9000000000, 9000000000+99]
SET NAMES utf8mb4;
USE `accounting_db_8`;

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (80)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (80)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 9000000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_80_shadow` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('080_PLATFORM_PROFIT_REVENUE', 9000000000 + 80, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_80_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('080_PLATFORM_TCHANNEL_RECEIVABLE', 9000000000 + 80, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_80_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('080_PLATFORM_TCHANNEL_PAYABLE', 9000000000 + 80, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_80_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('080_PLATFORM_TRANSACTION_FEE', 9000000000 + 80, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_80_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('080_PLATFORM_CHARGE_FEE', 9000000000 + 80, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_80_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('080_PLATFORM_TRANSIT', 9000000000 + 80, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (81)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (81)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 9000000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_81_shadow` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('081_PLATFORM_PROFIT_REVENUE', 9000000000 + 81, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_81_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('081_PLATFORM_TCHANNEL_RECEIVABLE', 9000000000 + 81, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_81_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('081_PLATFORM_TCHANNEL_PAYABLE', 9000000000 + 81, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_81_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('081_PLATFORM_TRANSACTION_FEE', 9000000000 + 81, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_81_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('081_PLATFORM_CHARGE_FEE', 9000000000 + 81, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_81_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('081_PLATFORM_TRANSIT', 9000000000 + 81, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (82)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (82)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 9000000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_82_shadow` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('082_PLATFORM_PROFIT_REVENUE', 9000000000 + 82, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_82_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('082_PLATFORM_TCHANNEL_RECEIVABLE', 9000000000 + 82, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_82_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('082_PLATFORM_TCHANNEL_PAYABLE', 9000000000 + 82, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_82_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('082_PLATFORM_TRANSACTION_FEE', 9000000000 + 82, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_82_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('082_PLATFORM_CHARGE_FEE', 9000000000 + 82, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_82_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('082_PLATFORM_TRANSIT', 9000000000 + 82, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (83)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (83)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 9000000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_83_shadow` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('083_PLATFORM_PROFIT_REVENUE', 9000000000 + 83, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_83_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('083_PLATFORM_TCHANNEL_RECEIVABLE', 9000000000 + 83, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_83_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('083_PLATFORM_TCHANNEL_PAYABLE', 9000000000 + 83, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_83_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('083_PLATFORM_TRANSACTION_FEE', 9000000000 + 83, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_83_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('083_PLATFORM_CHARGE_FEE', 9000000000 + 83, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_83_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('083_PLATFORM_TRANSIT', 9000000000 + 83, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (84)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (84)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 9000000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_84_shadow` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('084_PLATFORM_PROFIT_REVENUE', 9000000000 + 84, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_84_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('084_PLATFORM_TCHANNEL_RECEIVABLE', 9000000000 + 84, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_84_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('084_PLATFORM_TCHANNEL_PAYABLE', 9000000000 + 84, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_84_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('084_PLATFORM_TRANSACTION_FEE', 9000000000 + 84, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_84_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('084_PLATFORM_CHARGE_FEE', 9000000000 + 84, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_84_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('084_PLATFORM_TRANSIT', 9000000000 + 84, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (85)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (85)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 9000000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_85_shadow` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('085_PLATFORM_PROFIT_REVENUE', 9000000000 + 85, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_85_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('085_PLATFORM_TCHANNEL_RECEIVABLE', 9000000000 + 85, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_85_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('085_PLATFORM_TCHANNEL_PAYABLE', 9000000000 + 85, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_85_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('085_PLATFORM_TRANSACTION_FEE', 9000000000 + 85, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_85_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('085_PLATFORM_CHARGE_FEE', 9000000000 + 85, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_85_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('085_PLATFORM_TRANSIT', 9000000000 + 85, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (86)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (86)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 9000000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_86_shadow` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('086_PLATFORM_PROFIT_REVENUE', 9000000000 + 86, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_86_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('086_PLATFORM_TCHANNEL_RECEIVABLE', 9000000000 + 86, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_86_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('086_PLATFORM_TCHANNEL_PAYABLE', 9000000000 + 86, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_86_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('086_PLATFORM_TRANSACTION_FEE', 9000000000 + 86, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_86_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('086_PLATFORM_CHARGE_FEE', 9000000000 + 86, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_86_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('086_PLATFORM_TRANSIT', 9000000000 + 86, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (87)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (87)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 9000000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_87_shadow` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('087_PLATFORM_PROFIT_REVENUE', 9000000000 + 87, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_87_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('087_PLATFORM_TCHANNEL_RECEIVABLE', 9000000000 + 87, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_87_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('087_PLATFORM_TCHANNEL_PAYABLE', 9000000000 + 87, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_87_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('087_PLATFORM_TRANSACTION_FEE', 9000000000 + 87, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_87_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('087_PLATFORM_CHARGE_FEE', 9000000000 + 87, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_87_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('087_PLATFORM_TRANSIT', 9000000000 + 87, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (88)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (88)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 9000000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_88_shadow` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('088_PLATFORM_PROFIT_REVENUE', 9000000000 + 88, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_88_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('088_PLATFORM_TCHANNEL_RECEIVABLE', 9000000000 + 88, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_88_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('088_PLATFORM_TCHANNEL_PAYABLE', 9000000000 + 88, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_88_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('088_PLATFORM_TRANSACTION_FEE', 9000000000 + 88, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_88_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('088_PLATFORM_CHARGE_FEE', 9000000000 + 88, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_88_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('088_PLATFORM_TRANSIT', 9000000000 + 88, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (89)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (89)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 9000000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_89_shadow` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('089_PLATFORM_PROFIT_REVENUE', 9000000000 + 89, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_89_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('089_PLATFORM_TCHANNEL_RECEIVABLE', 9000000000 + 89, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_89_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('089_PLATFORM_TCHANNEL_PAYABLE', 9000000000 + 89, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_89_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('089_PLATFORM_TRANSACTION_FEE', 9000000000 + 89, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_89_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('089_PLATFORM_CHARGE_FEE', 9000000000 + 89, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_89_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('089_PLATFORM_TRANSIT', 9000000000 + 89, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

