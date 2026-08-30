#!/bin/sh
# Deploy one immutable Sub2API image and roll back automatically on failure.
# This script performs no build and never touches application data, PostgreSQL,
# Redis, or the production environment file.

set -eu

usage() {
  printf '%s\n' "Usage: $0 --image IMAGE --digest sha256:... [--commit COMMIT] [--deploy-path PATH] [--edge-health-url URL] [--skip-pull]" >&2
  exit 2
}

IMAGE=
DIGEST=
COMMIT=
DEPLOY_PATH=/opt/sub2api
HEALTH_TIMEOUT=${DEPLOY_HEALTH_TIMEOUT:-120}
EDGE_HEALTH_TIMEOUT=${DEPLOY_EDGE_HEALTH_TIMEOUT:-30}
EXPECTED_ARCH=${DEPLOY_ARCH:-amd64}
MIN_FREE_KIB=${DEPLOY_MIN_FREE_KIB:-12582912}
LOG_MAX_BYTES=${DEPLOY_LOG_MAX_BYTES:-5242880}
LOG_MAX_ARCHIVES=${DEPLOY_LOG_MAX_ARCHIVES:-5}
ALERT_COMMAND=${DEPLOY_ALERT_COMMAND:-/usr/local/sbin/sub2api-alert}
HOUSEKEEPING_COMMAND=${DEPLOY_HOUSEKEEPING_COMMAND:-/usr/local/sbin/sub2api-housekeeping}
MAINTENANCE_FILE=${DEPLOY_MAINTENANCE_FILE:-/var/lib/caddy/maintenance/sub2api}
EDGE_HEALTH_URL=${DEPLOY_EDGE_HEALTH_URL:-}
LOCK_STALE_SECONDS=${DEPLOY_LOCK_STALE_SECONDS:-1800}
SKIP_PULL=0

while [ $# -gt 0 ]; do
  case "$1" in
    --image) [ $# -ge 2 ] || usage; IMAGE=$2; shift 2 ;;
    --digest) [ $# -ge 2 ] || usage; DIGEST=$2; shift 2 ;;
    --commit) [ $# -ge 2 ] || usage; COMMIT=$2; shift 2 ;;
    --deploy-path) [ $# -ge 2 ] || usage; DEPLOY_PATH=$2; shift 2 ;;
    --health-timeout) [ $# -ge 2 ] || usage; HEALTH_TIMEOUT=$2; shift 2 ;;
    --edge-health-url) [ $# -ge 2 ] || usage; EDGE_HEALTH_URL=$2; shift 2 ;;
    --skip-pull) SKIP_PULL=1; shift ;;
    *) usage ;;
  esac
done

[ -n "$IMAGE" ] || usage
[ -n "$DIGEST" ] || usage

COMPOSE_FILE=$DEPLOY_PATH/docker-compose.yml
STATE_DIR=$DEPLOY_PATH/.deploy
DEPLOY_CONFIG_FILE=${DEPLOY_CONFIG_FILE:-$STATE_DIR/deploy.env}

load_deploy_config() {
  [ -z "$EDGE_HEALTH_URL" ] || return 0
  [ -r "$DEPLOY_CONFIG_FILE" ] || return 0
  configured_edge_health_url=$(awk -F= '
    $1 == "DEPLOY_EDGE_HEALTH_URL" {
      value = substr($0, index($0, "=") + 1)
    }
    END { print value }
  ' "$DEPLOY_CONFIG_FILE")
  [ -n "$configured_edge_health_url" ] && EDGE_HEALTH_URL=$configured_edge_health_url
}

load_deploy_config

for numeric in HEALTH_TIMEOUT EDGE_HEALTH_TIMEOUT MIN_FREE_KIB LOG_MAX_BYTES LOG_MAX_ARCHIVES LOCK_STALE_SECONDS; do
  eval "numeric_value=\$$numeric"
  case "$numeric_value" in
    ''|*[!0-9]*|0) printf 'invalid %s\n' "$numeric" >&2; exit 2 ;;
  esac
done

case "$IMAGE" in
  *[!A-Za-z0-9_./:@-]*) printf 'invalid image reference\n' >&2; exit 2 ;;
