# Applying schema migrations to an existing DB

The `database/metadb/init/init.sql` and `database/orderdb/init/*_init.sql`
scripts are idempotent (`CREATE TABLE IF NOT EXISTS`), but Docker's
`docker-entrypoint-initdb.d` mechanism only runs them **once, on first
volume creation**. If your MySQL volume was bootstrapped before a new
wave's tables landed, you'll see errors like:

```
Error 1146 (42S02): Table 'order_meta.webhook_deliveries' doesn't exist
Error 1146 (42S02): Table 'order_meta.admin_audit_log' doesn't exist
```

## Fix

### Meta DB (non-sharded: merchants, webhook_deliveries, admin_audit_log, ledger, merchant_channel_secret)

```bash
# docker-compose stack
docker exec -i paychan-meta mysql -uroot -ppassword order_meta \
  < database/metadb/migrations/001_waves_F_B_C_H_D_G.sql

# or against a local MySQL
mysql -uroot -ppassword order_meta \
  < database/metadb/migrations/001_waves_F_B_C_H_D_G.sql
```

Re-running is safe — every CREATE uses `IF NOT EXISTS`.

### Sharded DBs (inbound_webhook_XX, dispute_XX, dispute_event_XX, etc)

Fan the template out to all 10 × 10 shards:

```bash
bash database/orderdb/scripts/apply_migration.sh 001_waves_A_D.sql.tpl
```

Env vars:
- `MYSQL_HOST` (default 127.0.0.1)
- `MYSQL_PORT_BASE` (default 3306 → shards at 3306..3315)
- `MYSQL_USER` (default root)
- `MYSQL_PASS` (default password)

## Nuclear option

```bash
# drop everything and re-init; LOSES DATA
docker-compose down -v
docker-compose up -d
```

## Going forward

New waves append to `init/*.sql` + add a numbered template in `migrations/`.
Apply both: new deployments run init scripts; existing deployments run the
migration template.
