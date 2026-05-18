-- 3_versioning.sql — SP-7 Graph 版本快照表.
--
-- 不变性: 每次 SaveGraph 把 spec_json 落进本表一条新行 (graph_id + version 唯一),
-- moneyflow_graphs.active_version_id 指向其中一条作为当前激活版本.

CREATE TABLE IF NOT EXISTS moneyflow_graph_versions (
    id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    graph_id BIGINT NOT NULL,
    version VARCHAR(32) NOT NULL,
    spec_json JSON NOT NULL,
    immutable_at DATETIME,
    created_by VARCHAR(128),
    change_summary VARCHAR(512),
    created_at DATETIME NOT NULL,
    UNIQUE KEY uk_graph_version (graph_id, version),
    KEY idx_graph_created (graph_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