esac
case "$DIGEST" in
  sha256:????????????????????????????????????????????????????????????????) ;;
  *) printf 'invalid image digest\n' >&2; exit 2 ;;
esac
case "$HEALTH_TIMEOUT" in
  ''|*[!0-9]*) printf 'invalid health timeout\n' >&2; exit 2 ;;
esac
case "$EDGE_HEALTH_URL" in
  ''|http://*|https://*) ;;
  *) printf 'invalid edge health URL\n' >&2; exit 2 ;;
esac

LOCK_DIR=$STATE_DIR/lock
OVERRIDE_FILE=$STATE_DIR/compose.deploy.yml
ROLLBACK_FILE=$STATE_DIR/compose.rollback.yml
LOG_FILE=$STATE_DIR/deploy.log
IMAGE_REF=$IMAGE@$DIGEST
RUNTIME_IMAGE_REF=$IMAGE_REF
PERSISTED_IMAGE_REF=$IMAGE_REF
RESOLVED_DIGEST=$DIGEST
DIGEST_KIND=manifest
CONTENT_ID=
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
ATTEMPT_ID=$STAMP-$$
BACKUP_FILE=$STATE_DIR/docker-compose.yml.before-$STAMP
PERSISTED=0
MAINTENANCE_ACTIVE=0
ROLLBACK_ATTEMPTED=0
LOCK_ACQUIRED=0
old_image=
old_id=
old_previous_image=
old_digest=
old_content_id=
old_commit=
health=unknown
http_ok=0
edge_ok=0

rotate_log() {
  [ -f "$LOG_FILE" ] || return 0
  log_size=$(wc -c < "$LOG_FILE" | tr -d ' ')
  [ "$log_size" -lt "$LOG_MAX_BYTES" ] && return 0

  i=$LOG_MAX_ARCHIVES
  while [ "$i" -gt 1 ]; do
    previous=$((i - 1))
    [ -f "$LOG_FILE.$previous" ] && mv -f "$LOG_FILE.$previous" "$LOG_FILE.$i"
    i=$previous
  done
  [ -f "$LOG_FILE" ] && mv -f "$LOG_FILE" "$LOG_FILE.1"
}

log() {
  message=$1
  mkdir -p "$STATE_DIR"
  rotate_log
  printf '[%s] %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$message" | tee -a "$LOG_FILE"
}

notify() {
  severity=$1
  event=$2
  message=$3

  if [ -x "$ALERT_COMMAND" ]; then
    "$ALERT_COMMAND" "$severity" "$event" "$message" >/dev/null 2>&1 || log "alert delivery failed: $event"
    return 0
  fi
  if command -v sudo >/dev/null 2>&1 && sudo -n test -x "$ALERT_COMMAND" 2>/dev/null; then
    sudo -n "$ALERT_COMMAND" "$severity" "$event" "$message" >/dev/null 2>&1 || log "alert delivery failed: $event"
  fi
}

run_housekeeping() {
  [ -x "$HOUSEKEEPING_COMMAND" ] || return 1
  if [ "$(id -u)" -eq 0 ]; then
    "$HOUSEKEEPING_COMMAND" --emergency
    return $?
  fi
  if command -v sudo >/dev/null 2>&1 && sudo -n test -x "$HOUSEKEEPING_COMMAND" 2>/dev/null; then
    sudo -n "$HOUSEKEEPING_COMMAND" --emergency
    return $?
  fi
  return 1
}

read_state_value() {
  state_file=$1
  [ -r "$state_file" ] || return 0
  sed -n '1{s/\r$//;p;}' "$state_file"
}

