#!/bin/sh
# Send a compact operational event to an HTTPS webhook. The endpoint is kept
# outside the repository in a root-only file.

set -eu

SECRET_FILE=${SUB2API_ALERT_WEBHOOK_FILE:-/etc/sub2api/alert-webhook-url}
CONNECT_TIMEOUT=${SUB2API_ALERT_CONNECT_TIMEOUT:-5}
MAX_TIME=${SUB2API_ALERT_MAX_TIME:-15}

usage() {
  printf '%s\n' "Usage: $0 <info|warning|error|critical> <event> <message>" >&2
  exit 2
}

[ $# -eq 3 ] || usage
severity=$1
event=$2
message=$3

case "$severity" in
  info|warning|error|critical) ;;
  *) usage ;;
esac
case "$event" in
  *[!A-Za-z0-9_.-]*|'') usage ;;
esac

# No configured endpoint is intentionally a no-op. This lets a production
# deployment run before the operator has selected a notification provider.
[ -r "$SECRET_FILE" ] || exit 0
[ "$(stat -c '%U' "$SECRET_FILE" 2>/dev/null || true)" = root ] || exit 1
case "$(stat -c '%a' "$SECRET_FILE" 2>/dev/null || true)" in
  400|600) ;;
  *) exit 1 ;;
esac

webhook_url=$(tr -d '\r\n' < "$SECRET_FILE")
[ -n "$webhook_url" ] || exit 0
case "$webhook_url" in
  https://*) ;;
  *) exit 1 ;;
esac

payload=$(python3 - "$severity" "$event" "$message" <<'PY'
import json
import socket
import sys
from datetime import datetime, timezone

severity, event, message = sys.argv[1:]
print(json.dumps({
    "service": "sub2api",
    "severity": severity,
    "event": event,
    "message": message,
    "host": socket.gethostname(),
    "observed_at": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"),
}, ensure_ascii=True, separators=(",", ":")))
PY
)

curl --fail --silent --show-error \
  --connect-timeout "$CONNECT_TIMEOUT" \
  --max-time "$MAX_TIME" \
  -H 'Content-Type: application/json' \
  --data "$payload" \
  "$webhook_url" >/dev/null
