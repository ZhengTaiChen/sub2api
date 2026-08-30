#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
script=$repo_root/deploy/ops/sub2api-service-health.sh

sh -n "$script"

tmp_dir=$(mktemp -d)
trap 'rm -rf "$tmp_dir"' EXIT INT TERM
fake_bin=$tmp_dir/bin
state_file=$tmp_dir/state/health.env
maintenance_file=$tmp_dir/maintenance/sub2api
alert_log=$tmp_dir/alerts.log
mkdir -p "$fake_bin" "$(dirname "$maintenance_file")"

cat > "$fake_bin/docker" <<'EOF'
#!/bin/sh
[ "$1" = inspect ] || exit 1
printf '%s\n' "${FAKE_DOCKER_STATE:-true healthy}"
EOF
cat > "$fake_bin/curl" <<'EOF'
#!/bin/sh
case "$*" in
  *edge-health*) [ "${FAKE_CURL_EDGE_FAIL:-0}" != 1 ] ;;
  *) [ "${FAKE_CURL_INTERNAL_FAIL:-0}" != 1 ] ;;
esac
EOF
cat > "$fake_bin/alert" <<'EOF'
#!/bin/sh
printf '%s|%s|%s\n' "$1" "$2" "$3" >> "$FAKE_ALERT_LOG"
EOF
chmod +x "$fake_bin/docker" "$fake_bin/curl" "$fake_bin/alert"

run_monitor() {
  PATH="$fake_bin:$PATH" \
  FAKE_ALERT_LOG="$alert_log" \
  SUB2API_OPS_CONFIG="$tmp_dir/missing.env" \
  SUB2API_SERVICE_HEALTH_STATE="$state_file" \
  SUB2API_SERVICE_HEALTH_MAINTENANCE_FILE="$maintenance_file" \
  SUB2API_SERVICE_HEALTH_ALERT_COMMAND="$fake_bin/alert" \
  SUB2API_EDGE_HEALTH_URL=http://edge-health \
  "$script"
}

run_monitor
grep -Fqx 'status=healthy' "$state_file"
test ! -e "$alert_log"

FAKE_CURL_INTERNAL_FAIL=1 run_monitor
grep -Fqx 'status=failed' "$state_file"
grep -Fq 'sub2api_service_unhealthy' "$alert_log"
test "$(wc -l < "$alert_log" | tr -d ' ')" = 1

FAKE_CURL_INTERNAL_FAIL=1 run_monitor
test "$(wc -l < "$alert_log" | tr -d ' ')" = 1

run_monitor
grep -Fqx 'status=healthy' "$state_file"
grep -Fq 'sub2api_service_recovered' "$alert_log"
test "$(wc -l < "$alert_log" | tr -d ' ')" = 2

touch "$maintenance_file"
FAKE_CURL_EDGE_FAIL=1 run_monitor
grep -Fqx 'status=healthy' "$state_file"
test "$(wc -l < "$alert_log" | tr -d ' ')" = 2

printf 'sub2api service health test passed\n'