record_state() {
  status=$1
  detail=$2
  safe_detail=$(printf '%s' "$detail" | tr '\n' ' ')
  rollback_backup=
  [ -f "$BACKUP_FILE" ] && rollback_backup=$BACKUP_FILE
  tmp_file=$STATE_DIR/last-deployment.env.tmp
  mkdir -p "$STATE_DIR"
  umask 077
  {
    printf 'attempt_id=%s\n' "$ATTEMPT_ID"
    printf 'status=%s\n' "$status"
    printf 'updated_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf 'image=%s\n' "$RUNTIME_IMAGE_REF"
    printf 'digest=%s\n' "$RESOLVED_DIGEST"
    printf 'requested_digest=%s\n' "$DIGEST"
    printf 'digest_kind=%s\n' "$DIGEST_KIND"
    printf 'content_id=%s\n' "$CONTENT_ID"
    printf 'commit=%s\n' "$COMMIT"
    printf 'previous_image=%s\n' "$old_image"
    printf 'previous_digest=%s\n' "$old_digest"
    printf 'previous_content_id=%s\n' "$old_content_id"
    printf 'previous_commit=%s\n' "$old_commit"
    printf 'rollback_compose_backup=%s\n' "$rollback_backup"
    printf 'prior_successful_image=%s\n' "$old_previous_image"
    printf 'detail=%s\n' "$safe_detail"
  } > "$tmp_file"
  mv -f "$tmp_file" "$STATE_DIR/last-deployment.env"
}

die() {
  reason=$1
  record_state failed "$reason"
  log "ERROR: $reason" >&2
  notify error deployment_failed "$reason"
  exit 1
}

begin_maintenance() {
  [ -n "$MAINTENANCE_FILE" ] || return 0
  maintenance_dir=$(dirname "$MAINTENANCE_FILE")
  if [ -w "$maintenance_dir" ]; then
    : > "$MAINTENANCE_FILE"
  elif [ "$(id -u)" -eq 0 ]; then
    install -d -m 0755 "$maintenance_dir"
    install -m 0644 /dev/null "$MAINTENANCE_FILE"
  elif command -v sudo >/dev/null 2>&1 &&
    sudo -n install -d -m 0755 "$maintenance_dir" 2>/dev/null &&
    sudo -n install -m 0644 /dev/null "$MAINTENANCE_FILE" 2>/dev/null; then
    :
  else
    log "maintenance marker unavailable: $MAINTENANCE_FILE"
    return 0
  fi
  MAINTENANCE_ACTIVE=1
  log "maintenance marker enabled"
}

end_maintenance() {
  [ "$MAINTENANCE_ACTIVE" -eq 1 ] || return 0
  if [ -w "$(dirname "$MAINTENANCE_FILE")" ]; then
    rm -f "$MAINTENANCE_FILE"
  elif [ "$(id -u)" -eq 0 ]; then
    rm -f "$MAINTENANCE_FILE"
  elif command -v sudo >/dev/null 2>&1; then
    sudo -n rm -f "$MAINTENANCE_FILE" 2>/dev/null || log "could not remove maintenance marker"
  fi
  MAINTENANCE_ACTIVE=0
  log "maintenance marker cleared"
}

cleanup() {
  end_maintenance || true
  rm -f "$OVERRIDE_FILE" "$ROLLBACK_FILE"
  if [ "$LOCK_ACQUIRED" -eq 1 ]; then
    rm -f "$LOCK_DIR/pid" "$LOCK_DIR/started-at"
    rmdir "$LOCK_DIR" 2>/dev/null || true
  fi
}
trap 'cleanup' EXIT
trap 'exit 130' INT TERM

acquire_lock() {
  if mkdir "$LOCK_DIR" 2>/dev/null; then
    printf '%s\n' "$$" > "$LOCK_DIR/pid"
    date -u +%s > "$LOCK_DIR/started-at"
    LOCK_ACQUIRED=1
    return 0
  fi

  lock_pid=
  [ -r "$LOCK_DIR/pid" ] && lock_pid=$(cat "$LOCK_DIR/pid" 2>/dev/null || true)
  if [ -n "$lock_pid" ] && kill -0 "$lock_pid" 2>/dev/null; then
    die "another deployment is already running (pid $lock_pid)"
  fi

  lock_started=0
  [ -r "$LOCK_DIR/started-at" ] && lock_started=$(cat "$LOCK_DIR/started-at" 2>/dev/null || printf '0')
  now=$(date -u +%s)
  age=$((now - lock_started))
  if [ "$age" -lt "$LOCK_STALE_SECONDS" ]; then
    stale_pid=${lock_pid:-unknown}
    die "deployment lock exists and has no live owner (pid $stale_pid, age ${age}s)"
  fi

  stale_pid=${lock_pid:-unknown}
  log "removing stale deployment lock (pid $stale_pid, age ${age}s)"
  rm -rf "$LOCK_DIR"
  mkdir "$LOCK_DIR" || die 'failed to acquire deployment lock'
  printf '%s\n' "$$" > "$LOCK_DIR/pid"
  date -u +%s > "$LOCK_DIR/started-at"
  LOCK_ACQUIRED=1
}

