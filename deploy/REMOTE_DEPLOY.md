# Remote Image Deployment

`remote-deploy.sh` is the production deployment entrypoint used by GitHub Actions. It never builds source code on the target host.

## Server prerequisites

- Docker Engine and `docker compose` are installed.
- The deployment user can run Docker and can write to the Compose directory.
- The server can pull the selected image from GHCR or Docker Hub. Configure a read-only registry login on the server when the image is private.
- The existing Compose project remains at `/opt/sub2api` unless another path is passed.

For a private GHCR image, authenticate once as the deployment user and keep the
token read-only (the token needs package read access):

```sh
printf '%s' "$GHCR_READ_TOKEN" | docker login ghcr.io -u GHCR_USER --password-stdin
```

Configure these GitHub `production` Environment secrets before enabling the
deployment job: `DEPLOY_SSH_KEY`, `DEPLOY_HOST`, `DEPLOY_USER`, and optionally
`DEPLOY_PATH`. Set repository variable `AUTO_DEPLOY_PRODUCTION=true` only when
tag pushes should deploy automatically; manual dispatch remains available with
the approval rules of the `production` Environment.

## Manual deployment

Pass an immutable image reference and digest:

```sh
deploy/remote-deploy.sh \
  --image ghcr.io/wei-shaw/sub2api:0.1.180 \
  --digest sha256:<64-hex-digest> \
  --commit <git-commit> \
  --deploy-path /opt/sub2api
```

The script pulls and validates the image, starts it through a temporary Compose override, waits for Docker health and `/health`, and only then persists the digest-pinned image in the production Compose file. Failure restores the previous Compose configuration and starts the previous container.

Deployment state and logs are stored under `/opt/sub2api/.deploy`. The newest Compose backup and previous image reference are retained for rollback.
