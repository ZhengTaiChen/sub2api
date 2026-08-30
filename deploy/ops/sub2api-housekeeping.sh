#!/bin/sh
# Conservative daily maintenance for a Docker-hosted Sub2API deployment.
# It deliberately does not prune volumes, stopped containers, or tagged images
# outside the configured Sub2API release repository.

set -eu

DEPLOY_PATH=${SUB2API_DEPLOY_PATH:-/opt/sub2api}
STATE_DIR=$DEPLOY_PATH/.deploy
CONFIG_FILE=${SUB2API_OPS_CONFIG:-/etc/sub2api/ops.env}
ALERT_COMMAND=${SUB2API_ALERT_COMMAND:-/usr/local/sbin/sub2api-alert}
NEW_API_DB=${NEW_API_SQLITE_PATH:-/opt/new-api/data/one-api.db}
BACKUP_DIR=${NEW_API_BACKUP_DIR:-/opt/ops-backups/new-api}
RELEASE_IMAGE_PREFIX=${SUB2API_RELEASE_IMAGE_PREFIX:-}
LOG_MAX_BYTES=${SUB2API_DEPLOY_LOG_MAX_BYTES:-5242880}
LOG_MAX_ARCHIVES=${SUB2API_DEPLOY_LOG_MAX_ARCHIVES:-5}
WARN_PERCENT=${SUB2API_DISK_WARN_PERCENT:-70}
CRITICAL_PERCENT=${SUB2API_DISK_CRITICAL_PERCENT:-80}
EMERGENCY=0

if [ -r "$CONFIG_FILE" ]; then
  # This file only contains non-secret maintenance settings and is installed
  # root-owned. Do not put credentials in it.
  . "$CONFIG_FILE"
fi

case "${1:-}" in
  '') ;;
  --emergency) EMERGENCY=1 ;;
  *) printf '%s\n' "Usage: $0 [--emergency]" >&2; exit 2 ;;
esac

for numeric in LOG_MAX_BYTES LOG_MAX_ARCHIVES WARN_PERCENT CRITICAL_PERCENT; do
  eval "numeric_value=\$$numeric"
  case "$numeric_value" in
    ''|*[!0-9]*) printf 'invalid %s\n' "$numeric" >&2; exit 2 ;;
  esac
done
[ "$WARN_PERCENT" -lt "$CRITICAL_PERCENT" ] || {
  printf '%s\n' 'disk warning threshold must be lower than critical threshold' >&2
  exit 2
}

notify() {
  severity=$1
  event=$2
  message=$3
  [ -x "$ALERT_COMMAND" ] || return 0
  "$ALERT_COMMAND" "$severity" "$event" "$message" >/dev/null 2>&1 || true
}

rotate_log() {
  log_file=$1
  [ -f "$log_file" ] || return 0
  log_size=$(wc -c < "$log_file" | tr -d ' ')
  [ "$log_size" -lt "$LOG_MAX_BYTES" ] && return 0

  i=$LOG_MAX_ARCHIVES
  while [ "$i" -gt 1 ]; do
    previous=$((i - 1))
    [ -f "$log_file.$previous" ] && mv -f "$log_file.$previous" "$log_file.$i"
    i=$previous
  done
  mv -f "$log_file" "$log_file.1"
}

protected_image_ids() {
  {
    docker ps -aq | xargs -r docker inspect --format '{{.Image}}' 2>/dev/null || true
    docker image inspect sub2api-retain:current --format '{{.Id}}' 2>/dev/null || true
    docker image inspect sub2api-retain:previous --format '{{.Id}}' 2>/dev/null || true
    for state_file in "$STATE_DIR/current-image" "$STATE_DIR/previous-image"; do
      [ -r "$state_file" ] || continue
      image_ref=$(tr -d '\r\n' < "$state_file")
      [ -n "$image_ref" ] || continue
      docker image inspect "$image_ref" --format '{{.Id}}' 2>/dev/null || true
    done
  } | sort -u
}

is_protected_image() {
  image_id=$1
  protected=$2
  printf '%s\n' "$protected" | grep -Fxq "$image_id"
}

prune_old_release_images() {
  [ -n "$RELEASE_IMAGE_PREFIX" ] || return 0
  protected=$(protected_image_ids)

  docker image ls --format '{{.Repository}} {{.ID}}' |
    awk -v prefix="$RELEASE_IMAGE_PREFIX" '$1 == prefix { print $2 }' |
    sort -u |
    while IFS= read -r image_id; do
      [ -n "$image_id" ] || continue
      if is_protected_image "$image_id" "$protected"; then
        continue
      fi
      docker image rm "$image_id" >/dev/null 2>&1 || true
    done
}

backup_new_api_sqlite() {
  [ "$EMERGENCY" -eq 0 ] || return 0
  [ -f "$NEW_API_DB" ] || return 0
  command -v sqlite3 >/dev/null 2>&1 || {
    notify warning new_api_backup_skipped 'sqlite3 is unavailable; no SQLite backup was created'
    return 0
  }

  mkdir -p "$BACKUP_DIR"
  umask 077
  stamp=$(date -u +%Y%m%dT%H%M%SZ)
  target=$BACKUP_DIR/one-api-$stamp.db
  if sqlite3 "$NEW_API_DB" ".backup '$target'"; then
    find "$BACKUP_DIR" -maxdepth 1 -type f -name 'one-api-*.db' -mtime +7 -delete
  else
    rm -f "$target"
    notify error new_api_backup_failed 'SQLite online backup failed'
  fi
}

check_disk() {
  disk_line=$(df -Pk "$DEPLOY_PATH" | awk 'NR == 2 { sub(/%$/, "", $5); print $5 " " $4 }')
  used_percent=${disk_line%% *}
  available_kib=${disk_line##* }
  case "$used_percent:$available_kib" in
    *[!0-9:]*|'') return 0 ;;
  esac

  level=normal
  if [ "$used_percent" -ge "$CRITICAL_PERCENT" ]; then
    level=critical
  elif [ "$used_percent" -ge "$WARN_PERCENT" ]; then
    level=warning
  fi

  runtime_dir=/var/lib/sub2api-ops
  state_file=$runtime_dir/disk-level
  mkdir -p "$runtime_dir"
  previous=$(cat "$state_file" 2>/dev/null || true)
  [ "$level" = "$previous" ] && return 0
  printf '%s\n' "$level" > "$state_file"

  available_gib=$(awk -v kib="$available_kib" 'BEGIN { printf "%.2f", kib / 1048576 }')
  case "$level" in
    critical) notify critical disk_critical "root disk is $used_percent percent used; $available_gib GiB free" ;;
    warning) notify warning disk_warning "root disk is $used_percent percent used; $available_gib GiB free" ;;
    normal)
      [ -n "$previous" ] && notify info disk_recovered "root disk is $used_percent percent used; $available_gib GiB free"
      ;;
  esac
}

rotate_log "$STATE_DIR/deploy.log"
docker builder prune --filter until=168h --force >/dev/null 2>&1 || true
docker image prune --force >/dev/null 2>&1 || true
prune_old_release_images
backup_new_api_sqlite
check_disk
