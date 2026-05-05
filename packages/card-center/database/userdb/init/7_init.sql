SET NAMES utf8mb4;
CREATE DATABASE IF NOT EXISTS `card_center_db_7` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `card_center_db_7`;

-- card-center 分片表模板。7 = 0..9，70 = 00..99（globalTblIdx）。
-- generate.sh 把所有 7 / 70 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_7`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_70` (
    `id`            BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`       BIGINT       NOT NULL,
    `stored_token`  VARCHAR(1024) NOT NULL,           -- tok_card_<base64>
    `token_hash`    CHAR(64)     NOT NULL,            -- sha256(stored_token) 给唯一索引用
    `masked_pan`    VARCHAR(20)  NOT NULL,            -- BIN+last4
    `network`       VARCHAR(16)  NOT NULL,            -- visa / mastercard / ...
    `exp_month`     TINYINT      NOT NULL,
    `exp_year`      SMALLINT     NOT NULL,
    `holder_name`   VARCHAR(64)  NOT NULL DEFAULT '',
    `status`        VARCHAR(16)  NOT NULL DEFAULT 'active', -- active / deleted
    `kms_kid`       VARCHAR(32)  NOT NULL,            -- 加密用的 KMS key version
    `created_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`    DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 防同 token 多次入库
    KEY `idx_user`        (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card token index';

-- ─── card_payment_token_used：一次性支付 token 使用记录（按 pi_id 分片） ──────
-- Detokenize 前 INSERT 一行；INSERT 失败（uk_token_hash 冲突）= 重复使用 → 拒。
-- 这是"一次性"的物理保证。expired_at 之后 cron 清掉过期行。
CREATE TABLE IF NOT EXISTS `card_payment_token_used_70` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT,
    `token_hash`   CHAR(64)     NOT NULL,
    `pi_id`        VARCHAR(64)  NOT NULL,
    `caller`       VARCHAR(64)  NOT NULL,             -- 调 Detokenize 的服务名（白名单 = "card-payment"）
    `masked_pan`   VARCHAR(20)  DEFAULT NULL,         -- Detokenize 成功后异步反写；取证 / 客服查 PI 用
    `network`      VARCHAR(16)  DEFAULT NULL,
    `used_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `expires_at`   DATETIME(3)  NOT NULL,             -- = token 内嵌 exp_ts
    `created_at`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 一次性 enforcement
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='One-time payment token usage tracking';

-- card-center 分片表模板。7 = 0..9，71 = 00..99（globalTblIdx）。
-- generate.sh 把所有 7 / 71 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_7`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_71` (
    `id`            BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`       BIGINT       NOT NULL,
    `stored_token`  VARCHAR(1024) NOT NULL,           -- tok_card_<base64>
    `token_hash`    CHAR(64)     NOT NULL,            -- sha256(stored_token) 给唯一索引用
    `masked_pan`    VARCHAR(20)  NOT NULL,            -- BIN+last4
    `network`       VARCHAR(16)  NOT NULL,            -- visa / mastercard / ...
    `exp_month`     TINYINT      NOT NULL,
    `exp_year`      SMALLINT     NOT NULL,
    `holder_name`   VARCHAR(64)  NOT NULL DEFAULT '',
    `status`        VARCHAR(16)  NOT NULL DEFAULT 'active', -- active / deleted
    `kms_kid`       VARCHAR(32)  NOT NULL,            -- 加密用的 KMS key version
    `created_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`    DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 防同 token 多次入库
    KEY `idx_user`        (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card token index';

-- ─── card_payment_token_used：一次性支付 token 使用记录（按 pi_id 分片） ──────
-- Detokenize 前 INSERT 一行；INSERT 失败（uk_token_hash 冲突）= 重复使用 → 拒。
-- 这是"一次性"的物理保证。expired_at 之后 cron 清掉过期行。
CREATE TABLE IF NOT EXISTS `card_payment_token_used_71` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT,
    `token_hash`   CHAR(64)     NOT NULL,
    `pi_id`        VARCHAR(64)  NOT NULL,
    `caller`       VARCHAR(64)  NOT NULL,             -- 调 Detokenize 的服务名（白名单 = "card-payment"）
    `masked_pan`   VARCHAR(20)  DEFAULT NULL,         -- Detokenize 成功后异步反写；取证 / 客服查 PI 用
    `network`      VARCHAR(16)  DEFAULT NULL,
    `used_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `expires_at`   DATETIME(3)  NOT NULL,             -- = token 内嵌 exp_ts
    `created_at`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 一次性 enforcement
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='One-time payment token usage tracking';

-- card-center 分片表模板。7 = 0..9，72 = 00..99（globalTblIdx）。
-- generate.sh 把所有 7 / 72 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_7`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_72` (
    `id`            BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`       BIGINT       NOT NULL,
    `stored_token`  VARCHAR(1024) NOT NULL,           -- tok_card_<base64>
    `token_hash`    CHAR(64)     NOT NULL,            -- sha256(stored_token) 给唯一索引用
    `masked_pan`    VARCHAR(20)  NOT NULL,            -- BIN+last4
    `network`       VARCHAR(16)  NOT NULL,            -- visa / mastercard / ...
    `exp_month`     TINYINT      NOT NULL,
    `exp_year`      SMALLINT     NOT NULL,
    `holder_name`   VARCHAR(64)  NOT NULL DEFAULT '',
    `status`        VARCHAR(16)  NOT NULL DEFAULT 'active', -- active / deleted
    `kms_kid`       VARCHAR(32)  NOT NULL,            -- 加密用的 KMS key version
    `created_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`    DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 防同 token 多次入库
    KEY `idx_user`        (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card token index';

-- ─── card_payment_token_used：一次性支付 token 使用记录（按 pi_id 分片） ──────
-- Detokenize 前 INSERT 一行；INSERT 失败（uk_token_hash 冲突）= 重复使用 → 拒。
-- 这是"一次性"的物理保证。expired_at 之后 cron 清掉过期行。
CREATE TABLE IF NOT EXISTS `card_payment_token_used_72` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT,
    `token_hash`   CHAR(64)     NOT NULL,
    `pi_id`        VARCHAR(64)  NOT NULL,
    `caller`       VARCHAR(64)  NOT NULL,             -- 调 Detokenize 的服务名（白名单 = "card-payment"）
    `masked_pan`   VARCHAR(20)  DEFAULT NULL,         -- Detokenize 成功后异步反写；取证 / 客服查 PI 用
    `network`      VARCHAR(16)  DEFAULT NULL,
    `used_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `expires_at`   DATETIME(3)  NOT NULL,             -- = token 内嵌 exp_ts
    `created_at`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 一次性 enforcement
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='One-time payment token usage tracking';

-- card-center 分片表模板。7 = 0..9，73 = 00..99（globalTblIdx）。
-- generate.sh 把所有 7 / 73 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_7`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_73` (
    `id`            BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`       BIGINT       NOT NULL,
    `stored_token`  VARCHAR(1024) NOT NULL,           -- tok_card_<base64>
    `token_hash`    CHAR(64)     NOT NULL,            -- sha256(stored_token) 给唯一索引用
    `masked_pan`    VARCHAR(20)  NOT NULL,            -- BIN+last4
    `network`       VARCHAR(16)  NOT NULL,            -- visa / mastercard / ...
    `exp_month`     TINYINT      NOT NULL,
    `exp_year`      SMALLINT     NOT NULL,
    `holder_name`   VARCHAR(64)  NOT NULL DEFAULT '',
    `status`        VARCHAR(16)  NOT NULL DEFAULT 'active', -- active / deleted
    `kms_kid`       VARCHAR(32)  NOT NULL,            -- 加密用的 KMS key version
    `created_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`    DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 防同 token 多次入库
    KEY `idx_user`        (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card token index';

-- ─── card_payment_token_used：一次性支付 token 使用记录（按 pi_id 分片） ──────
-- Detokenize 前 INSERT 一行；INSERT 失败（uk_token_hash 冲突）= 重复使用 → 拒。
-- 这是"一次性"的物理保证。expired_at 之后 cron 清掉过期行。
CREATE TABLE IF NOT EXISTS `card_payment_token_used_73` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT,
    `token_hash`   CHAR(64)     NOT NULL,
    `pi_id`        VARCHAR(64)  NOT NULL,
    `caller`       VARCHAR(64)  NOT NULL,             -- 调 Detokenize 的服务名（白名单 = "card-payment"）
    `masked_pan`   VARCHAR(20)  DEFAULT NULL,         -- Detokenize 成功后异步反写；取证 / 客服查 PI 用
    `network`      VARCHAR(16)  DEFAULT NULL,
    `used_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `expires_at`   DATETIME(3)  NOT NULL,             -- = token 内嵌 exp_ts
    `created_at`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 一次性 enforcement
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='One-time payment token usage tracking';

-- card-center 分片表模板。7 = 0..9，74 = 00..99（globalTblIdx）。
-- generate.sh 把所有 7 / 74 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_7`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_74` (
    `id`            BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`       BIGINT       NOT NULL,
    `stored_token`  VARCHAR(1024) NOT NULL,           -- tok_card_<base64>
    `token_hash`    CHAR(64)     NOT NULL,            -- sha256(stored_token) 给唯一索引用
    `masked_pan`    VARCHAR(20)  NOT NULL,            -- BIN+last4
    `network`       VARCHAR(16)  NOT NULL,            -- visa / mastercard / ...
    `exp_month`     TINYINT      NOT NULL,
    `exp_year`      SMALLINT     NOT NULL,
    `holder_name`   VARCHAR(64)  NOT NULL DEFAULT '',
    `status`        VARCHAR(16)  NOT NULL DEFAULT 'active', -- active / deleted
    `kms_kid`       VARCHAR(32)  NOT NULL,            -- 加密用的 KMS key version
    `created_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`    DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 防同 token 多次入库
    KEY `idx_user`        (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card token index';

-- ─── card_payment_token_used：一次性支付 token 使用记录（按 pi_id 分片） ──────
-- Detokenize 前 INSERT 一行；INSERT 失败（uk_token_hash 冲突）= 重复使用 → 拒。
-- 这是"一次性"的物理保证。expired_at 之后 cron 清掉过期行。
CREATE TABLE IF NOT EXISTS `card_payment_token_used_74` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT,
    `token_hash`   CHAR(64)     NOT NULL,
    `pi_id`        VARCHAR(64)  NOT NULL,
    `caller`       VARCHAR(64)  NOT NULL,             -- 调 Detokenize 的服务名（白名单 = "card-payment"）
    `masked_pan`   VARCHAR(20)  DEFAULT NULL,         -- Detokenize 成功后异步反写；取证 / 客服查 PI 用
    `network`      VARCHAR(16)  DEFAULT NULL,
    `used_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `expires_at`   DATETIME(3)  NOT NULL,             -- = token 内嵌 exp_ts
    `created_at`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 一次性 enforcement
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='One-time payment token usage tracking';

-- card-center 分片表模板。7 = 0..9，75 = 00..99（globalTblIdx）。
-- generate.sh 把所有 7 / 75 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_7`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_75` (
    `id`            BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`       BIGINT       NOT NULL,
    `stored_token`  VARCHAR(1024) NOT NULL,           -- tok_card_<base64>
    `token_hash`    CHAR(64)     NOT NULL,            -- sha256(stored_token) 给唯一索引用
    `masked_pan`    VARCHAR(20)  NOT NULL,            -- BIN+last4
    `network`       VARCHAR(16)  NOT NULL,            -- visa / mastercard / ...
    `exp_month`     TINYINT      NOT NULL,
    `exp_year`      SMALLINT     NOT NULL,
    `holder_name`   VARCHAR(64)  NOT NULL DEFAULT '',
    `status`        VARCHAR(16)  NOT NULL DEFAULT 'active', -- active / deleted
    `kms_kid`       VARCHAR(32)  NOT NULL,            -- 加密用的 KMS key version
    `created_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`    DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 防同 token 多次入库
    KEY `idx_user`        (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card token index';

-- ─── card_payment_token_used：一次性支付 token 使用记录（按 pi_id 分片） ──────
-- Detokenize 前 INSERT 一行；INSERT 失败（uk_token_hash 冲突）= 重复使用 → 拒。
-- 这是"一次性"的物理保证。expired_at 之后 cron 清掉过期行。
CREATE TABLE IF NOT EXISTS `card_payment_token_used_75` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT,
    `token_hash`   CHAR(64)     NOT NULL,
    `pi_id`        VARCHAR(64)  NOT NULL,
    `caller`       VARCHAR(64)  NOT NULL,             -- 调 Detokenize 的服务名（白名单 = "card-payment"）
    `masked_pan`   VARCHAR(20)  DEFAULT NULL,         -- Detokenize 成功后异步反写；取证 / 客服查 PI 用
    `network`      VARCHAR(16)  DEFAULT NULL,
    `used_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `expires_at`   DATETIME(3)  NOT NULL,             -- = token 内嵌 exp_ts
    `created_at`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 一次性 enforcement
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='One-time payment token usage tracking';

-- card-center 分片表模板。7 = 0..9，76 = 00..99（globalTblIdx）。
-- generate.sh 把所有 7 / 76 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_7`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_76` (
    `id`            BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`       BIGINT       NOT NULL,
    `stored_token`  VARCHAR(1024) NOT NULL,           -- tok_card_<base64>
    `token_hash`    CHAR(64)     NOT NULL,            -- sha256(stored_token) 给唯一索引用
    `masked_pan`    VARCHAR(20)  NOT NULL,            -- BIN+last4
    `network`       VARCHAR(16)  NOT NULL,            -- visa / mastercard / ...
    `exp_month`     TINYINT      NOT NULL,
    `exp_year`      SMALLINT     NOT NULL,
    `holder_name`   VARCHAR(64)  NOT NULL DEFAULT '',
    `status`        VARCHAR(16)  NOT NULL DEFAULT 'active', -- active / deleted
    `kms_kid`       VARCHAR(32)  NOT NULL,            -- 加密用的 KMS key version
    `created_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`    DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 防同 token 多次入库
    KEY `idx_user`        (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card token index';

-- ─── card_payment_token_used：一次性支付 token 使用记录（按 pi_id 分片） ──────
-- Detokenize 前 INSERT 一行；INSERT 失败（uk_token_hash 冲突）= 重复使用 → 拒。
-- 这是"一次性"的物理保证。expired_at 之后 cron 清掉过期行。
CREATE TABLE IF NOT EXISTS `card_payment_token_used_76` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT,
    `token_hash`   CHAR(64)     NOT NULL,
    `pi_id`        VARCHAR(64)  NOT NULL,
    `caller`       VARCHAR(64)  NOT NULL,             -- 调 Detokenize 的服务名（白名单 = "card-payment"）
    `masked_pan`   VARCHAR(20)  DEFAULT NULL,         -- Detokenize 成功后异步反写；取证 / 客服查 PI 用
    `network`      VARCHAR(16)  DEFAULT NULL,
    `used_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `expires_at`   DATETIME(3)  NOT NULL,             -- = token 内嵌 exp_ts
    `created_at`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 一次性 enforcement
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='One-time payment token usage tracking';

-- card-center 分片表模板。7 = 0..9，77 = 00..99（globalTblIdx）。
-- generate.sh 把所有 7 / 77 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_7`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_77` (
    `id`            BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`       BIGINT       NOT NULL,
    `stored_token`  VARCHAR(1024) NOT NULL,           -- tok_card_<base64>
    `token_hash`    CHAR(64)     NOT NULL,            -- sha256(stored_token) 给唯一索引用
    `masked_pan`    VARCHAR(20)  NOT NULL,            -- BIN+last4
    `network`       VARCHAR(16)  NOT NULL,            -- visa / mastercard / ...
    `exp_month`     TINYINT      NOT NULL,
    `exp_year`      SMALLINT     NOT NULL,
    `holder_name`   VARCHAR(64)  NOT NULL DEFAULT '',
    `status`        VARCHAR(16)  NOT NULL DEFAULT 'active', -- active / deleted
    `kms_kid`       VARCHAR(32)  NOT NULL,            -- 加密用的 KMS key version
    `created_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`    DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 防同 token 多次入库
    KEY `idx_user`        (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card token index';

-- ─── card_payment_token_used：一次性支付 token 使用记录（按 pi_id 分片） ──────
-- Detokenize 前 INSERT 一行；INSERT 失败（uk_token_hash 冲突）= 重复使用 → 拒。
-- 这是"一次性"的物理保证。expired_at 之后 cron 清掉过期行。
CREATE TABLE IF NOT EXISTS `card_payment_token_used_77` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT,
    `token_hash`   CHAR(64)     NOT NULL,
    `pi_id`        VARCHAR(64)  NOT NULL,
    `caller`       VARCHAR(64)  NOT NULL,             -- 调 Detokenize 的服务名（白名单 = "card-payment"）
    `masked_pan`   VARCHAR(20)  DEFAULT NULL,         -- Detokenize 成功后异步反写；取证 / 客服查 PI 用
    `network`      VARCHAR(16)  DEFAULT NULL,
    `used_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `expires_at`   DATETIME(3)  NOT NULL,             -- = token 内嵌 exp_ts
    `created_at`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 一次性 enforcement
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='One-time payment token usage tracking';

-- card-center 分片表模板。7 = 0..9，78 = 00..99（globalTblIdx）。
-- generate.sh 把所有 7 / 78 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_7`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_78` (
    `id`            BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`       BIGINT       NOT NULL,
    `stored_token`  VARCHAR(1024) NOT NULL,           -- tok_card_<base64>
    `token_hash`    CHAR(64)     NOT NULL,            -- sha256(stored_token) 给唯一索引用
    `masked_pan`    VARCHAR(20)  NOT NULL,            -- BIN+last4
    `network`       VARCHAR(16)  NOT NULL,            -- visa / mastercard / ...
    `exp_month`     TINYINT      NOT NULL,
    `exp_year`      SMALLINT     NOT NULL,
    `holder_name`   VARCHAR(64)  NOT NULL DEFAULT '',
    `status`        VARCHAR(16)  NOT NULL DEFAULT 'active', -- active / deleted
    `kms_kid`       VARCHAR(32)  NOT NULL,            -- 加密用的 KMS key version
    `created_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`    DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 防同 token 多次入库
    KEY `idx_user`        (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card token index';

-- ─── card_payment_token_used：一次性支付 token 使用记录（按 pi_id 分片） ──────
-- Detokenize 前 INSERT 一行；INSERT 失败（uk_token_hash 冲突）= 重复使用 → 拒。
-- 这是"一次性"的物理保证。expired_at 之后 cron 清掉过期行。
CREATE TABLE IF NOT EXISTS `card_payment_token_used_78` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT,
    `token_hash`   CHAR(64)     NOT NULL,
    `pi_id`        VARCHAR(64)  NOT NULL,
    `caller`       VARCHAR(64)  NOT NULL,             -- 调 Detokenize 的服务名（白名单 = "card-payment"）
    `masked_pan`   VARCHAR(20)  DEFAULT NULL,         -- Detokenize 成功后异步反写；取证 / 客服查 PI 用
    `network`      VARCHAR(16)  DEFAULT NULL,
    `used_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `expires_at`   DATETIME(3)  NOT NULL,             -- = token 内嵌 exp_ts
    `created_at`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 一次性 enforcement
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='One-time payment token usage tracking';

-- card-center 分片表模板。7 = 0..9，79 = 00..99（globalTblIdx）。
-- generate.sh 把所有 7 / 79 替换后产 init/N_init.sql。
--
-- ⚠️ 本服务持久化层**永不存** PAN / CVV / 任何卡敏数据。
--    存的只是 token（KMS-encrypted blob）+ masked_pan + 业务元数据。

USE `card_center_db_7`;

-- ─── card_stored_token：用户存卡索引（按 user_id 分片） ──────────────────────
-- 每行对应一张存卡。stored_token 是 card-center 颁发的长期 token（KMS-encrypted），
-- 业务侧拿这个 token 调 CreatePaymentToken 派生支付 token。
-- 删卡只标 deleted_at（业务层 soft delete），物理失效靠 KMS key rotation。
CREATE TABLE IF NOT EXISTS `card_stored_token_79` (
    `id`            BIGINT       NOT NULL AUTO_INCREMENT,
    `user_id`       BIGINT       NOT NULL,
    `stored_token`  VARCHAR(1024) NOT NULL,           -- tok_card_<base64>
    `token_hash`    CHAR(64)     NOT NULL,            -- sha256(stored_token) 给唯一索引用
    `masked_pan`    VARCHAR(20)  NOT NULL,            -- BIN+last4
    `network`       VARCHAR(16)  NOT NULL,            -- visa / mastercard / ...
    `exp_month`     TINYINT      NOT NULL,
    `exp_year`      SMALLINT     NOT NULL,
    `holder_name`   VARCHAR(64)  NOT NULL DEFAULT '',
    `status`        VARCHAR(16)  NOT NULL DEFAULT 'active', -- active / deleted
    `kms_kid`       VARCHAR(32)  NOT NULL,            -- 加密用的 KMS key version
    `created_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    `deleted_at`    DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 防同 token 多次入库
    KEY `idx_user`        (`user_id`, `status`),
    KEY `idx_deleted_at`  (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='User stored card token index';

-- ─── card_payment_token_used：一次性支付 token 使用记录（按 pi_id 分片） ──────
-- Detokenize 前 INSERT 一行；INSERT 失败（uk_token_hash 冲突）= 重复使用 → 拒。
-- 这是"一次性"的物理保证。expired_at 之后 cron 清掉过期行。
CREATE TABLE IF NOT EXISTS `card_payment_token_used_79` (
    `id`           BIGINT       NOT NULL AUTO_INCREMENT,
    `token_hash`   CHAR(64)     NOT NULL,
    `pi_id`        VARCHAR(64)  NOT NULL,
    `caller`       VARCHAR(64)  NOT NULL,             -- 调 Detokenize 的服务名（白名单 = "card-payment"）
    `masked_pan`   VARCHAR(20)  DEFAULT NULL,         -- Detokenize 成功后异步反写；取证 / 客服查 PI 用
    `network`      VARCHAR(16)  DEFAULT NULL,
    `used_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `expires_at`   DATETIME(3)  NOT NULL,             -- = token 内嵌 exp_ts
    `created_at`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token_hash` (`token_hash`),         -- 一次性 enforcement
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_expires_at`  (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='One-time payment token usage tracking';

