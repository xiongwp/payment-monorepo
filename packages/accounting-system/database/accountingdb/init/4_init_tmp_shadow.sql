-- accounting_db_4 的影子 fleet 平台账户 seed
-- 依赖：4_init_shadow.sql 必须已经导入完成（_shadow 表已建）
-- fleet user_id 段：[9000000000, 9000000000+99]
SET NAMES utf8mb4;
USE `accounting_db_4`;

-- ============================================
-- 初始化平台 fleet 账户数据 (按位编码 account_no)
-- ============================================
--
-- account_no 19 位 layout（与 payment-util/shadow.EncodeAccountID 对齐）：
--   shadow(1) | currency(3) | accountType(2) | globalTbl(2) | businessType(4) | seq(7)
--
-- 平台 fleet 账户的字段固定值：
--   currency      = 608 (PHP, ISO 4217)
--   globalTbl     = 40 (00..99，本分片表序号)
--   accountType   = 4..9 (PLATFORM / TRANSITRECEIVE / TRANSITPAYABLE /
--                          TRANSACTIONFEE / CHARGEFEE / TRANSIT)
--   businessType  = 4..9 (与 accountType 一一对应，见 platformAccountSpec)
--   seq           = 1 (每 (currency, type, gtbl, biz) 组合下只有 1 个 fleet 账户)
--   shadow        = 1 (0 主流量 / 1 shadow)
--
-- 算式：
--   account_no = shadow * 1e18 + currency * 1e15 + accountType * 1e13 +
--                globalTbl * 1e11 + businessType * 1e7 + seq
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (40)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (40)
--
-- 9000000000 / 1 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_40_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 40 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  9000000000 + 40, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_40_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 40 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  9000000000 + 40, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_40_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 40 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  9000000000 + 40, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_40_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 40 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  9000000000 + 40, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_40_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 40 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  9000000000 + 40, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_40_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 40 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  9000000000 + 40, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- ============================================
-- 初始化平台 fleet 账户数据 (按位编码 account_no)
-- ============================================
--
-- account_no 19 位 layout（与 payment-util/shadow.EncodeAccountID 对齐）：
--   shadow(1) | currency(3) | accountType(2) | globalTbl(2) | businessType(4) | seq(7)
--
-- 平台 fleet 账户的字段固定值：
--   currency      = 608 (PHP, ISO 4217)
--   globalTbl     = 41 (00..99，本分片表序号)
--   accountType   = 4..9 (PLATFORM / TRANSITRECEIVE / TRANSITPAYABLE /
--                          TRANSACTIONFEE / CHARGEFEE / TRANSIT)
--   businessType  = 4..9 (与 accountType 一一对应，见 platformAccountSpec)
--   seq           = 1 (每 (currency, type, gtbl, biz) 组合下只有 1 个 fleet 账户)
--   shadow        = 1 (0 主流量 / 1 shadow)
--
-- 算式：
--   account_no = shadow * 1e18 + currency * 1e15 + accountType * 1e13 +
--                globalTbl * 1e11 + businessType * 1e7 + seq
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (41)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (41)
--
-- 9000000000 / 1 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_41_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 41 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  9000000000 + 41, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_41_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 41 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  9000000000 + 41, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_41_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 41 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  9000000000 + 41, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_41_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 41 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  9000000000 + 41, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_41_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 41 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  9000000000 + 41, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_41_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 41 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  9000000000 + 41, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- ============================================
-- 初始化平台 fleet 账户数据 (按位编码 account_no)
-- ============================================
--
-- account_no 19 位 layout（与 payment-util/shadow.EncodeAccountID 对齐）：
--   shadow(1) | currency(3) | accountType(2) | globalTbl(2) | businessType(4) | seq(7)
--
-- 平台 fleet 账户的字段固定值：
--   currency      = 608 (PHP, ISO 4217)
--   globalTbl     = 42 (00..99，本分片表序号)
--   accountType   = 4..9 (PLATFORM / TRANSITRECEIVE / TRANSITPAYABLE /
--                          TRANSACTIONFEE / CHARGEFEE / TRANSIT)
--   businessType  = 4..9 (与 accountType 一一对应，见 platformAccountSpec)
--   seq           = 1 (每 (currency, type, gtbl, biz) 组合下只有 1 个 fleet 账户)
--   shadow        = 1 (0 主流量 / 1 shadow)
--
-- 算式：
--   account_no = shadow * 1e18 + currency * 1e15 + accountType * 1e13 +
--                globalTbl * 1e11 + businessType * 1e7 + seq
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (42)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (42)
--
-- 9000000000 / 1 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_42_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 42 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  9000000000 + 42, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_42_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 42 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  9000000000 + 42, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_42_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 42 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  9000000000 + 42, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_42_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 42 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  9000000000 + 42, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_42_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 42 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  9000000000 + 42, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_42_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 42 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  9000000000 + 42, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- ============================================
-- 初始化平台 fleet 账户数据 (按位编码 account_no)
-- ============================================
--
-- account_no 19 位 layout（与 payment-util/shadow.EncodeAccountID 对齐）：
--   shadow(1) | currency(3) | accountType(2) | globalTbl(2) | businessType(4) | seq(7)
--
-- 平台 fleet 账户的字段固定值：
--   currency      = 608 (PHP, ISO 4217)
--   globalTbl     = 43 (00..99，本分片表序号)
--   accountType   = 4..9 (PLATFORM / TRANSITRECEIVE / TRANSITPAYABLE /
--                          TRANSACTIONFEE / CHARGEFEE / TRANSIT)
--   businessType  = 4..9 (与 accountType 一一对应，见 platformAccountSpec)
--   seq           = 1 (每 (currency, type, gtbl, biz) 组合下只有 1 个 fleet 账户)
--   shadow        = 1 (0 主流量 / 1 shadow)
--
-- 算式：
--   account_no = shadow * 1e18 + currency * 1e15 + accountType * 1e13 +
--                globalTbl * 1e11 + businessType * 1e7 + seq
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (43)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (43)
--
-- 9000000000 / 1 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_43_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 43 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  9000000000 + 43, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_43_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 43 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  9000000000 + 43, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_43_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 43 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  9000000000 + 43, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_43_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 43 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  9000000000 + 43, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_43_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 43 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  9000000000 + 43, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_43_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 43 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  9000000000 + 43, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- ============================================
-- 初始化平台 fleet 账户数据 (按位编码 account_no)
-- ============================================
--
-- account_no 19 位 layout（与 payment-util/shadow.EncodeAccountID 对齐）：
--   shadow(1) | currency(3) | accountType(2) | globalTbl(2) | businessType(4) | seq(7)
--
-- 平台 fleet 账户的字段固定值：
--   currency      = 608 (PHP, ISO 4217)
--   globalTbl     = 44 (00..99，本分片表序号)
--   accountType   = 4..9 (PLATFORM / TRANSITRECEIVE / TRANSITPAYABLE /
--                          TRANSACTIONFEE / CHARGEFEE / TRANSIT)
--   businessType  = 4..9 (与 accountType 一一对应，见 platformAccountSpec)
--   seq           = 1 (每 (currency, type, gtbl, biz) 组合下只有 1 个 fleet 账户)
--   shadow        = 1 (0 主流量 / 1 shadow)
--
-- 算式：
--   account_no = shadow * 1e18 + currency * 1e15 + accountType * 1e13 +
--                globalTbl * 1e11 + businessType * 1e7 + seq
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (44)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (44)
--
-- 9000000000 / 1 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_44_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 44 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  9000000000 + 44, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_44_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 44 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  9000000000 + 44, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_44_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 44 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  9000000000 + 44, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_44_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 44 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  9000000000 + 44, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_44_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 44 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  9000000000 + 44, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_44_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 44 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  9000000000 + 44, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- ============================================
-- 初始化平台 fleet 账户数据 (按位编码 account_no)
-- ============================================
--
-- account_no 19 位 layout（与 payment-util/shadow.EncodeAccountID 对齐）：
--   shadow(1) | currency(3) | accountType(2) | globalTbl(2) | businessType(4) | seq(7)
--
-- 平台 fleet 账户的字段固定值：
--   currency      = 608 (PHP, ISO 4217)
--   globalTbl     = 45 (00..99，本分片表序号)
--   accountType   = 4..9 (PLATFORM / TRANSITRECEIVE / TRANSITPAYABLE /
--                          TRANSACTIONFEE / CHARGEFEE / TRANSIT)
--   businessType  = 4..9 (与 accountType 一一对应，见 platformAccountSpec)
--   seq           = 1 (每 (currency, type, gtbl, biz) 组合下只有 1 个 fleet 账户)
--   shadow        = 1 (0 主流量 / 1 shadow)
--
-- 算式：
--   account_no = shadow * 1e18 + currency * 1e15 + accountType * 1e13 +
--                globalTbl * 1e11 + businessType * 1e7 + seq
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (45)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (45)
--
-- 9000000000 / 1 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_45_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 45 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  9000000000 + 45, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_45_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 45 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  9000000000 + 45, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_45_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 45 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  9000000000 + 45, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_45_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 45 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  9000000000 + 45, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_45_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 45 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  9000000000 + 45, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_45_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 45 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  9000000000 + 45, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- ============================================
-- 初始化平台 fleet 账户数据 (按位编码 account_no)
-- ============================================
--
-- account_no 19 位 layout（与 payment-util/shadow.EncodeAccountID 对齐）：
--   shadow(1) | currency(3) | accountType(2) | globalTbl(2) | businessType(4) | seq(7)
--
-- 平台 fleet 账户的字段固定值：
--   currency      = 608 (PHP, ISO 4217)
--   globalTbl     = 46 (00..99，本分片表序号)
--   accountType   = 4..9 (PLATFORM / TRANSITRECEIVE / TRANSITPAYABLE /
--                          TRANSACTIONFEE / CHARGEFEE / TRANSIT)
--   businessType  = 4..9 (与 accountType 一一对应，见 platformAccountSpec)
--   seq           = 1 (每 (currency, type, gtbl, biz) 组合下只有 1 个 fleet 账户)
--   shadow        = 1 (0 主流量 / 1 shadow)
--
-- 算式：
--   account_no = shadow * 1e18 + currency * 1e15 + accountType * 1e13 +
--                globalTbl * 1e11 + businessType * 1e7 + seq
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (46)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (46)
--
-- 9000000000 / 1 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_46_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 46 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  9000000000 + 46, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_46_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 46 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  9000000000 + 46, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_46_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 46 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  9000000000 + 46, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_46_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 46 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  9000000000 + 46, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_46_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 46 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  9000000000 + 46, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_46_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 46 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  9000000000 + 46, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- ============================================
-- 初始化平台 fleet 账户数据 (按位编码 account_no)
-- ============================================
--
-- account_no 19 位 layout（与 payment-util/shadow.EncodeAccountID 对齐）：
--   shadow(1) | currency(3) | accountType(2) | globalTbl(2) | businessType(4) | seq(7)
--
-- 平台 fleet 账户的字段固定值：
--   currency      = 608 (PHP, ISO 4217)
--   globalTbl     = 47 (00..99，本分片表序号)
--   accountType   = 4..9 (PLATFORM / TRANSITRECEIVE / TRANSITPAYABLE /
--                          TRANSACTIONFEE / CHARGEFEE / TRANSIT)
--   businessType  = 4..9 (与 accountType 一一对应，见 platformAccountSpec)
--   seq           = 1 (每 (currency, type, gtbl, biz) 组合下只有 1 个 fleet 账户)
--   shadow        = 1 (0 主流量 / 1 shadow)
--
-- 算式：
--   account_no = shadow * 1e18 + currency * 1e15 + accountType * 1e13 +
--                globalTbl * 1e11 + businessType * 1e7 + seq
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (47)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (47)
--
-- 9000000000 / 1 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_47_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 47 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  9000000000 + 47, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_47_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 47 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  9000000000 + 47, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_47_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 47 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  9000000000 + 47, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_47_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 47 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  9000000000 + 47, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_47_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 47 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  9000000000 + 47, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_47_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 47 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  9000000000 + 47, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- ============================================
-- 初始化平台 fleet 账户数据 (按位编码 account_no)
-- ============================================
--
-- account_no 19 位 layout（与 payment-util/shadow.EncodeAccountID 对齐）：
--   shadow(1) | currency(3) | accountType(2) | globalTbl(2) | businessType(4) | seq(7)
--
-- 平台 fleet 账户的字段固定值：
--   currency      = 608 (PHP, ISO 4217)
--   globalTbl     = 48 (00..99，本分片表序号)
--   accountType   = 4..9 (PLATFORM / TRANSITRECEIVE / TRANSITPAYABLE /
--                          TRANSACTIONFEE / CHARGEFEE / TRANSIT)
--   businessType  = 4..9 (与 accountType 一一对应，见 platformAccountSpec)
--   seq           = 1 (每 (currency, type, gtbl, biz) 组合下只有 1 个 fleet 账户)
--   shadow        = 1 (0 主流量 / 1 shadow)
--
-- 算式：
--   account_no = shadow * 1e18 + currency * 1e15 + accountType * 1e13 +
--                globalTbl * 1e11 + businessType * 1e7 + seq
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (48)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (48)
--
-- 9000000000 / 1 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_48_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 48 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  9000000000 + 48, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_48_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 48 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  9000000000 + 48, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_48_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 48 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  9000000000 + 48, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_48_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 48 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  9000000000 + 48, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_48_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 48 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  9000000000 + 48, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_48_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 48 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  9000000000 + 48, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- ============================================
-- 初始化平台 fleet 账户数据 (按位编码 account_no)
-- ============================================
--
-- account_no 19 位 layout（与 payment-util/shadow.EncodeAccountID 对齐）：
--   shadow(1) | currency(3) | accountType(2) | globalTbl(2) | businessType(4) | seq(7)
--
-- 平台 fleet 账户的字段固定值：
--   currency      = 608 (PHP, ISO 4217)
--   globalTbl     = 49 (00..99，本分片表序号)
--   accountType   = 4..9 (PLATFORM / TRANSITRECEIVE / TRANSITPAYABLE /
--                          TRANSACTIONFEE / CHARGEFEE / TRANSIT)
--   businessType  = 4..9 (与 accountType 一一对应，见 platformAccountSpec)
--   seq           = 1 (每 (currency, type, gtbl, biz) 组合下只有 1 个 fleet 账户)
--   shadow        = 1 (0 主流量 / 1 shadow)
--
-- 算式：
--   account_no = shadow * 1e18 + currency * 1e15 + accountType * 1e13 +
--                globalTbl * 1e11 + businessType * 1e7 + seq
--
-- fleet user_id 段（与 payment-util/shadow/identity.go 对齐）：
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (49)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (49)
--
-- 9000000000 / 1 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_49_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 49 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  9000000000 + 49, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_49_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 49 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  9000000000 + 49, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_49_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 49 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  9000000000 + 49, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_49_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 49 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  9000000000 + 49, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_49_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 49 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  9000000000 + 49, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_49_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 49 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  9000000000 + 49, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

