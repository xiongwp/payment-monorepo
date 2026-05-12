-- tokenization-vault DB schema. 真生产 shard by sha256(internal_token)[:N] → 100 shard.

CREATE DATABASE IF NOT EXISTS tokenization_vault DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE tokenization_vault;

-- ── internal_tokens: 主表 ──
-- internal_token 是商户对外用的, 永久稳定; pan_hash 给 dedup (同商户同 PAN 复用 token).
CREATE TABLE IF NOT EXISTS `internal_tokens` (
  `token`         VARCHAR(64)  NOT NULL COMMENT '"tk_live_<32hex>"',
  `merchant_id`   VARCHAR(64)  NOT NULL,
  `pan_hash`      CHAR(64)     NOT NULL COMMENT 'sha256(PAN)[:32]',
  `pan_last4`     CHAR(4)      NOT NULL,
  `bin`           CHAR(6)      NOT NULL,
  `brand`         ENUM('visa','mastercard','amex','discover','jcb','unionpay','unknown') NOT NULL,
  `exp_month`     TINYINT      NOT NULL,
  `exp_year`      SMALLINT     NOT NULL,
  `cardholder_h`  CHAR(32)     NULL,
  `provider`      ENUM('vts','mdes','inhouse') NULL,
  `network_token` VARCHAR(64)  NULL COMMENT 'DPAN from VTS/MDES; nil 表示还没 provision',
  `token_ref_id`  VARCHAR(64)  NULL,
  `token_expiry`  CHAR(4)      NULL COMMENT 'MMYY',
  `provisioned_at` DATETIME(3) NULL,
  `status`        ENUM('active','suspended','deleted','expired') NOT NULL DEFAULT 'active',
  `created_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `updated_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`token`),
  UNIQUE KEY `uk_merchant_pan` (`merchant_id`, `pan_hash`) COMMENT 'dedup 同商户同 PAN',
  KEY `idx_provider_ref` (`provider`, `token_ref_id`) COMMENT 'VTS/MDES 回调反查',
  KEY `idx_status_time`  (`status`, `updated_at`),
  KEY `idx_merchant`     (`merchant_id`, `status`)
) ENGINE=InnoDB COMMENT='internal_token ↔ NetworkRef';

-- ── encrypted_pan: PAN 加密表 (分开存; 防主表 SELECT * 误曝 PAN) ──
CREATE TABLE IF NOT EXISTS `encrypted_pan` (
  `token`        VARCHAR(64)  NOT NULL,
  `ciphertext`   TEXT         NOT NULL COMMENT 'base64(AES-256-GCM(PAN))',
  `created_at`   DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`token`),
  CONSTRAINT `fk_enc_token` FOREIGN KEY (`token`) REFERENCES `internal_tokens`(`token`) ON DELETE CASCADE
) ENGINE=InnoDB COMMENT='PAN 加密落表 (DEK by KMS envelope)';
