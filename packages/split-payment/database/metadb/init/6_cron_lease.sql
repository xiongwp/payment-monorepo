-- 6_cron_lease.sql — SP-AC-7 X3 多副本 cron lease 表.
--
-- 多副本 split-payment 同时跑 PayoutCron / ReconcileWorker / HoldUnstickWorker 时,
-- 只有抢到 lease 的实例才真跑; 防重复扫.

CREATE TABLE IF NOT EXISTS cron_lease (
    name         VARCHAR(64)  NOT NULL PRIMARY KEY,
    holder       VARCHAR(128) NOT NULL DEFAULT '',
    leased_until DATETIME     NOT NULL DEFAULT '1970-01-01 00:00:00',
    updated_at   DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
