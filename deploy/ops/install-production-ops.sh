#!/bin/sh
# Install the production-only helper scripts. Run as root on the target host.

set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_DEPLOY_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
DEPLOY_PATH=/opt/sub2api
NEW_API_DB=/opt/new-api/data/one-api.db
RELEASE_IMAGE_PREFIX=

usage() {
  printf '%s\n' "Usage: $0 [--deploy-path PATH] [--new-api-db PATH] [--release-image-prefix REPOSITORY]" >&2
  exit 2
}

while [ $# -gt 0 ]; do
  case "$1" in
    --deploy-path) [ $# -ge 2 ] || usage; DEPLOY_PATH=$2; shift 2 ;;
    --new-api-db) [ $# -ge 2 ] || usage; NEW_API_DB=$2; shift 2 ;;
    --release-image-prefix) [ $# -ge 2 ] || usage; RELEASE_IMAGE_PREFIX=$2; shift 2 ;;
    *) usage ;;
  esac
done

[ "$(id -u)" -eq 0 ] || {
  printf '%s\n' 'run this installer as root' >&2
  exit 1
}

set_env_value() {
  file=$1
  key=$2
  value=$3
  tmp_file=

  case "$key" in
    ''|*[!A-Za-z0-9_]*)
      printf '%s\n' "invalid configuration key: $key" >&2
      exit 2
      ;;
  esac
  if [ "$(printf '%s' "$value" | tr -d '\r\n')" != "$value" ]; then
    printf '%s\n' "refusing multiline value for $key" >&2
    exit 2
  fi

  tmp_file=$(mktemp "${file}.tmp.XXXXXX")
  umask 077
  if [ -f "$file" ]; then
    awk -v key="$key" -v value="$value" '
      $0 !~ "^" key "=" { print }
      END { print key "=" value }
    ' "$file" > "$tmp_file"
  else
    printf '%s=%s\n' "$key" "$value" > "$tmp_file"
  fi
  chmod 0600 "$tmp_file"
  chown root:root "$tmp_file"
  mv -f "$tmp_file" "$file"
}

install -d -m 0755 /etc/sub2api /var/lib/sub2api-ops /var/lib/caddy/maintenance /opt/ops-backups/new-api
install -m 0755 "$SCRIPT_DIR/sub2api-alert.sh" /usr/local/sbin/sub2api-alert
install -m 0755 "$SCRIPT_DIR/sub2api-housekeeping.sh" /usr/local/sbin/sub2api-housekeeping
install -m 0755 "$SCRIPT_DIR/sub2api-service-health.sh" /usr/local/sbin/sub2api-service-health
install -m 0755 "$SCRIPT_DIR/new-api-channel-watchdog.py" /usr/local/sbin/new-api-channel-watchdog
install -m 0755 "$SCRIPT_DIR/prune-new-api-source.sh" /usr/local/sbin/prune-new-api-source
install -m 0644 "$REPO_DEPLOY_DIR/systemd/sub2api-housekeeping.service" /etc/systemd/system/sub2api-housekeeping.service
install -m 0644 "$REPO_DEPLOY_DIR/systemd/sub2api-housekeeping.timer" /etc/systemd/system/sub2api-housekeeping.timer
install -m 0644 "$REPO_DEPLOY_DIR/systemd/sub2api-service-health.service" /etc/systemd/system/sub2api-service-health.service
install -m 0644 "$REPO_DEPLOY_DIR/systemd/sub2api-service-health.timer" /etc/systemd/system/sub2api-service-health.timer
install -m 0644 "$REPO_DEPLOY_DIR/systemd/new-api-channel-watchdog.service" /etc/systemd/system/new-api-channel-watchdog.service
install -m 0644 "$REPO_DEPLOY_DIR/systemd/new-api-channel-watchdog.timer" /etc/systemd/system/new-api-channel-watchdog.timer

if [ ! -e /etc/sub2api/alert-webhook-url ]; then
  install -m 0600 -o root -g root /dev/null /etc/sub2api/alert-webhook-url
fi

set_env_value /etc/sub2api/ops.env SUB2API_DEPLOY_PATH "$DEPLOY_PATH"
set_env_value /etc/sub2api/ops.env NEW_API_SQLITE_PATH "$NEW_API_DB"
if [ -n "$RELEASE_IMAGE_PREFIX" ]; then
  set_env_value /etc/sub2api/ops.env SUB2API_RELEASE_IMAGE_PREFIX "$RELEASE_IMAGE_PREFIX"
fi
set_env_value /etc/new-api-channel-watchdog.env NEW_API_CHANNEL_WATCHDOG_DB "$NEW_API_DB"
set_env_value /etc/new-api-channel-watchdog.env NEW_API_CHANNEL_WATCHDOG_ALERT_COMMAND /usr/local/sbin/sub2api-alert

systemctl daemon-reload
systemctl enable --now sub2api-housekeeping.timer
systemctl enable --now sub2api-service-health.timer
systemctl enable --now new-api-channel-watchdog.timer
printf '%s\n' 'Installed Sub2API production housekeeping helpers.'