check_free_space() {
  available_kib=$(df -Pk "$DEPLOY_PATH" | awk 'NR == 2 { print $4 }')
  case "$available_kib" in
    ''|*[!0-9]*) die 'could not determine free disk space' ;;
  esac
  if [ "$available_kib" -lt "$MIN_FREE_KIB" ] &&
    [ -x "$HOUSEKEEPING_COMMAND" ]; then
    log "free disk below threshold; running controlled housekeeping"
    run_housekeeping >/dev/null 2>&1 || log 'controlled housekeeping failed'
    available_kib=$(df -Pk "$DEPLOY_PATH" | awk 'NR == 2 { print $4 }')
  fi
  case "$available_kib" in
    ''|*[!0-9]*) die 'could not determine free disk space after housekeeping' ;;
  esac
  if [ "$available_kib" -lt "$MIN_FREE_KIB" ]; then
    available_gib=$(awk -v kib="$available_kib" 'BEGIN { printf "%.2f", kib / 1048576 }')
    required_gib=$(awk -v kib="$MIN_FREE_KIB" 'BEGIN { printf "%.2f", kib / 1048576 }')
    message=$(printf 'deployment blocked: only %s GiB free; %s GiB required' "$available_gib" "$required_gib")
    notify warning deployment_blocked_low_disk "$message"
    die "$message"
  fi
  free_gib=$(awk -v kib="$available_kib" 'BEGIN { printf "%.2f GiB", kib / 1048576 }')
  log "free disk check passed: $free_gib"
}

