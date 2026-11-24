#!/usr/bin/env bash
# Restore the latest backup into a throwaway container and check the data
# (FDR: "a restore test once a month"). Never touches the live database:
# the restored copy has archiving switched off so it can't write to the bucket.
set -euo pipefail
cd "$(dirname "$0")"

VOL=fixassist_restore_test
NAME=fixassist-restore-test
cleanup() { docker rm -f "$NAME" >/dev/null 2>&1 || true; docker volume rm -f "$VOL" >/dev/null 2>&1 || true; }
trap cleanup EXIT
cleanup

envs=(--env-file .env -e AWS_ACCESS_KEY_ID="$(grep ^R2_ACCESS_KEY_ID= .env | cut -d= -f2-)"
      -e AWS_SECRET_ACCESS_KEY="$(grep ^R2_SECRET_ACCESS_KEY= .env | cut -d= -f2-)"
      -e AWS_ENDPOINT="$(grep ^R2_ENDPOINT= .env | cut -d= -f2-)"
      -e AWS_REGION=auto -e AWS_S3_FORCE_PATH_STYLE=true)

echo "$(date -Is) fetching latest backup"
docker run --rm "${envs[@]}" -v "$VOL:/restore" fixassist-postgres:16 sh -ec '
  wal-g backup-fetch /restore LATEST
  touch /restore/recovery.signal
  echo "restore_command = '\''wal-g wal-fetch %f %p'\''" >> /restore/postgresql.auto.conf
  chown -R postgres:postgres /restore
  chmod 700 /restore'

echo "$(date -Is) replaying WAL"
docker run -d --name "$NAME" "${envs[@]}" -e PGDATA=/restore -v "$VOL:/restore" fixassist-postgres:16 \
  postgres -c archive_mode=off -c shared_buffers=256MB >/dev/null

for i in $(seq 1 120); do
  if docker exec "$NAME" psql -U fix -d fix -tAc "select not pg_is_in_recovery()" 2>/dev/null | grep -q t; then
    docker exec "$NAME" psql -U fix -d fix -c \
      "select (select count(*) from projects) projects, (select count(*) from issues) issues, (select max(received_at) from events) last_event"
    echo "$(date -Is) restore test ok"
    exit 0
  fi
  sleep 5
done
echo "$(date -Is) restore test FAILED"
docker logs --tail 50 "$NAME"
exit 1
