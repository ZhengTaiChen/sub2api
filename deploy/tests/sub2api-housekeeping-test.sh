#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
script=$repo_root/deploy/ops/sub2api-housekeeping.sh

sh -n "$script"

tmp_dir=$(mktemp -d)
trap 'rm -rf "$tmp_dir"' EXIT INT TERM
fake_bin=$tmp_dir/bin
deploy_path=$tmp_dir/app
docker_log=$tmp_dir/docker.log
mkdir -p "$fake_bin" "$deploy_path/.deploy"
printf '%s\n' 'current/image:1' > "$deploy_path/.deploy/current-image"
printf '%s\n' 'previous/image:1' > "$deploy_path/.deploy/previous-image"

cat > "$fake_bin/docker" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_DOCKER_LOG"
case "$1" in
  ps) printf '%s\n' container-1 ;;
  inspect)
    case "$*" in
      *"{{.Image}}"*) printf '%s\n' sha256:current ;;
      *"sub2api-retain:current"*) printf '%s\n' sha256:current ;;
      *"sub2api-retain:previous"*) printf '%s\n' sha256:previous ;;
      *"current/image:1"*) printf '%s\n' sha256:current ;;
      *"previous/image:1"*) printf '%s\n' sha256:previous ;;
      *) exit 1 ;;
    esac
    ;;
  image)
    case "$*" in
      *"image inspect sub2api-retain:current"*|*"image inspect current/image:1"*) printf '%s\n' sha256:current ;;
      *"image inspect sub2api-retain:previous"*|*"image inspect previous/image:1"*) printf '%s\n' sha256:previous ;;
      *"image ls"*)
        printf '%s\n' 'ghcr.io/test/sub2api sha256:current'
        printf '%s\n' 'ghcr.io/test/sub2api sha256:previous'
        printf '%s\n' 'ghcr.io/test/sub2api sha256:old'
        ;;
      *"image rm"*) ;;
      *) ;;
    esac
    ;;
  builder) ;;
  *) ;;
esac
EOF
chmod +x "$fake_bin/docker"

PATH="$fake_bin:$PATH" \
FAKE_DOCKER_LOG="$docker_log" \
SUB2API_DEPLOY_PATH="$deploy_path" \
SUB2API_OPS_CONFIG="$tmp_dir/missing.env" \
SUB2API_RELEASE_IMAGE_PREFIX=ghcr.io/test/sub2api \
SUB2API_ALERT_COMMAND="$tmp_dir/missing-alert" \
"$script"

if grep -Fq 'image rm sha256:current' "$docker_log" ||
  grep -Fq 'image rm sha256:previous' "$docker_log"; then
  printf '%s\n' 'housekeeping attempted to remove a protected image' >&2
  exit 1
fi
grep -Fq 'image rm sha256:old' "$docker_log"
grep -Fq -- '--no-trunc --format' "$docker_log"

printf '%s\n' 'sub2api housekeeping test passed'
