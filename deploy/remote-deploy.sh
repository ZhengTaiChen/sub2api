#!/bin/sh
# Deploy one immutable Sub2API image and roll back automatically on failure.
# This script performs no build and never touches application data, PostgreSQL,
# Redis, or the production environment file.

set -eu

usage() {
  printf '%s\n' "Usage: $0 --image IMAGE --digest sha256:... [--commit COMMIT] [--deploy-path PATH]" >&2
  exit 2
}

IMAGE=
DIGEST=
COMMIT=
DEPLOY_PATH=/opt/sub2api
HEALTH_TIMEOUT=${DEPLOY_HEALTH_TIMEOUT:-60}
EXPECTED_ARCH=${DEPLOY_ARCH:-amd64}

while [ $# -gt 0 ]; do
  case "$1" in
    --image) [ $# -ge 2 ] || usage; IMAGE=$2; shift 2 ;;
    --digest) [ $# -ge 2 ] || usage; DIGEST=$2; shift 2 ;;
    --commit) [ $# -ge 2 ] || usage; COMMIT=$2; shift 2 ;;
    --deploy-path) [ $# -ge 2 ] || usage; DEPLOY_PATH=$2; shift 2 ;;
    --health-timeout) [ $# -ge 2 ] || usage; HEALTH_TIMEOUT=$2; shift 2 ;;
    *) usage ;;
  esac
done

[ -n "$IMAGE" ] || usage
[ -n "$DIGEST" ] || usage

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

COMPOSE_FILE=$DEPLOY_PATH/docker-compose.yml
STATE_DIR=$DEPLOY_PATH/.deploy
LOCK_DIR=$STATE_DIR/lock
OVERRIDE_FILE=$STATE_DIR/compose.deploy.yml
ROLLBACK_FILE=$STATE_DIR/compose.rollback.yml
LOG_FILE=$STATE_DIR/deploy.log
IMAGE_REF=$IMAGE@$DIGEST
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
BACKUP_FILE=$STATE_DIR/docker-compose.yml.before-$STAMP
PERSISTED=0

log() {
  message=$1
  mkdir -p "$STATE_DIR"
  printf '[%s] %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$message" | tee -a "$LOG_FILE"
}

die() {
  log "ERROR: $1" >&2
  exit 1
}

