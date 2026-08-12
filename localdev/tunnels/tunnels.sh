#!/usr/bin/env bash
# Lifecycle for Cloudflare quick tunnels that expose local controlplane services to
# external nodes without a domain/account (https://<random>.trycloudflare.com per service).
#
# These are anonymous "quick tunnels": Cloudflare does not guarantee uptime for them and
# the URL is re-randomized every time a tunnel is (re)started -- fine for dev/testing,
# not for anything that needs a stable address. For that, switch to a named tunnel
# (requires a domain added to a free Cloudflare account) and a cloudflared config.yml.
#
# Services to expose are read from tunnels.conf (name + local port, one per line).
set -euo pipefail

ACTION="${1:?usage: tunnels.sh up|down|status}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONF_FILE="${TUNNELS_CONF:-$SCRIPT_DIR/tunnels.conf}"
BASE_PATH="${TUNNELS_BASE_PATH:-$HOME/.hypothesisloop-tunnels}"
PID_DIR="$BASE_PATH/pids"
LOG_DIR="$BASE_PATH/logs"

require_cloudflared() {
  if ! command -v cloudflared >/dev/null 2>&1; then
    echo "tunnels: cloudflared not found on PATH -- install it first" >&2
    exit 1
  fi
}

read_services() {
  grep -Ev '^\s*(#|$)' "$CONF_FILE"
}

down() {
  local stopped=0
  if [[ -d "$PID_DIR" ]]; then
    for pid_file in "$PID_DIR"/*.pid; do
      [[ -e "$pid_file" ]] || continue
      local name pid
      name="$(basename "$pid_file" .pid)"
      pid="$(cat "$pid_file")"
      if kill -0 "$pid" 2>/dev/null; then
        kill "$pid"
        for _ in $(seq 1 20); do
          kill -0 "$pid" 2>/dev/null || break
          sleep 0.2
        done
        kill -9 "$pid" 2>/dev/null || true
        stopped=1
        echo "tunnels: stopped $name (pid=$pid)"
      fi
      rm -f "$pid_file"
    done
  fi
  if [[ "$stopped" -eq 0 ]]; then
    echo "tunnels: none running"
  fi
}

up() {
  require_cloudflared
  [[ -f "$CONF_FILE" ]] || { echo "tunnels: no config at $CONF_FILE" >&2; exit 1; }
  down
  mkdir -p "$PID_DIR" "$LOG_DIR"

  while read -r name port; do
    local log_file="$LOG_DIR/$name.log"
    : >"$log_file"
    nohup cloudflared tunnel --url "http://localhost:$port" >"$log_file" 2>&1 &
    echo $! >"$PID_DIR/$name.pid"
  done < <(read_services)

  echo "tunnels: waiting for URLs..."
  sleep 8
  status
}

status() {
  [[ -d "$PID_DIR" ]] || { echo "tunnels: not running"; return; }
  local any=0
  while read -r name port; do
    local pid_file="$PID_DIR/$name.pid"
    local log_file="$LOG_DIR/$name.log"
    if [[ -f "$pid_file" ]] && kill -0 "$(cat "$pid_file")" 2>/dev/null; then
      any=1
      local url
      url="$(grep -oE 'https://[a-zA-Z0-9.-]+\.trycloudflare\.com' "$log_file" 2>/dev/null | head -1 || true)"
      echo "  $name (:$port) -> ${url:-<not ready yet, check $log_file>}"
    else
      echo "  $name (:$port) -> not running"
    fi
  done < <(read_services)
  [[ "$any" -eq 1 ]] || echo "tunnels: none running"
}

case "$ACTION" in
  up) up ;;
  down) down ;;
  status) status ;;
  *) echo "usage: tunnels.sh up|down|status" >&2; exit 1 ;;
esac
