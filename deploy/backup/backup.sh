#!/usr/bin/env bash
# backup.sh — 单库备份: mysqldump | gzip | s3 cp。
#
# env:
#   DB_HOST DB_USER DB_PASS DB_NAME
#   S3_BUCKET S3_REGION [S3_ENDPOINT]
#   PUSHGATEWAY_URL  (可选, 推 success metric)

set -euo pipefail

[[ -z "${DB_NAME:-}" ]] && { echo "DB_NAME required"; exit 2; }

TS=$(date -u +%Y%m%d-%H%M%S)
KEY="daily/$(date -u +%Y/%m/%d)/${DB_NAME}-${TS}.sql.gz"
OBJ="s3://${S3_BUCKET}/${KEY}"
TMP=$(mktemp)

echo "▶ dump ${DB_NAME} from ${DB_HOST}..."
mysqldump \
  --host="$DB_HOST" --user="$DB_USER" --password="$DB_PASS" \
  --single-transaction --quick --routines --triggers --events \
  --set-gtid-purged=OFF \
  --databases "$DB_NAME" \
  | gzip -6 > "$TMP"

SIZE=$(stat -c%s "$TMP" 2>/dev/null || stat -f%z "$TMP")
echo "  dumped + gzip: ${SIZE} bytes"

echo "▶ upload to $OBJ"
AWS_DEFAULT_REGION="${S3_REGION:-us-east-1}"
AWS_ENDPOINT_FLAG=""
[[ -n "${S3_ENDPOINT:-}" ]] && AWS_ENDPOINT_FLAG="--endpoint-url $S3_ENDPOINT"
aws s3 cp $AWS_ENDPOINT_FLAG --storage-class STANDARD_IA "$TMP" "$OBJ" \
  --metadata "db=${DB_NAME},ts=${TS},host=${DB_HOST}"

# 校验 checksum (上传后下载头部 verify)
echo "▶ verify upload"
aws s3 cp $AWS_ENDPOINT_FLAG "$OBJ" /tmp/verify.gz --range bytes=0-4095 > /dev/null
gzip -t /tmp/verify.gz && echo "  ✓ gzip header valid"

# 推 metric 到 pushgateway (可选)
if [[ -n "${PUSHGATEWAY_URL:-}" ]]; then
  cat <<EOF | curl -fsS --data-binary @- "${PUSHGATEWAY_URL}/metrics/job/db_backup/db/${DB_NAME}"
db_backup_last_success_ts ${SECONDS}
db_backup_bytes ${SIZE}
EOF
fi

rm -f "$TMP" /tmp/verify.gz
echo "✅ backup ${DB_NAME} done (${SIZE} bytes → ${KEY})"