cleanup() {
  rm -f "$OVERRIDE_FILE" "$ROLLBACK_FILE"
  rmdir "$LOCK_DIR" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

[ -f "$COMPOSE_FILE" ] || die "compose file not found: $COMPOSE_FILE"
command -v docker >/dev/null 2>&1 || die 'docker is required'
docker compose version >/dev/null 2>&1 || die 'docker compose is required'
command -v curl >/dev/null 2>&1 || die 'curl is required'
cd "$DEPLOY_PATH"
mkdir -p "$STATE_DIR"
if ! mkdir "$LOCK_DIR" 2>/dev/null; then
  die 'another deployment is already running'
fi

old_image=$(docker inspect sub2api --format '{{.Config.Image}}' 2>/dev/null || true)
old_id=$(docker inspect sub2api --format '{{.Image}}' 2>/dev/null || true)

log "pulling $IMAGE_REF"
docker pull "$IMAGE_REF" >/dev/null || die 'docker pull failed; current service was not changed'

arch=$(docker image inspect "$IMAGE_REF" --format '{{.Architecture}}' 2>/dev/null || true)
[ "$arch" = "$EXPECTED_ARCH" ] || die "image architecture is $arch, expected $EXPECTED_ARCH"

if [ -n "$COMMIT" ]; then
  image_commit=$(docker image inspect "$IMAGE_REF" --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' 2>/dev/null || true)
  [ "$image_commit" = "$COMMIT" ] || die "image commit is $image_commit, expected $COMMIT"
fi

cp "$COMPOSE_FILE" "$BACKUP_FILE"
printf '%s\n' 'services:' '  sub2api:' "    image: $IMAGE_REF" > "$OVERRIDE_FILE"

docker compose -f "$COMPOSE_FILE" -f "$OVERRIDE_FILE" config -q || die 'deployment compose configuration is invalid'

rollback() {
  log 'deployment failed; restoring previous container'
  rm -f "$OVERRIDE_FILE" "$ROLLBACK_FILE"
  if [ "$PERSISTED" -eq 1 ]; then
    cp "$BACKUP_FILE" "$COMPOSE_FILE"
  fi
  if [ -n "$old_image" ]; then
    printf '%s\n' 'services:' '  sub2api:' "    image: $old_image" > "$ROLLBACK_FILE"
    docker compose -f "$COMPOSE_FILE" -f "$ROLLBACK_FILE" up -d --no-deps sub2api >/dev/null 2>&1 || true
  else
    docker compose -f "$COMPOSE_FILE" up -d --no-deps sub2api >/dev/null 2>&1 || true
  fi
  log "rollback requested; previous image was ${old_image:-unknown} (${old_id:-unknown})"
}

if ! docker compose -f "$COMPOSE_FILE" -f "$OVERRIDE_FILE" up -d --no-deps sub2api; then
  rollback
  die 'container recreation failed'
fi

container_id=$(docker inspect sub2api --format '{{.Id}}' 2>/dev/null || true)
[ -n "$container_id" ] || { rollback; die 'new container was not created'; }

healthy=0
i=0
health=unknown
http_ok=0
while [ "$i" -lt "$HEALTH_TIMEOUT" ]; do
  health=$(docker inspect sub2api --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' 2>/dev/null || true)
  port_mapping=$(docker port sub2api 8080/tcp 2>/dev/null | head -n 1 || true)
  host_port=${port_mapping##*:}
  if [ -n "$host_port" ] && curl -fsS --max-time 1 "http://127.0.0.1:$host_port/health" >/dev/null 2>&1; then
    http_ok=1
  else
    http_ok=0
  fi
  if [ "$health" = healthy ] && [ "$http_ok" -eq 1 ]; then
    healthy=1
    break
  fi
  if [ "$health" = none ] && [ "$http_ok" -eq 1 ]; then
    healthy=1
    break
  fi
  [ "$health" = unhealthy ] && break
  i=$((i + 1))
  sleep 1
done

if [ "$healthy" -ne 1 ]; then
  rollback
  docker logs --tail 100 sub2api >&2 2>/dev/null || true
  die "health check failed (docker=$health http=$http_ok)"
fi

# Persist the exact image reference only after the replacement is healthy.
# The original file is retained as a rollback artifact.
rm -f "$OVERRIDE_FILE"
mv "$BACKUP_FILE" "$BACKUP_FILE.tmp"
awk -v image="$IMAGE_REF" '
  /^[[:space:]]*sub2api:[[:space:]]*$/ { in_service=1; print; next }
  in_service && $0 ~ /^[^[:space:]]/ { in_service=0 }
  in_service && !done && $0 ~ /^[[:space:]]+image:[[:space:]]*/ {
    sub(/image:.*/, "image: " image)
    done=1
  }
  { print }
  END { if (!done) exit 1 }
' "$BACKUP_FILE.tmp" > "$COMPOSE_FILE.tmp" || { rm -f "$COMPOSE_FILE.tmp"; mv "$BACKUP_FILE.tmp" "$BACKUP_FILE"; rollback; die 'failed to persist sub2api image reference'; }
mv "$COMPOSE_FILE.tmp" "$COMPOSE_FILE"
mv "$BACKUP_FILE.tmp" "$BACKUP_FILE" 2>/dev/null || true
PERSISTED=1

docker compose -f "$COMPOSE_FILE" config -q || { rollback; die 'persisted compose configuration is invalid'; }
printf '%s\n' "$IMAGE_REF" > "$STATE_DIR/current-image"
printf '%s\n' "${old_image:-}" > "$STATE_DIR/previous-image"
printf '%s\n' "$COMMIT" > "$STATE_DIR/current-commit"
log "deployment succeeded: $IMAGE_REF container=$container_id docker_health=$health"

# Retain only the newest compose backup for manual rollback.
for backup in $(find "$STATE_DIR" -maxdepth 1 -type f -name 'docker-compose.yml.before-*' -print | sort); do
  [ "$backup" = "$BACKUP_FILE" ] || rm -f "$backup"
done
