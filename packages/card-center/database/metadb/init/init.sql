-- card_center_meta：留 leaf_alloc + 全局审计（量小不分片）。
CREATE DATABASE IF NOT EXISTS `card_center_meta` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `card_center_meta`;

CREATE TABLE IF NOT EXISTS `leaf_alloc` (
    `biz_tag`     VARCHAR(128) NOT NULL DEFAULT '',
    `max_id`      BIGINT       NOT NULL DEFAULT 1,
    `step`        INT          NOT NULL DEFAULT 100000,
    `description` VARCHAR(256) DEFAULT NULL,
    `update_time` DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`biz_tag`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Leaf 号段';

INSERT IGNORE INTO `leaf_alloc` (`biz_tag`, `max_id`, `step`, `description`) VALUES
    ('card_center.stored_token', 1, 100000, 'card_stored_token id'),
    ('card_center.payment_token_used', 1, 100000, 'card_payment_token_used id');

-- user_card_session：HTTPS 入口认证的短期会话 token。
--
-- 流程：
--   1. 浏览器登录 api-gateway → api-gateway 持 user_id 调 card-center.IssueUserCardSession
--      （走 mTLS gRPC，clientCN 白名单：api-gateway）
--   2. card-center 颁发 ucs_<random_32bytes>，绑 user_id + scope + ttl，存本表
--   3. api-gateway 把 ucs_xxx 通过 HTTPS 返浏览器（HttpOnly cookie 或一次性表单字段）
--   4. 浏览器 / 前端 SDK HTTPS 调 card-center 时带 Authorization: Bearer ucs_xxx
--   5. card-center HTTPS handler 校验 session：未过期 + 未撤销 + scope 匹配 → 注入 user_id 到 ctx
--   6. 任何 handler 都从 ctx 取 user_id，**绝不**信任 request body 里的 user_id
--      → 用户 A 永远拿不到用户 B 的卡信息
--
-- TTL 策略（按 scope）：
--   tokenize  → 5 min（绑卡场景，给前端校验用户输入留时间）
--   list      → 30 sec（拉列表，毫秒级，给短暂窗口）
--   delete    → 30 sec
-- 一次性约束：绑卡（tokenize scope）成功后立即标 used=1，禁止复用。
CREATE TABLE IF NOT EXISTS `user_card_session` (
    `id`            BIGINT       NOT NULL AUTO_INCREMENT,
    `session_hash`  CHAR(64)     NOT NULL,             -- sha256(ucs_xxx)；不存 token 本身，DB 泄露也拿不到 session
    `user_id`       BIGINT       NOT NULL,
    `scope`         VARCHAR(16)  NOT NULL,             -- tokenize / list / delete
    `issued_to`     VARCHAR(64)  NOT NULL,             -- 申请方 mTLS CN，e.g. "api-gateway"
    `client_ip`     VARCHAR(64)  DEFAULT NULL,         -- 浏览器侧 IP（绑卡时用，可做风控）
    `expires_at`    DATETIME(3)  NOT NULL,
    `used_count`    INT          NOT NULL DEFAULT 0,   -- list/delete 可多次复用；tokenize 单次后置 1
    `revoked_at`    DATETIME(3)  DEFAULT NULL,
    `created_at`    DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_session_hash` (`session_hash`),
    KEY `idx_user_scope`         (`user_id`, `scope`),
    KEY `idx_expires_at`         (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='HTTPS 入口的用户会话 token (Bearer auth)';

-- ─── audit_log 已从 meta 移到 shard ─────────────────────────────────────────
-- 详见 packages/card-center/database/userdb/init/N_init.sql 里的
-- audit_log_NN 分片表（10 库 × 10 表 = 100 表，按 user_id 路由）。
--
-- 移走原因（10K TPS 时 meta 单库 audit insert 是写瓶颈）：
--   - 单 MySQL ~30K simple-insert/s 上限
--   - 10K charge × 1-2 audit/charge = 15K-20K writes/s ≈ 67% 容量
--   - fsync 频率 + AUTO_INCREMENT 锁 + 链式 sha256 串行 → 实际更慢
--
-- 分片设计（per-shard chain）：
--   - 路由 key: user_id；user_id 为空（系统操作）走 trace_id hash 兜底
--   - 链式签名变 per-shard：每 (db_idx, table_idx) 维护独立 prev_hash 链
--     verify 工具按 (db_idx, table_idx) 走 100 条独立链各自验证完整性
--   - 跨 shard 全局序由 Kafka append-only 保证（PCI Req 10 canonical store）
--
-- meta 这边只留 leaf_alloc + user_card_session（量小、不分片）。
