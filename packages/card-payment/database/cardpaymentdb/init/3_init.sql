SET NAMES utf8mb4;
CREATE DATABASE IF NOT EXISTS `card_payment_db_3` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `card_payment_db_3`;

-- card-payment 分片表模板。3 = 0..9，30 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_3`;

CREATE TABLE IF NOT EXISTS `card_transaction_30` (
    `id`              BIGINT       NOT NULL AUTO_INCREMENT,
    `pi_id`           VARCHAR(64)  NOT NULL,
    `network`         VARCHAR(16)  NOT NULL,           -- visa / mastercard / jcb / amex / unionpay
    `network_ref_no`  VARCHAR(64),                     -- 卡组织返的 transaction id
    `masked_pan`      VARCHAR(20)  NOT NULL,           -- BIN(6)+last4，**仅此字段含卡片信息**
    `amount`          BIGINT       NOT NULL,
    `currency`        VARCHAR(8)   NOT NULL,
    `status`          VARCHAR(16)  NOT NULL,           -- approved / declined / pending / error
    `decline_code`    VARCHAR(32),
    `decline_reason`  VARCHAR(256),
    `arn`             VARCHAR(64),
    `idempotency_key` VARCHAR(128),
    `created_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_pi_idem` (`pi_id`, `idempotency_key`),
    KEY `idx_network_ref` (`network_ref_no`),
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_status`      (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Card transaction record (no PAN, only masked)';

-- card-payment 分片表模板。3 = 0..9，31 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_3`;

CREATE TABLE IF NOT EXISTS `card_transaction_31` (
    `id`              BIGINT       NOT NULL AUTO_INCREMENT,
    `pi_id`           VARCHAR(64)  NOT NULL,
    `network`         VARCHAR(16)  NOT NULL,           -- visa / mastercard / jcb / amex / unionpay
    `network_ref_no`  VARCHAR(64),                     -- 卡组织返的 transaction id
    `masked_pan`      VARCHAR(20)  NOT NULL,           -- BIN(6)+last4，**仅此字段含卡片信息**
    `amount`          BIGINT       NOT NULL,
    `currency`        VARCHAR(8)   NOT NULL,
    `status`          VARCHAR(16)  NOT NULL,           -- approved / declined / pending / error
    `decline_code`    VARCHAR(32),
    `decline_reason`  VARCHAR(256),
    `arn`             VARCHAR(64),
    `idempotency_key` VARCHAR(128),
    `created_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_pi_idem` (`pi_id`, `idempotency_key`),
    KEY `idx_network_ref` (`network_ref_no`),
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_status`      (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Card transaction record (no PAN, only masked)';

-- card-payment 分片表模板。3 = 0..9，32 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_3`;

CREATE TABLE IF NOT EXISTS `card_transaction_32` (
    `id`              BIGINT       NOT NULL AUTO_INCREMENT,
    `pi_id`           VARCHAR(64)  NOT NULL,
    `network`         VARCHAR(16)  NOT NULL,           -- visa / mastercard / jcb / amex / unionpay
    `network_ref_no`  VARCHAR(64),                     -- 卡组织返的 transaction id
    `masked_pan`      VARCHAR(20)  NOT NULL,           -- BIN(6)+last4，**仅此字段含卡片信息**
    `amount`          BIGINT       NOT NULL,
    `currency`        VARCHAR(8)   NOT NULL,
    `status`          VARCHAR(16)  NOT NULL,           -- approved / declined / pending / error
    `decline_code`    VARCHAR(32),
    `decline_reason`  VARCHAR(256),
    `arn`             VARCHAR(64),
    `idempotency_key` VARCHAR(128),
    `created_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_pi_idem` (`pi_id`, `idempotency_key`),
    KEY `idx_network_ref` (`network_ref_no`),
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_status`      (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Card transaction record (no PAN, only masked)';

-- card-payment 分片表模板。3 = 0..9，33 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_3`;

CREATE TABLE IF NOT EXISTS `card_transaction_33` (
    `id`              BIGINT       NOT NULL AUTO_INCREMENT,
    `pi_id`           VARCHAR(64)  NOT NULL,
    `network`         VARCHAR(16)  NOT NULL,           -- visa / mastercard / jcb / amex / unionpay
    `network_ref_no`  VARCHAR(64),                     -- 卡组织返的 transaction id
    `masked_pan`      VARCHAR(20)  NOT NULL,           -- BIN(6)+last4，**仅此字段含卡片信息**
    `amount`          BIGINT       NOT NULL,
    `currency`        VARCHAR(8)   NOT NULL,
    `status`          VARCHAR(16)  NOT NULL,           -- approved / declined / pending / error
    `decline_code`    VARCHAR(32),
    `decline_reason`  VARCHAR(256),
    `arn`             VARCHAR(64),
    `idempotency_key` VARCHAR(128),
    `created_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_pi_idem` (`pi_id`, `idempotency_key`),
    KEY `idx_network_ref` (`network_ref_no`),
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_status`      (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Card transaction record (no PAN, only masked)';

-- card-payment 分片表模板。3 = 0..9，34 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_3`;

CREATE TABLE IF NOT EXISTS `card_transaction_34` (
    `id`              BIGINT       NOT NULL AUTO_INCREMENT,
    `pi_id`           VARCHAR(64)  NOT NULL,
    `network`         VARCHAR(16)  NOT NULL,           -- visa / mastercard / jcb / amex / unionpay
    `network_ref_no`  VARCHAR(64),                     -- 卡组织返的 transaction id
    `masked_pan`      VARCHAR(20)  NOT NULL,           -- BIN(6)+last4，**仅此字段含卡片信息**
    `amount`          BIGINT       NOT NULL,
    `currency`        VARCHAR(8)   NOT NULL,
    `status`          VARCHAR(16)  NOT NULL,           -- approved / declined / pending / error
    `decline_code`    VARCHAR(32),
    `decline_reason`  VARCHAR(256),
    `arn`             VARCHAR(64),
    `idempotency_key` VARCHAR(128),
    `created_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_pi_idem` (`pi_id`, `idempotency_key`),
    KEY `idx_network_ref` (`network_ref_no`),
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_status`      (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Card transaction record (no PAN, only masked)';

-- card-payment 分片表模板。3 = 0..9，35 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_3`;

CREATE TABLE IF NOT EXISTS `card_transaction_35` (
    `id`              BIGINT       NOT NULL AUTO_INCREMENT,
    `pi_id`           VARCHAR(64)  NOT NULL,
    `network`         VARCHAR(16)  NOT NULL,           -- visa / mastercard / jcb / amex / unionpay
    `network_ref_no`  VARCHAR(64),                     -- 卡组织返的 transaction id
    `masked_pan`      VARCHAR(20)  NOT NULL,           -- BIN(6)+last4，**仅此字段含卡片信息**
    `amount`          BIGINT       NOT NULL,
    `currency`        VARCHAR(8)   NOT NULL,
    `status`          VARCHAR(16)  NOT NULL,           -- approved / declined / pending / error
    `decline_code`    VARCHAR(32),
    `decline_reason`  VARCHAR(256),
    `arn`             VARCHAR(64),
    `idempotency_key` VARCHAR(128),
    `created_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_pi_idem` (`pi_id`, `idempotency_key`),
    KEY `idx_network_ref` (`network_ref_no`),
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_status`      (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Card transaction record (no PAN, only masked)';

-- card-payment 分片表模板。3 = 0..9，36 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_3`;

CREATE TABLE IF NOT EXISTS `card_transaction_36` (
    `id`              BIGINT       NOT NULL AUTO_INCREMENT,
    `pi_id`           VARCHAR(64)  NOT NULL,
    `network`         VARCHAR(16)  NOT NULL,           -- visa / mastercard / jcb / amex / unionpay
    `network_ref_no`  VARCHAR(64),                     -- 卡组织返的 transaction id
    `masked_pan`      VARCHAR(20)  NOT NULL,           -- BIN(6)+last4，**仅此字段含卡片信息**
    `amount`          BIGINT       NOT NULL,
    `currency`        VARCHAR(8)   NOT NULL,
    `status`          VARCHAR(16)  NOT NULL,           -- approved / declined / pending / error
    `decline_code`    VARCHAR(32),
    `decline_reason`  VARCHAR(256),
    `arn`             VARCHAR(64),
    `idempotency_key` VARCHAR(128),
    `created_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_pi_idem` (`pi_id`, `idempotency_key`),
    KEY `idx_network_ref` (`network_ref_no`),
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_status`      (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Card transaction record (no PAN, only masked)';

-- card-payment 分片表模板。3 = 0..9，37 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_3`;

CREATE TABLE IF NOT EXISTS `card_transaction_37` (
    `id`              BIGINT       NOT NULL AUTO_INCREMENT,
    `pi_id`           VARCHAR(64)  NOT NULL,
    `network`         VARCHAR(16)  NOT NULL,           -- visa / mastercard / jcb / amex / unionpay
    `network_ref_no`  VARCHAR(64),                     -- 卡组织返的 transaction id
    `masked_pan`      VARCHAR(20)  NOT NULL,           -- BIN(6)+last4，**仅此字段含卡片信息**
    `amount`          BIGINT       NOT NULL,
    `currency`        VARCHAR(8)   NOT NULL,
    `status`          VARCHAR(16)  NOT NULL,           -- approved / declined / pending / error
    `decline_code`    VARCHAR(32),
    `decline_reason`  VARCHAR(256),
    `arn`             VARCHAR(64),
    `idempotency_key` VARCHAR(128),
    `created_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_pi_idem` (`pi_id`, `idempotency_key`),
    KEY `idx_network_ref` (`network_ref_no`),
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_status`      (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Card transaction record (no PAN, only masked)';

-- card-payment 分片表模板。3 = 0..9，38 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_3`;

CREATE TABLE IF NOT EXISTS `card_transaction_38` (
    `id`              BIGINT       NOT NULL AUTO_INCREMENT,
    `pi_id`           VARCHAR(64)  NOT NULL,
    `network`         VARCHAR(16)  NOT NULL,           -- visa / mastercard / jcb / amex / unionpay
    `network_ref_no`  VARCHAR(64),                     -- 卡组织返的 transaction id
    `masked_pan`      VARCHAR(20)  NOT NULL,           -- BIN(6)+last4，**仅此字段含卡片信息**
    `amount`          BIGINT       NOT NULL,
    `currency`        VARCHAR(8)   NOT NULL,
    `status`          VARCHAR(16)  NOT NULL,           -- approved / declined / pending / error
    `decline_code`    VARCHAR(32),
    `decline_reason`  VARCHAR(256),
    `arn`             VARCHAR(64),
    `idempotency_key` VARCHAR(128),
    `created_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_pi_idem` (`pi_id`, `idempotency_key`),
    KEY `idx_network_ref` (`network_ref_no`),
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_status`      (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Card transaction record (no PAN, only masked)';

-- card-payment 分片表模板。3 = 0..9，39 = 00..99。
-- ⚠️ 严禁出现 PAN / CVV / track_data 字段。

USE `card_payment_db_3`;

CREATE TABLE IF NOT EXISTS `card_transaction_39` (
    `id`              BIGINT       NOT NULL AUTO_INCREMENT,
    `pi_id`           VARCHAR(64)  NOT NULL,
    `network`         VARCHAR(16)  NOT NULL,           -- visa / mastercard / jcb / amex / unionpay
    `network_ref_no`  VARCHAR(64),                     -- 卡组织返的 transaction id
    `masked_pan`      VARCHAR(20)  NOT NULL,           -- BIN(6)+last4，**仅此字段含卡片信息**
    `amount`          BIGINT       NOT NULL,
    `currency`        VARCHAR(8)   NOT NULL,
    `status`          VARCHAR(16)  NOT NULL,           -- approved / declined / pending / error
    `decline_code`    VARCHAR(32),
    `decline_reason`  VARCHAR(256),
    `arn`             VARCHAR(64),
    `idempotency_key` VARCHAR(128),
    `created_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_pi_idem` (`pi_id`, `idempotency_key`),
    KEY `idx_network_ref` (`network_ref_no`),
    KEY `idx_pi`          (`pi_id`),
    KEY `idx_status`      (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Card transaction record (no PAN, only masked)';

