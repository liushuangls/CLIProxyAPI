#!/usr/bin/env bash
set -euo pipefail

image="${1:?usage: smoke-self-image.sh IMAGE}"
smoke_dir="$(mktemp -d)"
container="cliproxy-self-smoke-${RANDOM}-${RANDOM}"
cleanup() {
  status=$?
  if [ "$status" -ne 0 ]; then
    docker logs "$container" 2>&1 || true
  fi
  docker rm -f "$container" >/dev/null 2>&1 || true
  rm -rf "$smoke_dir"
  exit "$status"
}
trap cleanup EXIT

cat > "$smoke_dir/config.yaml" <<'YAML'
server:
  host: "0.0.0.0"
  port: 8317
management:
  allow-remote: true
  secret-key: "smoke-test-only"
  disable-auto-update-panel: true
oauth:
  auth-dir: "/root/.cli-proxy-api"
YAML

docker run --detach --name "$container" \
  --publish 127.0.0.1::8317 \
  --volume "$smoke_dir/config.yaml:/CLIProxyAPI/config.yaml" \
  "$image" >/dev/null
address="$(docker port "$container" 8317/tcp)"
base="http://${address}"
ready=0
for attempt in {1..30}; do
  if curl --fail --silent --show-error "$base/management.html" -o "$smoke_dir/panel.html" 2>/dev/null; then
    ready=1
    break
  fi
  sleep 1
done
test "$ready" -eq 1
grep -Fq 'auth_usage' "$smoke_dir/panel.html"

endpoint="$base/v8/management/observability/usage/credentials?auth_index=smoke-missing"
unauthorized="$(curl --silent --show-error -o /dev/null --write-out '%{http_code}' "$endpoint")"
test "$unauthorized" = 401
authorized="$(curl --silent --show-error \
  --header 'Authorization: Bearer smoke-test-only' \
  --output "$smoke_dir/response.json" --write-out '%{http_code}' "$endpoint")"
test "$authorized" = 404
grep -Fq 'credential_not_found' "$smoke_dir/response.json"
docker exec "$container" test -s /root/.cli-proxy-api/usage-history.db
docker exec "$container" sh -c 'test "$MANAGEMENT_PANEL_BUNDLED" = true'
printf 'Container smoke check passed: %s\n' "$image"
