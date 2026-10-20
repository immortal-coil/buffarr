#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# Site-specific values (registry, target host, user) live in deploy.env, which
# is gitignored so the repo carries no host/user details. Copy
# deploy.env.example to deploy.env and fill it in.
if [[ -f "$SCRIPT_DIR/deploy.env" ]]; then
  # shellcheck source=/dev/null
  source "$SCRIPT_DIR/deploy.env"
fi
: "${REGISTRY:?set REGISTRY in deploy.env}"
: "${REGISTRY_USER:?set REGISTRY_USER in deploy.env}"
: "${TARGET:?set TARGET (ssh destination, user@host) in deploy.env}"
: "${COMPOSE_DIR:?set COMPOSE_DIR in deploy.env}"
TOKEN_FILE="${TOKEN_FILE:-$HOME/.secrets/registry-token}"

if [[ ! -f "$TOKEN_FILE" ]]; then
  echo "error: token not found at $TOKEN_FILE" >&2
  exit 1
fi

echo "==> Logging in to $REGISTRY"
# --password-stdin keeps the token out of the process list.
sudo podman login "$REGISTRY" \
  --username "$REGISTRY_USER" \
  --password-stdin \
  --tls-verify=false <"$TOKEN_FILE"

# Build stamp for the About page: git describe (short SHA, tags, -dirty) + date.
VERSION="$(git -C "$SCRIPT_DIR" describe --tags --always --dirty 2>/dev/null || echo unknown) ($(date -u +%Y-%m-%d))"
echo "==> Building images (backend build $VERSION)"
# --network=host: podman's default build networking (pasta) needs a tap
# device, which an unprivileged LXC build host can't create.
sudo podman build --network=host -t "$REGISTRY/buffarr-nginx:latest"   "$SCRIPT_DIR/docker/nginx"
sudo podman build --network=host -t "$REGISTRY/buffarr-dnsmasq:latest" "$SCRIPT_DIR/docker/dnsmasq"
sudo podman build --network=host --build-arg VERSION="$VERSION" -t "$REGISTRY/buffarr-backend:latest" -f "$SCRIPT_DIR/docker/backend/Dockerfile" "$SCRIPT_DIR"
sudo podman build --network=host -t "$REGISTRY/buffarr-prefill:latest" "$SCRIPT_DIR/docker/prefill"

echo "==> Pushing images"
sudo podman push --tls-verify=false "$REGISTRY/buffarr-nginx:latest"
sudo podman push --tls-verify=false "$REGISTRY/buffarr-dnsmasq:latest"
sudo podman push --tls-verify=false "$REGISTRY/buffarr-backend:latest"
sudo podman push --tls-verify=false "$REGISTRY/buffarr-prefill:latest"

echo "==> Deploying on $TARGET"
ssh "$TARGET" "cd $COMPOSE_DIR && sudo docker compose pull && sudo docker compose up -d"

echo "==> Done"
