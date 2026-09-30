#!/usr/bin/env bash
set -euo pipefail

# Arguments are supplied by CI; the server's .env is never transferred or printed.
deploy_root=${1:?usage: release.sh DEPLOY_ROOT IMAGE}
release_image=${2:?usage: release.sh DEPLOY_ROOT IMAGE}
if [[ ! "$release_image" =~ ^ghcr\.io/kenniele/imap_mcp:[a-f0-9]{40}$ ]]; then
  echo 'Expected an immutable imap_mcp image tagged with a full commit SHA' >&2
  exit 1
fi
deploy_root=$(cd "$deploy_root" && pwd)
release_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
test -f "$deploy_root/.env"
exec 9>"$deploy_root/.deploy.lock"
flock -w 120 9

export MCP_IMAGE="$release_image"
compose=(docker compose --project-name imap_mcp --project-directory "$deploy_root"
  --env-file "$deploy_root/.env" -f "$release_dir/docker-compose.prod.yml")
"${compose[@]}" config --quiet
"${compose[@]}" pull mail-mcp postgres
"${compose[@]}" run --rm --no-deps -T mail-mcp check-config </dev/null
"${compose[@]}" up -d --wait --wait-timeout 60 postgres
# Close stdin so a migration container cannot consume the SSH command stream.
"${compose[@]}" run --rm --no-deps -T mail-mcp migrate </dev/null
"${compose[@]}" up -d --no-deps --no-build mail-mcp

# Get the configured port without sourcing .env as shell code or printing secrets.
host_port=$("${compose[@]}" config --format json | python3 -c \
  'import json,sys; print(json.load(sys.stdin)["services"]["mail-mcp"]["ports"][0]["published"])')
if ! curl -fsS --retry 15 --retry-connrefused --retry-delay 2 --max-time 5 \
  "http://127.0.0.1:$host_port/ready" >/dev/null; then
  "${compose[@]}" ps -a
  "${compose[@]}" logs --tail=20 mail-mcp
  echo 'MCP readiness failed' >&2
  exit 1
fi

public_url=$("${compose[@]}" config --format json | python3 -c \
  'import json,sys; print(json.load(sys.stdin)["services"]["mail-mcp"]["environment"]["MCP_PUBLIC_URL"])')
issuer=${public_url%/mcp}
curl -fsS --proto '=https' --retry 5 --retry-delay 2 --max-time 15 \
  "$issuer/.well-known/oauth-authorization-server" | python3 -c \
  'import json,sys; m=json.load(sys.stdin); assert m["issuer"] == sys.argv[1] and "S256" in m["code_challenge_methods_supported"]' "$issuer"
curl -fsS --proto '=https' --max-time 15 \
  "$issuer/.well-known/oauth-protected-resource/mcp" | python3 -c \
  'import json,sys; m=json.load(sys.stdin); assert m["resource"] == sys.argv[1] and "mail.read" in m["scopes_supported"]' "$public_url"
status=$(curl -sS --proto '=https' --max-time 15 -o /dev/null -w '%{http_code}' "$public_url")
if [[ "$status" != 401 ]]; then
  echo "Expected public MCP to require OAuth (401), got $status" >&2
  exit 1
fi

# Only update the current release after readiness succeeds. Keep prior bundles for rollback.
ln -sfn "$release_dir" "$deploy_root/deploy/current"
echo "MCP deployment ready: $release_image"
