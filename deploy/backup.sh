#!/usr/bin/env bash
# Nightly full backup to R2 with WAL-G, then keep the last 7 full backups
# (and the WAL needed to restore them). WAL itself is archived continuously.
set -euo pipefail
cd "$(dirname "$0")"

if ! grep -q '^WALG_S3_PREFIX=s3://' .env || grep -q '^WALG_S3_PREFIX=s3://BUCKET' .env; then
  echo "$(date -Is) backups not configured (WALG_S3_PREFIX); skipping"
  exit 0
fi

echo "$(date -Is) backup-push"
docker compose exec -T -u postgres postgres sh -c 'wal-g backup-push "$PGDATA"'
echo "$(date -Is) retain 7"
docker compose exec -T -u postgres postgres wal-g delete retain FULL 7 --confirm
docker compose exec -T -u postgres postgres wal-g backup-list
echo "$(date -Is) ok"
