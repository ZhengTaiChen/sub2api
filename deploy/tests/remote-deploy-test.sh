#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
script=$repo_root/deploy/remote-deploy.sh

sh -n "$script"
if sh "$script" --image ghcr.io/example/sub2api:latest --digest invalid >/dev/null 2>&1; then
  printf 'remote deploy script accepted an invalid digest\n' >&2
  exit 1
fi
grep -Fq 'docker pull "$IMAGE_REF"' "$script"
grep -Fq 'docker compose -f "$COMPOSE_FILE" up -d --no-deps sub2api' "$script"
grep -Fq 'rollback()' "$script"
grep -Fq 'org.opencontainers.image.revision' "$script"
grep -Fq 'docker-compose.yml.before-' "$script"

tmp_dir=$(mktemp -d)
trap 'rm -rf "$tmp_dir"' EXIT INT TERM
fake_bin=$tmp_dir/bin
mkdir -p "$fake_bin" "$tmp_dir/app/.deploy"
cat > "$tmp_dir/app/docker-compose.yml" <<'EOF'
services:
  sub2api:
    image: old/image:stable
    ports:
      - "18080:8080"
EOF
cat > "$fake_bin/docker" <<'EOF'
#!/bin/sh
mode=${FAKE_DOCKER_MODE:-success}
case "$1" in
  compose) [ "${2:-}" = version ] && { echo 'Docker Compose version v2'; exit 0; }; [ "$mode" = compose-fail ] && exit 1; exit 0 ;;
  pull) [ "$mode" = no-pull ] && exit 1; exit 0 ;;
  image) if [ "$mode" = no-pull ] && case "$*" in *@sha256:*) true ;; *) false ;; esac; then exit 1; fi; case "$*" in *inspect*) case "$*" in *Architecture*) echo amd64 ;; *revision*) echo commit-test ;; esac ;; esac; exit 0 ;;
  inspect) case "$*" in *Config.Image*) echo old/image:stable ;; *Health.Status*) [ "$mode" = unhealthy ] && echo unhealthy || echo healthy ;; *'{{.Image}}'*) echo old-image-id ;; *'{{.Id}}'*) echo new-container-id ;; esac; exit 0 ;;
  port) echo '0.0.0.0:18080' ;;
  logs) exit 0 ;;
  *) exit 0 ;;
esac
EOF
cat > "$fake_bin/curl" <<'EOF'
#!/bin/sh
[ "${FAKE_DOCKER_MODE:-success}" != unhealthy ]
EOF
chmod +x "$fake_bin/docker" "$fake_bin/curl"

PATH="$fake_bin:$PATH" FAKE_DOCKER_MODE=success "$script" --image example/sub2api:1.0 --digest sha256:0000000000000000000000000000000000000000000000000000000000000000 --commit commit-test --deploy-path "$tmp_dir/app" >/dev/null
grep -Fq 'example/sub2api:1.0@sha256:' "$tmp_dir/app/docker-compose.yml"
test ! -e "$tmp_dir/app/.deploy/compose.deploy.yml"
cp "$tmp_dir/app/docker-compose.yml" "$tmp_dir/app/docker-compose.yml.success"

if PATH="$fake_bin:$PATH" FAKE_DOCKER_MODE=unhealthy "$script" --image example/sub2api:2.0 --digest sha256:1111111111111111111111111111111111111111111111111111111111111111 --commit commit-test --deploy-path "$tmp_dir/app" >/dev/null 2>&1; then
  printf 'remote deploy script did not roll back unhealthy image\n' >&2
  exit 1
fi
cmp "$tmp_dir/app/docker-compose.yml" "$tmp_dir/app/docker-compose.yml.success"

PATH="$fake_bin:$PATH" FAKE_DOCKER_MODE=no-pull "$script" --image example/sub2api:1.0 --digest sha256:0000000000000000000000000000000000000000000000000000000000000000 --commit commit-test --skip-pull --deploy-path "$tmp_dir/app" >/dev/null
grep -Fq 'image: example/sub2api:1.0' "$tmp_dir/app/docker-compose.yml"
grep -Fq 'sha256:0000000000000000000000000000000000000000000000000000000000000000' "$tmp_dir/app/.deploy/current-digest"

printf 'remote deploy script test passed\n'
