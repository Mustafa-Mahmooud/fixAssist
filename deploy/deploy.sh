#!/usr/bin/env bash
# Roll out a release on the server. Called by the GitHub Actions deploy job:
#   ./deploy.sh ghcr.io/owner/repo:<sha>
# Without an argument it redeploys the image already in .env.
set -euo pipefail
cd "$(dirname "$0")"

if [ -n "${1:-}" ]; then
  sed -i "s|^APP_IMAGE=.*|APP_IMAGE=$1|" .env
fi

docker compose build postgres          # cached after the first run
docker compose pull app caddy
# Migrations run at app startup behind a Postgres advisory lock.
docker compose up -d --remove-orphans
docker image prune -f >/dev/null

domain=$(grep ^DOMAIN= .env | cut -d= -f2)
for i in $(seq 1 30); do
  # /healthz pings Postgres; going through Caddy also checks TLS and routing.
  if curl -fsS -o /dev/null "https://$domain/healthz"; then
    echo "deployed $(grep ^APP_IMAGE= .env | cut -d= -f2)"
    exit 0
  fi
  sleep 2
done
echo "app did not become healthy; recent logs:"
docker compose logs --tail=50 app
exit 1
