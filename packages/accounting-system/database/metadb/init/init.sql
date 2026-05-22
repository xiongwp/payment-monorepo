-- 创建另一个数据库account_meta 用于存储账户体系的元数据，如账户类型、交易规则等

-- 强制本次 session 的三个字符集都切到 utf8mb4。否则如果 server 默认是 latin1（MySQL 5.7
-- 很多默认 my.cnf 就是 latin1 / utf8），下面 INSERT 里的中文会被按 latin1 字节存进 utf8mb4 列，
-- 读回来时再当 utf8mb4 解码就变成  "ç"¨æˆ·ä½™é¢è´¦æˆ·"  这种 mojibake。
SET NAMES utf8mb4;
SET CHARACTER SET utf8mb4;

CREATE DATABASE IF NOT EXISTS `account_meta` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `account_meta`;


-- ============================================
-- 8. 账户类型配置表 (account_type_info)
-- 全局表，定义账户科目类型及其属性
-- ============================================
CREATE TABLE IF NOT EXISTS `account_type_info` (
    `id`               BIGINT(20) UNSIGNED NOT NULL COMMENT 'id',
    `account_type`     VARCHAR(32)  NOT NULL COMMENT '账户类型唯一码，如 USER_WALLET / MERCHANT_WALLET',
    `account_type_name` VARCHAR(256) NOT NULL COMMENT '账户类型名称',
    `account_type_desc` VARCHAR(256) DEFAULT NULL COMMENT '账户类型描述',
    `owner_type`       INT(4) NOT NULL COMMENT '账户所有者类型，对应 account.account_type: 1=user 2=merchant 3=merchant_pending 4=platform 5=transit_receivable 6=transit_payable 7=tx_fee 8=charge_fee 9=transit',
    -- is_platform: 1 = 平台内部类型（owner_id 落在预留段 [1,10000]，只能通过 CreatePlatformAccount / Fleet 创建）
    --              0 = 业务账户类型（owner_id 落在 user/merchant 段，通过 CreateAccount 创建）
    -- 未来新增 AccountType 时补一行并设好 is_platform；Go 代码 isPlatformAccountType 也会同步从这里派生（
    -- 当前是 hardcoded switch 4-9）。
    `is_platform`      TINYINT(1) NOT NULL DEFAULT 0 COMMENT '1=平台内部类型(owner_id∈[1,10000]) 0=业务账户类型',
    `balance_direction` VARCHAR(8) NOT NULL COMMENT '正常余额方向: C=贷方 D=借方',
    `description`      VARCHAR(1024) DEFAULT NULL,
    `byte_rds_ctx`     TEXT DEFAULT NULL COMMENT 'env ctx',
    `extra`            TEXT DEFAULT NULL,
    `create_time`      TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `update_time`      TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uniq_account_type` (`account_type`),
    KEY `idx_is_platform` (`is_platform`),
    KEY `idx_create_time` (`create_time`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE utf8mb4_general_ci COMMENT='账户类型配置表';

-- ============================================
-- 8b. 账户业务类型配置表 (account_business_type_info)
-- 全局表，定义 (user_id, business_type) 唯一键中的 business_type 数字码（1-999）。
--
-- category 不在本表存储 —— category 由 account_type 1:1 确定(例如 TransitChannelReceivable=ASSET,
-- TransactionFee=REVENUE...), 放在表里会产生冗余并有被写错的风险。需要 category 时
-- 从 accountingService.categoryForAccountType(account_type) 派生。
--
-- 背景：AccountBusinessType 原本是硬编码的 Go 常量（1-9）。随着"同一 AccountType
-- 多渠道"（Alipay/Gcash 都是 TransitChannelReceivable=5，但需独立账务）需求出现，
-- 每个新渠道需要占用一个未使用的 business_type 码。由 DB 管理后：
--   - 新渠道注册不需要改代码、发版；
--   - 所有实例通过 ConfigSyncWorker 或 HTTP 推送同步最新映射；
--   - 运营可直接在 admin-web 查询"101 对应哪个渠道"，避免约定文档脱节。
--
-- 约定:
--   - 1-9 系统默认保留（映射 AccountType 1-9 的默认渠道）；
--   - 10-100 预留给未来官方 business_type；
--   - 101-999 给自定义渠道（Alipay/Gcash 等）使用。
-- ============================================
CREATE TABLE IF NOT EXISTS `account_business_type_info` (
    `id`                 BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT 'id',
    `business_type`      SMALLINT UNSIGNED NOT NULL COMMENT '业务类型数字码 1-999，(user_id, business_type) 的 business_type 部分',
    `business_type_code` VARCHAR(64)  NOT NULL COMMENT '业务类型码名，如 USER_BALANCE / ALIPAY_RECEIVABLE',
    `account_type`       TINYINT      NOT NULL COMMENT '绑定的 AccountType 数字枚举（1-9，对应 account_type_info.owner_type）;category 由此 1:1 派生，不再存表',
    `description`        VARCHAR(256) DEFAULT NULL,
    `enabled`            TINYINT(1)   NOT NULL DEFAULT 1 COMMENT '0=停用，1=启用',
    `created_at`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`         DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_business_type`      (`business_type`),
    UNIQUE KEY `uk_business_type_code` (`business_type_code`),
    KEY         `idx_account_type`     (`account_type`),
    KEY         `idx_enabled`          (`enabled`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='账户业务类型配置表（channel registry）';

-- 预置默认 1-9（对应 Go 代码中的 AccountBusinessType 常量）
INSERT IGNORE INTO `account_business_type_info`
    (`business_type`, `business_type_code`, `account_type`, `description`, `enabled`)
VALUES
    (1, 'USER_BALANCE',                 1, '用户余额账户', 1),
    (2, 'MERCHANT_BALANCE',             2, '商户结算账户', 1),
    (3, 'MERCHANT_PENDING_SETTLE',      3, '商户待结算余额账户', 1),
    (4, 'PLATFORM_PNL',                 4, '平台损益账户', 1),
    (5, 'TRANSIT_CHANNEL_RECEIVABLE',   5, '中间账户-渠道应收款（默认渠道）', 1),
    (6, 'TRANSIT_CHANNEL_PAYABLE',      6, '中间账户-渠道应付款（默认渠道）', 1),
    (7, 'TRANSACTION_FEE',              7, '平台手续费账户', 1),
    (8, 'CHARGE_FEE',                   8, '平台服务费账户', 1),
    (9, 'TRANSIT',                      9, '平台中间账户', 1);

-- ============================================
-- 9. 交易规则表 (transaction_rule)
-- 全局表，product_code + event_code 决定记账科目和方向
-- 同一 product+event 可有多条规则（主流水 + 手续费等）
-- ============================================
CREATE TABLE IF NOT EXISTS `transaction_rule` (
    `id`               BIGINT(20) UNSIGNED NOT NULL AUTO_INCREMENT COMMENT 'id',
    `product_code`     VARCHAR(256) NOT NULL COMMENT '产品编码',
    `event_code`       VARCHAR(256) NOT NULL COMMENT '事件编码',
    `hash_key`         VARCHAR(256) NOT NULL COMMENT 'product_code,event_code,credit_subject_id,debit_subject_id 拼接',
    `credit_subject_id` VARCHAR(256) DEFAULT NULL COMMENT '贷方科目，对应 account_type_info.account_type',
    `debit_subject_id`  VARCHAR(256) DEFAULT NULL COMMENT '借方科目，对应 account_type_info.account_type',
    `from_direction`   VARCHAR(16) NOT NULL COMMENT 'from方账户在本规则中的方向: debit/credit',
    `to_direction`     VARCHAR(16) NOT NULL COMMENT 'to方账户在本规则中的方向: debit/credit',
    `transaction_type` INT(4)  NOT NULL COMMENT '交易类型',
    `bookkeeping_mode` VARCHAR(32) NOT NULL COMMENT '记账模式',
    `extra`            TEXT DEFAULT NULL,
    `byte_rds_ctx`     TEXT DEFAULT NULL,
    `create_time`      TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `update_time`      TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uniq_hash_key` (`hash_key`),
    KEY `idx_product_code_event_code` (`product_code`, `event_code`),
    KEY `idx_create_time` (`create_time`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE utf8mb4_general_ci COMMENT='交易规则配置表';
-- ============================================
-- 初始化示例：account_type_info 数据
-- ============================================
-- 1-3 业务账户 (is_platform=0)，4-9 平台内部账户 (is_platform=1)。
-- 未来新增类型(例如 10 = OFFLINE_SETTLEMENT 等)按同样规则写 is_platform,
-- admin-web 的"平台账户"查询页/账户管理下拉会按此 flag 自动过滤。
INSERT IGNORE INTO `account_type_info` (`id`, `account_type`, `account_type_name`, `owner_type`, `is_platform`, `balance_direction`)
VALUES
    (1, 'USER_WALLET',                       '用户钱包',             1, 0, 'C'),
    (2, 'MERCHANT_WALLET',                   '商户钱包',             2, 0, 'C'),
    (3, 'MERCHANT_PENDING_SETTLE',           '商户待结算账户',       3, 0, 'C'),
    (4, 'PLATFORM_PNL',                      '平台损益账户',         4, 1, 'C'),
    (5, 'PLATFORM_TRANSIT_CHANNEL_RECEIVABLE','中间账户渠道应收款',  5, 1, 'D'),
    (6, 'PLATFORM_TRANSIT_CHANNEL_PAYABLE',  '中间账户渠道应付款',   6, 1, 'C'),
    (7, 'PLATFORM_TRANSACTION_FEE',          '平台手续费账户',       7, 1, 'C'),
    (8, 'PLATFORM_CHARGE_FEE',               '平台服务费账户',       8, 1, 'C'),
    (9, 'PLATFORM_TRANSIT',                  '平台中间账户',         9, 1, 'C');

-- ============================================

-- ============================================
-- 10. 号段 ID 分配表 (leaf_alloc)
-- 全局单表，存于 account_meta，不随分库分表复制。
-- 号段模式（Leaf Segment）：每次分配一段连续 ID，双 Buffer 保证高吞吐低延迟。
-- ID 严格单调递增，不依赖时钟，重启后时间漂移不影响单调性。
-- ============================================
CREATE TABLE IF NOT EXISTS `leaf_alloc` (
    `biz_tag`     VARCHAR(128) NOT NULL DEFAULT '' COMMENT '业务标签（全局唯一，如 accounting.voucher / accounting.transaction）',
    `max_id`      BIGINT       NOT NULL DEFAULT 1   COMMENT '当前已分配到的最大号段 ID',
    `step`        INT          NOT NULL              COMMENT '号段步长（每次从 DB 获取的 ID 数量）',
    `description` VARCHAR(256)          DEFAULT NULL COMMENT '业务描述',
    `update_time` DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '最后更新时间',
    PRIMARY KEY (`biz_tag`),
    KEY `idx_update_time` (`update_time`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='号段 ID 分配表（Leaf Segment 模式）';

-- ============================================
-- 11. TCC 全局协调者表 (tcc_coordinator)
-- 已迁移至各分片库（accounting_db_0 ~ accounting_db_9），每库一张，不分表。
-- 路由规则：hash(tcc_id 数字) % 10 → accounting_db_{dbIndex}.tcc_coordinator
-- 迁移原因：meta DB 单实例无法承受高并发 TCC 的协调者写入（30K TPS × 3 次/TCC = 90K writes/s）。
-- ============================================

-- ============================================
-- 12. 热点账户配置表 (hot_account_config)
-- 记录走 Redis 热路径的账户列表，服务启动时加载到本地缓存。
-- admin 更新后通过 gRPC ReloadHotAccountAllowlist 触发重新拉取。
-- ============================================
CREATE TABLE IF NOT EXISTS `hot_account_config` (
    `id`          BIGINT       NOT NULL AUTO_INCREMENT,
    `account_no`  VARCHAR(128) NOT NULL COMMENT '账户号（唯一）',
    `enabled`     TINYINT(1)   NOT NULL DEFAULT 1 COMMENT '是否启用热路径：1=是 0=否',
    `description` VARCHAR(256) DEFAULT NULL COMMENT '备注',
    `created_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uniq_account_no` (`account_no`),
    KEY `idx_enabled` (`enabled`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='热点账户配置（Redis 热路径白名单）';

-- ============================================
-- 13. 缓冲记账账户配置表 (buffer_account_config)
-- 记录走缓冲记账模式的账户及其余额刷新间隔等级。
-- flush_interval_level: 1=1分钟 5=5分钟 10=10分钟 60=60分钟 1440=24小时
-- 服务启动时加载到本地缓存；admin 更新后通过 gRPC ReloadBufferAccountConfig 重新拉取。
-- ============================================
CREATE TABLE IF NOT EXISTS `buffer_account_config` (
    `id`                   BIGINT       NOT NULL AUTO_INCREMENT,
    `account_no`           VARCHAR(128) NOT NULL COMMENT '账户号（唯一）',
    `flush_interval_level` SMALLINT     NOT NULL DEFAULT 1 COMMENT '刷新间隔（分钟）：1/5/10/60/1440',
    `enabled`              TINYINT(1)   NOT NULL DEFAULT 1 COMMENT '是否启用缓冲记账：1=是 0=否',
    `description`          VARCHAR(256) DEFAULT NULL COMMENT '备注',
    `created_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    `updated_at`           DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uniq_account_no` (`account_no`),
    KEY `idx_enabled_level` (`enabled`, `flush_interval_level`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='缓冲记账账户配置（按刷新间隔等级分组）';

-- ============================================
-- 初始化示例：transaction_rule 数据（CHECKOUT_PAY 支付场景）
-- from=买家(user) → credit(贷出), to=卖家(merchant) → debit(借入)
-- ============================================
INSERT IGNORE INTO `transaction_rule`
    (`id`, `product_code`, `event_code`, `hash_key`,
     `credit_subject_id`, `debit_subject_id`,
     `from_direction`, `to_direction`,
     `transaction_type`, `bookkeeping_mode`)
VALUES
    (1, 'PAYMENT', 'CHECKOUT_PAY',
     'PAYMENT,CHECKOUT_PAY,USER_WALLET,MERCHANT_WALLET',
     'USER_WALLET', 'MERCHANT_WALLET',
     'credit', 'debit',
     1, 'DOUBLE_ENTRY');

-- ============================================
-- 服务实例注册表 (service_instance)
-- 每个 accounting-system 实例启动时注册，定期心跳，停止时注销。
-- admin-web 通过此表发现所有活跃实例并推送配置变更。
-- ============================================
CREATE TABLE IF NOT EXISTS `service_instance` (
    `instance_id`     VARCHAR(128) NOT NULL COMMENT '实例唯一ID（hostname:http_admin_port）',
    `host`            VARCHAR(256) NOT NULL COMMENT '外部可达主机地址（IP 或域名）',
    `http_admin_port` INT          NOT NULL COMMENT 'HTTP admin 端口',
    `grpc_port`       INT          NOT NULL COMMENT 'gRPC 端口',
    `status`          TINYINT      NOT NULL DEFAULT 1 COMMENT '状态：1=运行中，0=已停止',
    `last_heartbeat`  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '最近心跳时间',
    `started_at`      DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '实例启动时间',
    PRIMARY KEY (`instance_id`),
    KEY `idx_status_heartbeat` (`status`, `last_heartbeat`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='accounting-system 服务实例注册表';

-- ============================================
-- 系统配置（system_config）已删除（v2 迁到 config-center 服务）。
--
-- 历史：本表是 accounting-system 内置的山寨 key-value 配置中心，被
--       SystemConfigService.GetXxx 当作 hot-path 读源，admin-web 改后
--       走 /admin/reload/config 扇出。
--
-- 现状（v2）：
--   - 全平台统一 config-center 服务（packages/config-center）已上线
--   - 业务侧 SystemConfigService 现在 wrap configcenter SDK，namespace =
--     "accounting-system"，key 名 1:1 保留
--   - admin 写操作改走 config-center admin web /admin/ns/accounting-system
--   - 历史 7 个种子 key 在首次部署 config-center 时由运维人工 seed：
--       tcc_recovery.stuck_timeout_minutes  = 5
--       outbox.poll_interval_ms             = 100
--       outbox.batch_size                   = 500
--       day_cut.chunk_size                  = 100000
--       outbox_backpressure.high_threshold  = 5000
--       outbox_backpressure.low_threshold   = 1000
--       outbox_backpressure.shrink_ratio    = 0.5
-- ============================================

-- ============================================
-- Rotating Suspense / Receivable / Payable Accounts
-- 设计文档：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md
-- 域模型：internal/domain/model/rotation.go
--
-- logical_account：跨周期稳定的逻辑账户。多个 account instance 在不同周期承接其流量。
-- I1 不变量：同一 logical_account_id 下任意时刻至多一个 instance phase=active
--           （由 scheduler 切换事务 + invariant_audit_job 巡检保证）
-- 反范式化：current_active_account_no/period_end 由 scheduler 在切换事务原子更新，
--           路由层热路径只查本表即可，避免跨片 account 表 scan。
-- ============================================
CREATE TABLE IF NOT EXISTS `logical_account` (
    `id` BIGINT UNSIGNED NOT NULL COMMENT '主键（Leaf 号段生成）',
    `logical_account_key` VARCHAR(64) NOT NULL COMMENT '业务稳定 key（命名前缀白名单见 rotation.go AllowedKeyPrefixes）',
    `account_type` TINYINT NOT NULL COMMENT '复用 AccountType (期望值 5/6/9)',
    `account_business_type` SMALLINT NOT NULL COMMENT '复用 AccountBusinessType (1-999)',
    `currency` CHAR(3) NOT NULL COMMENT 'ISO 4217',
    `description` VARCHAR(255) DEFAULT NULL,
    `rotation_enabled` TINYINT NOT NULL DEFAULT 0 COMMENT '0=不轮换(legacy) 1=轮换',
    `current_active_account_no` VARCHAR(64) DEFAULT NULL COMMENT '反范式化：当期 active 的 account_no',
    `current_active_period_end` DATETIME DEFAULT NULL,
    `status` TINYINT NOT NULL DEFAULT 1 COMMENT '0=disabled 1=enabled',
    `registered_by` VARCHAR(64) NOT NULL COMMENT '注册者（审计；禁止 lazy create）',
    `created_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `version` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '乐观锁版本号',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_lak` (`logical_account_key`),
    KEY `idx_type_biz_currency` (`account_type`, `account_business_type`, `currency`),
    KEY `idx_rotation_enabled` (`rotation_enabled`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='逻辑账户（跨周期稳定）';


-- ============================================
-- logical_account_rotation_policy：单个逻辑账户的轮换策略
-- 关键字段：
--   drain_p99_seconds       — draining 保留下限（业务 P99 生命周期）
--   drain_hard_timeout_secs — draining 保留上限，超过强制迁移（§8）
--   archive_grace_secs      — frozen → archived 缓冲
--   provision_lead_secs     — scheduler 提前多久预创建下一期（默认 24h）
--   config_version          — 配置版本号，路由层用它判断缓存是否过期 (E-30)
-- 旧 instance 走出生时锁定的 policy_version_at_birth 而非最新策略 (E-28/E-29)。
-- ============================================
CREATE TABLE IF NOT EXISTS `logical_account_rotation_policy` (
    `logical_account_id` BIGINT UNSIGNED NOT NULL COMMENT 'logical_account.id',
    `period_unit` VARCHAR(8) NOT NULL COMMENT 'DAY(测试) / MONTH(应付应收) / QUARTER(通用中间)',
    `period_count` INT NOT NULL DEFAULT 1 COMMENT '周期倍数',
    `rotation_anchor_tz` VARCHAR(32) NOT NULL COMMENT 'IANA 时区',
    `drain_p99_seconds` INT NOT NULL COMMENT 'draining 最短保留',
    `drain_hard_timeout_secs` INT NOT NULL COMMENT 'draining 最长保留；超过强制迁移',
    `archive_grace_secs` INT NOT NULL DEFAULT 604800 COMMENT 'frozen → archived 缓冲（默认 7 天）',
    `provision_lead_secs` INT NOT NULL DEFAULT 86400 COMMENT '提前预创建下一期（默认 24h）',
    `config_version` BIGINT NOT NULL DEFAULT 1 COMMENT '配置版本号；每次变更 +1',
    `effective_from` DATETIME NOT NULL,
    `updated_at` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`logical_account_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='轮换策略';

-- 注：原 10-13 轮换内部过渡科目（ROTATION_MIGRATION_SUSPENSE / RESIDUAL_WRITEOFF /
-- OPS_ADJUST / CARRYFORWARD）已删除 — 不在 business_type registry 暴露给运维。
-- 这些科目是轮换内部 migration / convergence / 归档流程的实现细节，调用方应在代码
-- 内部用专用编码处理，不占用对外的 1-N business_type 名额。
