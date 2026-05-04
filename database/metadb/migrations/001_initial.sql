-- 001_initial.sql
-- 等价于 init/init.sql 内容；线上首次部署用这个 + golang-migrate 之类的工具跑。
-- 后续变更（002/003...）只做增量，避免重跑 CREATE。
--
-- 应用方式：
--   migrate -path database/metadb/migrations -database "$DSN" up
--
-- 用 init.sql 那一份的目的是 docker-compose 一键起；线上严禁直接挂 init.sql
-- （MySQL docker-entrypoint-initdb.d 只在数据卷空时执行，无版本管控）。

SOURCE database/metadb/init/init.sql;
