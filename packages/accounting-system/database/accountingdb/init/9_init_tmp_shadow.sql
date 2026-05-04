-- accounting_db_9 的影子 fleet 平台账户 seed
-- 依赖：9_init_shadow.sql 必须已经导入完成（_shadow 表已建）
-- fleet user_id 段：[9000000000, 9000000000+99]
SET NAMES utf8mb4;
USE `accounting_db_9`;

-- ============================================
-- 初始化平台 fleet 账户数据 (按位编码 account_no)
-- ============================================
--
-- account_no 19 位 layout（与 payment-util/shadow.EncodeAccountID 对齐）：
--   shadow(1) | currency(3) | accountType(2) | globalTbl(2) | businessType(4) | seq(7)
--
-- 平台 fleet 账户的字段固定值：
--   currency      = 608 (PHP, ISO 4217)
--   globalTbl     = 90 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (90)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (90)
--
-- 9000000000 / 1 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_90_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 90 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  9000000000 + 90, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_90_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 90 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  9000000000 + 90, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_90_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 90 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  9000000000 + 90, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_90_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 90 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  9000000000 + 90, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_90_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 90 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  9000000000 + 90, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_90_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 90 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  9000000000 + 90, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 91 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (91)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (91)
--
-- 9000000000 / 1 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_91_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 91 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  9000000000 + 91, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_91_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 91 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  9000000000 + 91, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_91_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 91 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  9000000000 + 91, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_91_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 91 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  9000000000 + 91, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_91_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 91 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  9000000000 + 91, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_91_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 91 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  9000000000 + 91, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 92 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (92)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (92)
--
-- 9000000000 / 1 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_92_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 92 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  9000000000 + 92, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_92_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 92 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  9000000000 + 92, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_92_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 92 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  9000000000 + 92, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_92_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 92 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  9000000000 + 92, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_92_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 92 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  9000000000 + 92, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_92_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 92 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  9000000000 + 92, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 93 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (93)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (93)
--
-- 9000000000 / 1 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_93_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 93 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  9000000000 + 93, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_93_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 93 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  9000000000 + 93, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_93_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 93 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  9000000000 + 93, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_93_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 93 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  9000000000 + 93, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_93_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 93 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  9000000000 + 93, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_93_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 93 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  9000000000 + 93, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 94 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (94)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (94)
--
-- 9000000000 / 1 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_94_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 94 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  9000000000 + 94, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_94_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 94 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  9000000000 + 94, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_94_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 94 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  9000000000 + 94, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_94_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 94 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  9000000000 + 94, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_94_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 94 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  9000000000 + 94, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_94_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 94 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  9000000000 + 94, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 95 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (95)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (95)
--
-- 9000000000 / 1 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_95_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 95 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  9000000000 + 95, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_95_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 95 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  9000000000 + 95, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_95_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 95 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  9000000000 + 95, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_95_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 95 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  9000000000 + 95, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_95_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 95 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  9000000000 + 95, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_95_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 95 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  9000000000 + 95, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 96 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (96)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (96)
--
-- 9000000000 / 1 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_96_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 96 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  9000000000 + 96, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_96_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 96 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  9000000000 + 96, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_96_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 96 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  9000000000 + 96, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_96_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 96 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  9000000000 + 96, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_96_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 96 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  9000000000 + 96, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_96_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 96 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  9000000000 + 96, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 97 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (97)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (97)
--
-- 9000000000 / 1 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_97_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 97 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  9000000000 + 97, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_97_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 97 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  9000000000 + 97, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_97_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 97 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  9000000000 + 97, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_97_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 97 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  9000000000 + 97, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_97_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 97 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  9000000000 + 97, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_97_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 97 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  9000000000 + 97, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 98 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (98)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (98)
--
-- 9000000000 / 1 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_98_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 98 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  9000000000 + 98, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_98_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 98 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  9000000000 + 98, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_98_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 98 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  9000000000 + 98, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_98_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 98 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  9000000000 + 98, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_98_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 98 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  9000000000 + 98, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_98_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 98 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  9000000000 + 98, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
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
--   globalTbl     = 99 (00..99，本分片表序号)
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
--   主流量：MainFleetUserIDMin   (1_000_000)        + globalTableIdx (99)
--   shadow：ShadowFleetUserIDMin (9_000_000_000)    + globalTableIdx (99)
--
-- 9000000000 / 1 由 generate.sh 替换。

-- 平台损益账户 (accountType=4, businessType=4, REVENUE)
INSERT IGNORE INTO `account_99_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 4 * 10000000000000 + 99 * 100000000000 + 4 * 10000000 + 1 AS CHAR),
  9000000000 + 99, 4, 4, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应收 (accountType=5, businessType=5, ASSET)
INSERT IGNORE INTO `account_99_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 5 * 10000000000000 + 99 * 100000000000 + 5 * 10000000 + 1 AS CHAR),
  9000000000 + 99, 5, 5, 'ASSET', 'PHP', 0, 0, 0, 1
);

-- 中间渠道应付 (accountType=6, businessType=6, LIABILITY)
INSERT IGNORE INTO `account_99_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 6 * 10000000000000 + 99 * 100000000000 + 6 * 10000000 + 1 AS CHAR),
  9000000000 + 99, 6, 6, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

-- 平台手续费 (accountType=7, businessType=7, REVENUE)
INSERT IGNORE INTO `account_99_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 7 * 10000000000000 + 99 * 100000000000 + 7 * 10000000 + 1 AS CHAR),
  9000000000 + 99, 7, 7, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台服务费 (accountType=8, businessType=8, REVENUE)
INSERT IGNORE INTO `account_99_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 8 * 10000000000000 + 99 * 100000000000 + 8 * 10000000 + 1 AS CHAR),
  9000000000 + 99, 8, 8, 'REVENUE', 'PHP', 0, 0, 0, 1
);

-- 平台中间账户 (accountType=9, businessType=9, LIABILITY)
INSERT IGNORE INTO `account_99_shadow` (`account_no`, `user_id`, `account_business_type`, `account_type`, `account_category`, `currency`, `balance`, `frozen_balance`, `available_balance`, `status`)
VALUES (
  CAST(1 * 1000000000000000000 + 608 * 1000000000000000 + 9 * 10000000000000 + 99 * 100000000000 + 9 * 10000000 + 1 AS CHAR),
  9000000000 + 99, 9, 9, 'LIABILITY', 'PHP', 0, 0, 0, 1
);

