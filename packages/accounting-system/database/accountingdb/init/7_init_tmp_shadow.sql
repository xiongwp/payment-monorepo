-- accounting_db_7 的影子 fleet 平台账户 seed
-- 依赖：7_init_shadow.sql 必须已经导入完成（_shadow 表已建）
-- fleet user_id 段：[9000000000, 9000000000+99]
SET NAMES utf8mb4;
USE `accounting_db_7`;

-- ============================================
-- 初始化平台 fleet 账户数据 (按位编码 account_no)
-- ============================================
--
-- account_no 19 位 layout（与 payment-util/shadow.EncodeAccountID 对齐）：
--   shadow(1) | currency(3) | accountType(2) | globalTbl(2) | businessType(4) | seq(7)
--
-- 平台 fleet 账户的字段固定值：
--   currency      = 608 (PHP, ISO 4217)
--   globalTbl     = 70 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (70)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (70)
--
-- 9000000000 / 1 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_70_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 70 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  9000000000 + 70, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_70_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 70 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  9000000000 + 70, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_70_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 70 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  9000000000 + 70, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_70_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 70 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  9000000000 + 70, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_70_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 70 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  9000000000 + 70, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_70_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 70 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  9000000000 + 70, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 71 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (71)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (71)
--
-- 9000000000 / 1 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_71_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 71 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  9000000000 + 71, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_71_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 71 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  9000000000 + 71, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_71_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 71 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  9000000000 + 71, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_71_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 71 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  9000000000 + 71, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_71_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 71 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  9000000000 + 71, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_71_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 71 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  9000000000 + 71, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 72 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (72)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (72)
--
-- 9000000000 / 1 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_72_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 72 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  9000000000 + 72, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_72_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 72 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  9000000000 + 72, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_72_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 72 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  9000000000 + 72, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_72_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 72 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  9000000000 + 72, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_72_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 72 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  9000000000 + 72, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_72_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 72 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  9000000000 + 72, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 73 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (73)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (73)
--
-- 9000000000 / 1 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_73_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 73 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  9000000000 + 73, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_73_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 73 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  9000000000 + 73, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_73_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 73 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  9000000000 + 73, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_73_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 73 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  9000000000 + 73, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_73_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 73 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  9000000000 + 73, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_73_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 73 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  9000000000 + 73, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 74 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (74)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (74)
--
-- 9000000000 / 1 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_74_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 74 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  9000000000 + 74, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_74_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 74 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  9000000000 + 74, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_74_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 74 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  9000000000 + 74, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_74_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 74 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  9000000000 + 74, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_74_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 74 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  9000000000 + 74, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_74_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 74 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  9000000000 + 74, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 75 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (75)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (75)
--
-- 9000000000 / 1 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_75_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 75 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  9000000000 + 75, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_75_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 75 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  9000000000 + 75, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_75_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 75 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  9000000000 + 75, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_75_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 75 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  9000000000 + 75, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_75_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 75 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  9000000000 + 75, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_75_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 75 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  9000000000 + 75, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 76 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (76)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (76)
--
-- 9000000000 / 1 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_76_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 76 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  9000000000 + 76, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_76_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 76 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  9000000000 + 76, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_76_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 76 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  9000000000 + 76, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_76_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 76 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  9000000000 + 76, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_76_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 76 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  9000000000 + 76, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_76_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 76 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  9000000000 + 76, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 77 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (77)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (77)
--
-- 9000000000 / 1 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_77_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 77 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  9000000000 + 77, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_77_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 77 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  9000000000 + 77, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_77_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 77 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  9000000000 + 77, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_77_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 77 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  9000000000 + 77, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_77_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 77 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  9000000000 + 77, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_77_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 77 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  9000000000 + 77, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 78 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (78)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (78)
--
-- 9000000000 / 1 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_78_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 78 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  9000000000 + 78, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_78_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 78 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  9000000000 + 78, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_78_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 78 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  9000000000 + 78, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_78_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 78 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  9000000000 + 78, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_78_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 78 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  9000000000 + 78, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_78_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 78 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  9000000000 + 78, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 79 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (79)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (79)
--
-- 9000000000 / 1 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_79_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 79 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  9000000000 + 79, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_79_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 79 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  9000000000 + 79, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_79_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 79 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  9000000000 + 79, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_79_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 79 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  9000000000 + 79, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_79_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 79 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  9000000000 + 79, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_79_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 79 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  9000000000 + 79, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