wait_for_container_health() {
  healthy=0
  i=0
  health=unknown
  http_ok=0

  while [ "$i" -lt "$HEALTH_TIMEOUT" ]; do
    health=$(docker inspect sub2api --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' 2>/dev/null || true)
    port_mapping=$(docker port sub2api 8080/tcp 2>/dev/null | head -n 1 || true)
    host_port=${port_mapping##*:}
    if [ -n "$host_port" ] &&
      curl -fsS --connect-timeout 2 --max-time 5 "http://127.0.0.1:$host_port/health" >/dev/null 2>&1; then
      http_ok=1
    else
      http_ok=0
    fi

    if [ "$http_ok" -eq 1 ] && { [ "$health" = healthy ] || [ "$health" = none ]; }; then
      healthy=1
      return 0
    fi
    [ "$health" = unhealthy ] && break
    i=$((i + 1))
    sleep 1
  done

  return 1
}

wait_for_edge_health() {
  [ -n "$EDGE_HEALTH_URL" ] || return 0
  edge_ok=0
  i=0
  while [ "$i" -lt "$EDGE_HEALTH_TIMEOUT" ]; do
    if curl -fsS --connect-timeout 2 --max-time 5 "$EDGE_HEALTH_URL" >/dev/null 2>&1; then
      edge_ok=1
      return 0
    fi
    i=$((i + 1))
    sleep 1
  done
  return 1
}

rollback() {
  reason=$1
  [ "$ROLLBACK_ATTEMPTED" -eq 0 ] || return 1
  ROLLBACK_ATTEMPTED=1
  log "deployment failed; restoring previous container: $reason"
  rm -f "$OVERRIDE_FILE" "$ROLLBACK_FILE"

  if [ "$PERSISTED" -eq 1 ] && [ -f "$BACKUP_FILE" ]; then
    cp "$BACKUP_FILE" "$COMPOSE_FILE"
  fi

  rollback_image=$old_image
  [ -n "$rollback_image" ] || rollback_image=$old_previous_image
  if [ -n "$rollback_image" ]; then
    printf '%s\n' 'services:' '  sub2api:' "    image: $rollback_image" > "$ROLLBACK_FILE"
    if ! docker compose -f "$COMPOSE_FILE" -f "$ROLLBACK_FILE" up -d --no-deps sub2api >/dev/null 2>&1; then
      log 'rollback container recreation failed'
      return 1
    fi
  elif ! docker compose -f "$COMPOSE_FILE" up -d --no-deps sub2api >/dev/null 2>&1; then
    log 'rollback container recreation failed'
    return 1
  fi

  if wait_for_container_health; then
    log "rollback healthy: ${rollback_image:-unknown} (${old_id:-unknown})"
    return 0
  fi
  log "rollback health check failed (docker=$health http=$http_ok)"
  return 1
}

fail_after_switch() {
  reason=$1
  if rollback "$reason"; then
    end_maintenance || true
    record_state rolled_back "$reason"
    notify error deployment_rolled_back "$reason"
    log "ERROR: $reason; rollback completed" >&2
  else
    record_state rollback_failed "$reason"
    notify critical deployment_rollback_failed "$reason"
    log "ERROR: $reason; rollback failed" >&2
  fi
  exit 1
}

persist_compose_image() {
  rm -f "$OVERRIDE_FILE"
  mv "$BACKUP_FILE" "$BACKUP_FILE.tmp"
  if ! awk -v image="$PERSISTED_IMAGE_REF" '
    /^[[:space:]]*sub2api:[[:space:]]*$/ { in_service=1; print; next }
    in_service && $0 ~ /^[^[:space:]]/ { in_service=0 }
    in_service && !done && $0 ~ /^[[:space:]]+image:[[:space:]]*/ {
      sub(/image:.*/, "image: " image)
      done=1
    }
    { print }
    END { if (!done) exit 1 }
  ' "$BACKUP_FILE.tmp" > "$COMPOSE_FILE.tmp"; then
    rm -f "$COMPOSE_FILE.tmp"
    mv "$BACKUP_FILE.tmp" "$BACKUP_FILE"
    return 1
  fi
  mv "$COMPOSE_FILE.tmp" "$COMPOSE_FILE"
  mv "$BACKUP_FILE.tmp" "$BACKUP_FILE"
  PERSISTED=1
  docker compose -f "$COMPOSE_FILE" config -q
}

retain_release_images() {
  if [ -n "$old_id" ] && docker image inspect "$old_id" >/dev/null 2>&1; then
    docker tag "$old_id" sub2api-retain:previous >/dev/null 2>&1 || log 'could not tag previous rollback image'
  fi
  docker tag "$RUNTIME_IMAGE_REF" sub2api-retain:current >/dev/null 2>&1 || log 'could not tag current release image'
}

save_release_state() {
  umask 077
  printf '%s\n' "$PERSISTED_IMAGE_REF" > "$STATE_DIR/current-image"
  printf '%s\n' "$RESOLVED_DIGEST" > "$STATE_DIR/current-digest"
  printf '%s\n' "$DIGEST" > "$STATE_DIR/requested-digest"
  printf '%s\n' "$CONTENT_ID" > "$STATE_DIR/current-content-id"
  printf '%s\n' "$DIGEST_KIND" > "$STATE_DIR/digest-kind"
  printf '%s\n' "${old_image:-}" > "$STATE_DIR/previous-image"
  printf '%s\n' "${old_digest:-}" > "$STATE_DIR/previous-digest"
  printf '%s\n' "${old_content_id:-}" > "$STATE_DIR/previous-content-id"
  printf '%s\n' "${old_commit:-}" > "$STATE_DIR/previous-commit"
  printf '%s\n' "$BACKUP_FILE" > "$STATE_DIR/rollback-compose-backup"
  printf '%s\n' "$COMMIT" > "$STATE_DIR/current-commit"
}

retain_latest_compose_backup() {
  for backup in $(find "$STATE_DIR" -maxdepth 1 -type f -name 'docker-compose.yml.before-*' -print | sort); do
    [ "$backup" = "$BACKUP_FILE" ] || rm -f "$backup"
  done
}

[ -f "$COMPOSE_FILE" ] || die "compose file not found: $COMPOSE_FILE"
command -v docker >/dev/null 2>&1 || die 'docker is required'
docker compose version >/dev/null 2>&1 || die 'docker compose is required'
command -v curl >/dev/null 2>&1 || die 'curl is required'
cd "$DEPLOY_PATH"
mkdir -p "$STATE_DIR"
acquire_lock
check_free_space

old_image=$(docker inspect sub2api --format '{{.Config.Image}}' 2>/dev/null || true)
old_id=$(docker inspect sub2api --format '{{.Image}}' 2>/dev/null || true)
old_previous_image=$(read_state_value "$STATE_DIR/previous-image")
old_digest=$(read_state_value "$STATE_DIR/current-digest")
old_content_id=$(read_state_value "$STATE_DIR/current-content-id")
old_commit=$(read_state_value "$STATE_DIR/current-commit")
[ -n "$old_content_id" ] || old_content_id=$old_id

if [ "$SKIP_PULL" -eq 1 ]; then
  if docker image inspect "$IMAGE_REF" >/dev/null 2>&1; then
    log "using locally loaded immutable image $IMAGE_REF"
  elif docker image inspect "$IMAGE" >/dev/null 2>&1; then
    # docker load may restore a version tag without RepoDigests. The release
    # tag is unique; retain the requested digest in deployment state below.
    RUNTIME_IMAGE_REF=$IMAGE
    PERSISTED_IMAGE_REF=$IMAGE
    log "using locally loaded tagged image $IMAGE (digest $DIGEST)"
  else
    die 'locally loaded image is not available; current service was not changed'
  fi
else
  log "pulling $IMAGE_REF"
  docker pull "$IMAGE_REF" >/dev/null || die 'docker pull failed; current service was not changed'
fi

CONTENT_ID=$(docker image inspect "$RUNTIME_IMAGE_REF" --format '{{.Id}}' 2>/dev/null || true)
case "$CONTENT_ID" in
  sha256:????????????????????????????????????????????????????????????????) ;;
  *) die "could not determine loaded image content ID for $RUNTIME_IMAGE_REF" ;;
