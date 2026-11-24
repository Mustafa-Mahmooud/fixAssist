#!/usr/bin/env bash
# Download this month's DB-IP Lite country database (CC BY 4.0; the
# dashboard shows the attribution). Run by bootstrap.sh and monthly by cron.
# Restarts the app only when the file changed.
set -euo pipefail
cd "$(dirname "$0")"
mkdir -p geo
month=$(date -u +%Y-%m)
url="https://download.db-ip.com/free/dbip-country-lite-$month.mmdb.gz"
tmp=$(mktemp)
if ! curl -fsSL -o "$tmp" "$url"; then
  # Early in the month the new file may not be published yet.
  month=$(date -u -d "$(date -u +%Y-%m-01) -1 day" +%Y-%m)
  curl -fsSL -o "$tmp" "https://download.db-ip.com/free/dbip-country-lite-$month.mmdb.gz"
fi
gunzip -c "$tmp" > geo/dbip.mmdb.new
rm -f "$tmp"
if [ -s geo/dbip.mmdb.new ] && ! cmp -s geo/dbip.mmdb.new geo/dbip.mmdb 2>/dev/null; then
  mv geo/dbip.mmdb.new geo/dbip.mmdb
  echo "$(date -Is) installed DB-IP Lite $month"
  if docker compose ps app --status running -q 2>/dev/null | grep -q .; then docker compose restart app; fi
else
  rm -f geo/dbip.mmdb.new
  echo "$(date -Is) DB-IP Lite unchanged"
fi
