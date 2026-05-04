-- accounting_db_9 的影子 fleet 平台账户 seed
-- 依赖：9_init_shadow.sql 必须已经导入完成（_shadow 表已建）
-- fleet user_id 段：[9000000000, 9000000000+99]
SET NAMES utf8mb4;
USE `accounting_db_9`;

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (90)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (90)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 9000000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_90_shadow` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('090_PLATFORM_PROFIT_REVENUE', 9000000000 + 90, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_90_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('090_PLATFORM_TCHANNEL_RECEIVABLE', 9000000000 + 90, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_90_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('090_PLATFORM_TCHANNEL_PAYABLE', 9000000000 + 90, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_90_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('090_PLATFORM_TRANSACTION_FEE', 9000000000 + 90, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_90_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('090_PLATFORM_CHARGE_FEE', 9000000000 + 90, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_90_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('090_PLATFORM_TRANSIT', 9000000000 + 90, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (91)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (91)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 9000000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_91_shadow` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('091_PLATFORM_PROFIT_REVENUE', 9000000000 + 91, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_91_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('091_PLATFORM_TCHANNEL_RECEIVABLE', 9000000000 + 91, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_91_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('091_PLATFORM_TCHANNEL_PAYABLE', 9000000000 + 91, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_91_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('091_PLATFORM_TRANSACTION_FEE', 9000000000 + 91, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_91_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('091_PLATFORM_CHARGE_FEE', 9000000000 + 91, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_91_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('091_PLATFORM_TRANSIT', 9000000000 + 91, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (92)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (92)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 9000000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_92_shadow` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('092_PLATFORM_PROFIT_REVENUE', 9000000000 + 92, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_92_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('092_PLATFORM_TCHANNEL_RECEIVABLE', 9000000000 + 92, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_92_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('092_PLATFORM_TCHANNEL_PAYABLE', 9000000000 + 92, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_92_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('092_PLATFORM_TRANSACTION_FEE', 9000000000 + 92, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_92_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('092_PLATFORM_CHARGE_FEE', 9000000000 + 92, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_92_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('092_PLATFORM_TRANSIT', 9000000000 + 92, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (93)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (93)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 9000000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_93_shadow` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('093_PLATFORM_PROFIT_REVENUE', 9000000000 + 93, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_93_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('093_PLATFORM_TCHANNEL_RECEIVABLE', 9000000000 + 93, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_93_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('093_PLATFORM_TCHANNEL_PAYABLE', 9000000000 + 93, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_93_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('093_PLATFORM_TRANSACTION_FEE', 9000000000 + 93, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_93_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('093_PLATFORM_CHARGE_FEE', 9000000000 + 93, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_93_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('093_PLATFORM_TRANSIT', 9000000000 + 93, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (94)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (94)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 9000000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_94_shadow` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('094_PLATFORM_PROFIT_REVENUE', 9000000000 + 94, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_94_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('094_PLATFORM_TCHANNEL_RECEIVABLE', 9000000000 + 94, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_94_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('094_PLATFORM_TCHANNEL_PAYABLE', 9000000000 + 94, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_94_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('094_PLATFORM_TRANSACTION_FEE', 9000000000 + 94, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_94_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('094_PLATFORM_CHARGE_FEE', 9000000000 + 94, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_94_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('094_PLATFORM_TRANSIT', 9000000000 + 94, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (95)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (95)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 9000000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_95_shadow` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('095_PLATFORM_PROFIT_REVENUE', 9000000000 + 95, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_95_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('095_PLATFORM_TCHANNEL_RECEIVABLE', 9000000000 + 95, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_95_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('095_PLATFORM_TCHANNEL_PAYABLE', 9000000000 + 95, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_95_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('095_PLATFORM_TRANSACTION_FEE', 9000000000 + 95, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_95_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('095_PLATFORM_CHARGE_FEE', 9000000000 + 95, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_95_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('095_PLATFORM_TRANSIT', 9000000000 + 95, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (96)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (96)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 9000000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_96_shadow` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('096_PLATFORM_PROFIT_REVENUE', 9000000000 + 96, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_96_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('096_PLATFORM_TCHANNEL_RECEIVABLE', 9000000000 + 96, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_96_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('096_PLATFORM_TCHANNEL_PAYABLE', 9000000000 + 96, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_96_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('096_PLATFORM_TRANSACTION_FEE', 9000000000 + 96, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_96_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('096_PLATFORM_CHARGE_FEE', 9000000000 + 96, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_96_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('096_PLATFORM_TRANSIT', 9000000000 + 96, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (97)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (97)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 9000000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_97_shadow` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('097_PLATFORM_PROFIT_REVENUE', 9000000000 + 97, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_97_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('097_PLATFORM_TCHANNEL_RECEIVABLE', 9000000000 + 97, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_97_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('097_PLATFORM_TCHANNEL_PAYABLE', 9000000000 + 97, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_97_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('097_PLATFORM_TRANSACTION_FEE', 9000000000 + 97, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_97_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('097_PLATFORM_CHARGE_FEE', 9000000000 + 97, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_97_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('097_PLATFORM_TRANSIT', 9000000000 + 97, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (98)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (98)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 9000000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_98_shadow` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('098_PLATFORM_PROFIT_REVENUE', 9000000000 + 98, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_98_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('098_PLATFORM_TCHANNEL_RECEIVABLE', 9000000000 + 98, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_98_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('098_PLATFORM_TCHANNEL_PAYABLE', 9000000000 + 98, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_98_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('098_PLATFORM_TRANSACTION_FEE', 9000000000 + 98, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_98_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('098_PLATFORM_CHARGE_FEE', 9000000000 + 98, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_98_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('098_PLATFORM_TRANSIT', 9000000000 + 98, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

-- ============================================
-- 初始化平台 fleet 账户数据
-- ============================================
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin (1_000_000) + globalTableIdx (99)
--   shadow ：ShadowFleetUserIDMin (9_000_000_000) + globalTableIdx (99)
--           shadow 版本由 generate.sh 单独生成，写入 *_init_tmp_shadow.sql。
--
-- 主流量 fleet 段 [1_000_000, 9_999_999] 共 900 万容量，每个 globalTableIdx
-- (0-99) 占用一个 user_id（fleet account 当前 6 种类型 × 100 分片 = 600 个，
-- 留充足空间给未来加 platform 账户类型 / 扩 shard）。
--
-- 9000000000 由 generate.sh 替换：主流量 = 1_000_000，shadow = 9_000_000_000。

-- 平台损益账户（资产类账户) 平台手续费收入
INSERT IGNORE INTO  `account_99_shadow` (`account_no`, `user_id`, `account_business_type`,`account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('099_PLATFORM_PROFIT_REVENUE', 9000000000 + 99, 4, 4, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应收款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_RECEIVABLE
INSERT IGNORE INTO `account_99_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('099_PLATFORM_TCHANNEL_RECEIVABLE', 9000000000 + 99, 5, 5, 'ASSET', 'PHP',0,0,0,1);

-- 平台中间账户（用于复式记账过渡 渠道应付款) AccountType_ACCOUNT_TYPE_TRANSIT_CHANNEL_PAYABLE
INSERT IGNORE INTO `account_99_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('099_PLATFORM_TCHANNEL_PAYABLE', 9000000000 + 99, 6, 6, 'LIABILITY', 'PHP',0,0,0,1);

-- 平台收益 AccountBusinessType_ACCOUNT_BUSINESS_TYPE_TRANSACTION_FEE
INSERT IGNORE INTO `account_99_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('099_PLATFORM_TRANSACTION_FEE', 9000000000 + 99, 7, 7, 'REVENUE', 'PHP',0,0,0,1);

-- 平台收益  AccountBusinessType_ACCOUNT_BUSINESS_TYPE_CHARGE_FEE
INSERT IGNORE INTO `account_99_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('099_PLATFORM_CHARGE_FEE', 9000000000 + 99, 8, 8, 'REVENUE', 'PHP',0,0,0,1);

-- 平台中间账户  ACCOUNT_TYPE_TRANSIT
INSERT IGNORE INTO `account_99_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES ('099_PLATFORM_TRANSIT', 9000000000 + 99, 9, 9, 'LIABILITY', 'PHP',0,0,0,1);