esac

if [ "$SKIP_PULL" -eq 1 ]; then
  # A local docker save/load may normalize the image config and produce a
  # daemon-specific content ID. Persist the ID from the target daemon rather
  # than the ID observed in the build environment.
  RESOLVED_DIGEST=$CONTENT_ID
  DIGEST_KIND=content
fi

arch=$(docker image inspect "$RUNTIME_IMAGE_REF" --format '{{.Architecture}}' 2>/dev/null || true)
[ "$arch" = "$EXPECTED_ARCH" ] || die "image architecture is $arch, expected $EXPECTED_ARCH"

if [ -n "$COMMIT" ]; then
  image_commit=$(docker image inspect "$RUNTIME_IMAGE_REF" --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' 2>/dev/null || true)
  [ "$image_commit" = "$COMMIT" ] || die "image commit is $image_commit, expected $COMMIT"
fi

cp "$COMPOSE_FILE" "$BACKUP_FILE"
printf '%s\n' 'services:' '  sub2api:' "    image: $RUNTIME_IMAGE_REF" > "$OVERRIDE_FILE"
docker compose -f "$COMPOSE_FILE" -f "$OVERRIDE_FILE" config -q || die 'deployment compose configuration is invalid'

begin_maintenance
if ! docker compose -f "$COMPOSE_FILE" -f "$OVERRIDE_FILE" up -d --no-deps sub2api; then
  fail_after_switch 'container recreation failed'
fi

container_id=$(docker inspect sub2api --format '{{.Id}}' 2>/dev/null || true)
[ -n "$container_id" ] || fail_after_switch 'new container was not created'

if ! wait_for_container_health; then
  docker logs --tail 100 sub2api >&2 2>/dev/null || true
  fail_after_switch "internal health check failed (docker=$health http=$http_ok)"
fi

if ! wait_for_edge_health; then
  fail_after_switch "edge health check failed for $EDGE_HEALTH_URL"
fi

if ! persist_compose_image; then
  fail_after_switch 'failed to persist sub2api image reference'
fi

retain_release_images
save_release_state
retain_latest_compose_backup
end_maintenance
record_state success "container=$container_id docker_health=$health edge_health=$edge_ok"
notify info deployment_succeeded "image=$PERSISTED_IMAGE_REF commit=$COMMIT"
log "deployment succeeded: $PERSISTED_IMAGE_REF digest=$RESOLVED_DIGEST requested_digest=$DIGEST content_id=$CONTENT_ID container=$container_id docker_health=$health edge_health=$edge_ok"
