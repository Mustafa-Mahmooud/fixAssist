#!/usr/bin/env bash
# One-time server setup for Ubuntu 22.04/24.04 (Google Cloud e2-micro, x86).
# Run as the default sudo user from the directory holding the deploy/ files:
#   sudo DOMAIN=fixassist.duckdns.org ./bootstrap.sh
# Safe to rerun.
set -euo pipefail

APP_DIR=/opt/fixassist
RUN_USER=${SUDO_USER:-ubuntu}
HERE=$(cd "$(dirname "$0")" && pwd)

if [ "$(id -u)" -ne 0 ]; then echo "run with sudo"; exit 1; fi

echo "== packages"
export DEBIAN_FRONTEND=noninteractive
apt-get update -q
apt-get install -y -q docker.io docker-compose-v2 iptables-persistent cron curl ca-certificates
systemctl enable --now docker
usermod -aG docker "$RUN_USER"

echo "== swap (the e2-micro has 1 GB RAM)"
if ! swapon --show | grep -q /swapfile; then
  fallocate -l 2G /swapfile && chmod 600 /swapfile && mkswap /swapfile && swapon /swapfile
  grep -q /swapfile /etc/fstab || echo "/swapfile none swap sw 0 0" >> /etc/fstab
  sysctl -w vm.swappiness=10 && echo vm.swappiness=10 > /etc/sysctl.d/99-fixassist.conf
fi

echo "== host firewall (cloud firewall rules must also allow 80/443)"
for rule in "-p tcp --dport 80" "-p tcp --dport 443" "-p udp --dport 443"; do
  # shellcheck disable=SC2086
  iptables -C INPUT $rule -m conntrack --ctstate NEW -j ACCEPT 2>/dev/null ||
    iptables -I INPUT 1 $rule -m conntrack --ctstate NEW -j ACCEPT
done
netfilter-persistent save

echo "== files"
mkdir -p "$APP_DIR/postgres"
install -m 0644 "$HERE/compose.yml" "$HERE/Caddyfile" "$APP_DIR/"
install -m 0644 "$HERE/postgres/Dockerfile" "$APP_DIR/postgres/"
install -m 0755 "$HERE/deploy.sh" "$HERE/backup.sh" "$HERE/restore-test.sh" "$HERE/geoip.sh" "$APP_DIR/"
if [ ! -f "$APP_DIR/.env" ]; then
  install -m 0600 "$HERE/env.example" "$APP_DIR/.env"
  pw=$(head -c 32 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 32)
  sed -i "s|^POSTGRES_PASSWORD=.*|POSTGRES_PASSWORD=$pw|" "$APP_DIR/.env"
  secret=$(head -c 48 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 48)
  sed -i "s|^APP_SECRET=.*|APP_SECRET=$secret|" "$APP_DIR/.env"
  [ -n "${DOMAIN:-}" ] && sed -i "s|^DOMAIN=.*|DOMAIN=$DOMAIN|" "$APP_DIR/.env"
  echo "created $APP_DIR/.env (fill in the rest)"
fi
[ -f "$APP_DIR/geo/dbip.mmdb" ] || (cd "$APP_DIR" && ./geoip.sh) || echo "GeoIP download failed; countries stay empty until geoip.sh succeeds"
chown -R "$RUN_USER:$RUN_USER" "$APP_DIR"
chmod 600 "$APP_DIR/.env"

echo "== scheduled jobs"
cat > /etc/cron.d/fixassist <<EOF
# Nightly full backup + keep 7 (FDR: WAL archiving + nightly full, 7 days)
30 3 * * * $RUN_USER $APP_DIR/backup.sh >> /var/log/fixassist-backup.log 2>&1
# Monthly GeoIP refresh (DB-IP Lite)
0 4 3 * * $RUN_USER $APP_DIR/geoip.sh >> /var/log/fixassist-backup.log 2>&1
# Monthly restore test
0 5 1 * * $RUN_USER $APP_DIR/restore-test.sh >> /var/log/fixassist-restore-test.log 2>&1
EOF
touch /var/log/fixassist-backup.log /var/log/fixassist-restore-test.log
chown "$RUN_USER" /var/log/fixassist-backup.log /var/log/fixassist-restore-test.log

echo "== done. Next: fill $APP_DIR/.env, then run $APP_DIR/deploy.sh (or push to main)."
