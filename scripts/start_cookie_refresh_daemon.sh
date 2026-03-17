#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

ENV_FILE="${ENV_FILE:-$ROOT_DIR/.env}"
MODE="${MODE:-cdp}"
CDP_ENDPOINT="${CDP_ENDPOINT:-}"
CDP_HOST="${CDP_HOST:-127.0.0.1}"
CDP_PORT="${CDP_PORT:-9322}"
MCP_HOST="${MCP_HOST:-localhost}"
MCP_PORT="${MCP_PORT:-0}"
START_TIMEOUT="${START_TIMEOUT:-30}"
LEAD_SECONDS="${LEAD_SECONDS:-3600}"
POLL_SECONDS="${POLL_SECONDS:-300}"
DEBUG="${DEBUG:-0}"

if [[ "$MODE" != "cdp" && "$MODE" != "extension" && "$MODE" != "auto" ]]; then
  echo "[start_cookie_refresh_daemon] invalid MODE=$MODE (expected: auto|cdp|extension)" >&2
  exit 1
fi

args=(
  scripts/refresh_cookie_mcp.py
  --daemon
  --env "$ENV_FILE"
  --mode "$MODE"
  --host "$MCP_HOST"
  --port "$MCP_PORT"
  --start-timeout "$START_TIMEOUT"
  --lead-seconds "$LEAD_SECONDS"
  --poll-seconds "$POLL_SECONDS"
)

if [[ "$MODE" == "extension" ]]; then
  : "${PLAYWRIGHT_MCP_EXTENSION_TOKEN:?PLAYWRIGHT_MCP_EXTENSION_TOKEN is required for extension mode}"
fi

if [[ "$MODE" == "cdp" || "$MODE" == "auto" ]]; then
  if [[ -n "$CDP_ENDPOINT" ]]; then
    args+=(--cdp-endpoint "$CDP_ENDPOINT")
  else
    args+=(--cdp-host "$CDP_HOST" --cdp-port "$CDP_PORT")
  fi
fi

if [[ "$DEBUG" == "1" ]]; then
  args+=(--debug)
fi

echo "[start_cookie_refresh_daemon] env=$ENV_FILE mode=$MODE mcp=${MCP_HOST}:${MCP_PORT}" >&2
if [[ -n "$CDP_ENDPOINT" ]]; then
  echo "[start_cookie_refresh_daemon] cdp-endpoint=$CDP_ENDPOINT" >&2
else
  echo "[start_cookie_refresh_daemon] cdp=${CDP_HOST}:${CDP_PORT}" >&2
fi

exec python3 "${args[@]}"
