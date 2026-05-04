# 迁移工作流

```
golang-migrate \
  -path   database/metadb/migrations \
  -database "mysql://root:pwd@tcp(meta:3306)/user_merchant_meta" \
  up
```

- `001_initial.sql` 是首次安装（等价于 `init/init.sql`）；以后**永远不要**修改它。
- 后续每个变更新加一份 `NNN_<desc>.sql`，按数字递增。
- 已上线集群按时间顺序执行 002 / 003 / ...

## 与 docker-compose 的关系

`docker-compose.yml` 仍 mount `init/init.sql` 跑首次启动；线上 / staging 一律
走 `migrations/` + `golang-migrate`，便于回滚和审计。
