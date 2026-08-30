#!/bin/sh
# Emit one alert when Sub2API becomes unhealthy and one when it recovers.
# Planned deployments create the maintenance marker, so they do not generate
# false positives while the container is being replaced.

set -eu

CONFIG_FILE=${SUB2API_OPS_CONFIG:-/etc/sub2api/ops.env}
if [ -r "$CONFIG_FILE" ]; then
  # This root-owned file only carries operational paths and thresholds.
  . "$CONFIG_FILE"
fi

DEPLOY_PATH=${SUB2API_DEPLOY_PATH:-/opt/sub2api}
STATE_FILE=${SUB2API_SERVICE_HEALTH_STATE:-/var/lib/sub2api-ops/sub2api-service-health.env}
ALERT_COMMAND=${SUB2API_SERVICE_HEALTH_ALERT_COMMAND:-/usr/local/sbin/sub2api-alert}
MAINTENANCE_FILE=${SUB2API_SERVICE_HEALTH_MAINTENANCE_FILE:-/var/lib/caddy/maintenance/sub2api}
INTERNAL_HEALTH_URL=${SUB2API_INTERNAL_HEALTH_URL:-http://127.0.0.1:8082/health}
EDGE_HEALTH_URL=${SUB2API_EDGE_HEALTH_URL:-}

load_edge_health_url() {
  [ -n "$EDGE_HEALTH_URL" ] && return 0
  deploy_config=$DEPLOY_PATH/.deploy/deploy.env
  [ -r "$deploy_config" ] || return 0
  EDGE_HEALTH_URL=$(awk -F= '
    $1 == "DEPLOY_EDGE_HEALTH_URL" {
      value = substr($0, index($0, "=") + 1)
    }
    END { print value }
  ' "$deploy_config")
}

read_previous_status() {
  [ -r "$STATE_FILE" ] || return 0
  awk -F= '$1 == "status" { print substr($0, index($0, "=") + 1); exit }' "$STATE_FILE"
}

write_state() {
  status=$1
  detail=$2
  state_dir=$(dirname "$STATE_FILE")
  mkdir -p "$state_dir"
  umask 077
  tmp_file=$(mktemp "$STATE_FILE.tmp.XXXXXX")
  {
    printf 'status=%s\n' "$status"
    printf 'updated_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf 'detail=%s\n' "$detail"
  } > "$tmp_file"
  chmod 0600 "$tmp_file"
  mv -f "$tmp_file" "$STATE_FILE"
}

notify() {
  severity=$1
  event=$2
  message=$3
  [ -x "$ALERT_COMMAND" ] || return 0
  "$ALERT_COMMAND" "$severity" "$event" "$message" > /dev/null 2>&1 || true
}

check_http_health() {
  url=$1
  curl -fsS --connect-timeout 2 --max-time 5 "$url" > /dev/null 2>&1
}

load_edge_health_url

# The deployment script owns this marker. Preserve the last observed state so
# the next regular probe can still emit a real recovery event when appropriate.
[ -e "$MAINTENANCE_FILE" ] && exit 0

status=healthy
detail=healthy
container_state=$(docker inspect sub2api --format '{{.State.Running}} {{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' 2>/dev/null || true)
case "$container_state" in
  'true healthy'|'true none') ;;
  *)
    status=failed
    detail="container state ${container_state:-missing}"
    ;;
esac

if [ "$status" = healthy ] && ! check_http_health "$INTERNAL_HEALTH_URL"; then
  status=failed
  detail="internal health check failed: $INTERNAL_HEALTH_URL"
fi

if [ "$status" = healthy ] && [ -n "$EDGE_HEALTH_URL" ] && ! check_http_health "$EDGE_HEALTH_URL"; then
  status=failed
  detail="edge health check failed: $EDGE_HEALTH_URL"
fi

previous_status=$(read_previous_status)
if [ "$status" = failed ] && [ "$previous_status" != failed ]; then
  notify error sub2api_service_unhealthy "Sub2API health check failed: $detail"
elif [ "$status" = healthy ] && [ "$previous_status" = failed ]; then
  notify info sub2api_service_recovered "Sub2API health check recovered"
fi

write_state "$status" "$detail"
