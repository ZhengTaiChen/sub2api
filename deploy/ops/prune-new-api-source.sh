#!/bin/sh
# Remove reviewed, unused new-api source copies without touching the running
# container, its data directory, or Docker volumes. The command is dry-run by
# default; --apply is required for every destructive operation.

set -eu

SOURCE_ROOT=/opt/new-api/src
CURRENT_SOURCE=
ROLLBACK_SOURCE=
ARCHIVE_DIR=/opt/new-api/backups/source
APPLY=0
PRUNE_CURRENT_DEPENDENCIES=0

usage() {
  printf '%s\n' "Usage: $0 --current NAME --rollback NAME [options]" >&2
  printf '%s\n' '  --source-root PATH                 source root (default: /opt/new-api/src)' >&2
  printf '%s\n' '  --archive-dir PATH                 rollback archive directory' >&2
  printf '%s\n' '  --apply                            perform the reviewed cleanup' >&2
  printf '%s\n' '  --prune-current-dependencies      remove current web/node_modules caches' >&2
  exit 2
}

while [ $# -gt 0 ]; do
  case "$1" in
    --source-root) [ $# -ge 2 ] || usage; SOURCE_ROOT=$2; shift 2 ;;
    --current) [ $# -ge 2 ] || usage; CURRENT_SOURCE=$2; shift 2 ;;
    --rollback) [ $# -ge 2 ] || usage; ROLLBACK_SOURCE=$2; shift 2 ;;
    --archive-dir) [ $# -ge 2 ] || usage; ARCHIVE_DIR=$2; shift 2 ;;
    --apply) APPLY=1; shift ;;
    --prune-current-dependencies) PRUNE_CURRENT_DEPENDENCIES=1; shift ;;
    *) usage ;;
  esac
done

[ -n "$CURRENT_SOURCE" ] || usage
[ -n "$ROLLBACK_SOURCE" ] || usage
[ "$(id -u)" -eq 0 ] || {
  printf '%s\n' 'run this cleanup as root' >&2
  exit 1
}

command -v docker >/dev/null 2>&1 || {
  printf '%s\n' 'docker is required' >&2
  exit 1
}
command -v realpath >/dev/null 2>&1 || {
  printf '%s\n' 'realpath is required' >&2
  exit 1
}

is_simple_child_name() {
  case "$1" in
    ''|.|..|*/*|*\\*|*[!A-Za-z0-9._-]*) return 1 ;;
  esac
  return 0
}

is_simple_child_name "$CURRENT_SOURCE" && is_simple_child_name "$ROLLBACK_SOURCE" || {
  printf '%s\n' 'source names must be simple child directory names' >&2
  exit 2
}
[ "$CURRENT_SOURCE" != "$ROLLBACK_SOURCE" ] || {
  printf '%s\n' 'current and rollback source names must differ' >&2
  exit 2
}

case "$ARCHIVE_DIR" in
  /*) ;;
  *) printf '%s\n' 'archive directory must be an absolute path' >&2; exit 2 ;;
esac

root_real=$(realpath -e "$SOURCE_ROOT")
current_real=$(realpath -e "$root_real/$CURRENT_SOURCE")
rollback_real=$(realpath -e "$root_real/$ROLLBACK_SOURCE")
[ -d "$current_real" ] || { printf '%s\n' 'current source is not a directory' >&2; exit 1; }
[ -d "$rollback_real" ] || { printf '%s\n' 'rollback source is not a directory' >&2; exit 1; }
[ "$(dirname "$current_real")" = "$root_real" ] || { printf '%s\n' 'current source escapes source root' >&2; exit 1; }
[ "$(dirname "$rollback_real")" = "$root_real" ] || { printf '%s\n' 'rollback source escapes source root' >&2; exit 1; }

mounted_sources=$(docker ps -q | xargs -r docker inspect --format '{{range .Mounts}}{{.Source}}{{"\n"}}{{end}}' 2>/dev/null || true)
while IFS= read -r mounted; do
  [ -n "$mounted" ] || continue
  mounted_real=$(realpath -m "$mounted" 2>/dev/null || printf '%s' "$mounted")
  case "$mounted_real" in
    "$root_real"|"$root_real"/*)
      printf 'source tree is mounted by a running container: %s\n' "$mounted_real" >&2
      exit 1
      ;;
  esac
done <<EOF
$mounted_sources
EOF

if command -v lsof >/dev/null 2>&1; then
  open_pids=$(lsof +D "$root_real" -t 2>/dev/null | sort -u || true)
  if [ -n "$open_pids" ]; then
    printf 'source tree has open files held by process(es): %s\n' "$(printf '%s' "$open_pids" | tr '\n' ' ')" >&2
    exit 1
  fi
fi

archive_name="new-api-source-rollback-$(date -u +%Y%m%dT%H%M%SZ).tar.gz"
archive_path=$ARCHIVE_DIR/$archive_name

printf 'source root: %s\n' "$root_real"
printf 'keep current: %s\n' "$current_real"
printf 'archive rollback: %s -> %s\n' "$rollback_real" "$archive_path"
find "$root_real" -mindepth 1 -maxdepth 1 -type d -name 'new-api-*' -print | sort
if [ "$PRUNE_CURRENT_DEPENDENCIES" -eq 1 ]; then
  [ -d "$current_real/web/node_modules" ] && printf 'remove cache: %s\n' "$current_real/web/node_modules"
  [ -d "$current_real/node_modules" ] && printf 'remove cache: %s\n' "$current_real/node_modules"
fi

[ "$APPLY" -eq 1 ] || exit 0

umask 077
install -d -m 0700 "$ARCHIVE_DIR"
archive_tmp=$archive_path.tmp
rm -f "$archive_tmp"
tar --exclude='*/node_modules' --exclude='*/.cache' -czf "$archive_tmp" -C "$root_real" "$ROLLBACK_SOURCE"
tar -tzf "$archive_tmp" >/dev/null
mv -f "$archive_tmp" "$archive_path"

for candidate in "$root_real"/new-api-*; do
  [ -d "$candidate" ] || continue
  candidate_real=$(realpath -e "$candidate")
  [ "$candidate_real" = "$current_real" ] && continue
  [ "$(dirname "$candidate_real")" = "$root_real" ] || {
    printf 'refusing to remove path outside source root: %s\n' "$candidate_real" >&2
    exit 1
  }
  rm -rf -- "$candidate_real"
done

if [ "$PRUNE_CURRENT_DEPENDENCIES" -eq 1 ]; then
  for dependency_dir in "$current_real/web/node_modules" "$current_real/node_modules"; do
    [ -d "$dependency_dir" ] || continue
    dependency_real=$(realpath -e "$dependency_dir")
    case "$dependency_real" in
      "$current_real"/*/node_modules|"$current_real"/node_modules)
        rm -rf -- "$dependency_real"
        ;;
      *)
        printf 'refusing to remove unexpected dependency path: %s\n' "$dependency_real" >&2
        exit 1
        ;;
    esac
  done
fi

for old_archive in "$ARCHIVE_DIR"/new-api-source-rollback-*.tar.gz; do
  [ -f "$old_archive" ] || continue
  [ "$old_archive" = "$archive_path" ] || rm -f -- "$old_archive"
done

printf 'cleanup applied; remaining source usage:\n'
du -sh "$current_real" "$ARCHIVE_DIR" 2>/dev/null || true
