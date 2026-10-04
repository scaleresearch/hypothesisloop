#!/usr/bin/env bash
# Lifecycle for Cloudflare quick tunnels that expose local controlplane services to
# external nodes without a domain/account (https://<random>.trycloudflare.com per service).
#
# These are anonymous "quick tunnels": Cloudflare does not guarantee uptime for them and
# the URL is re-randomized every time a tunnel is (re)started -- fine for dev/testing,
# not for anything that needs a stable address. For that, switch to a named tunnel
# (requires a domain added to a free Cloudflare account) and a cloudflared config.yml.
#
# Services to expose are read from tunnels.conf (name + local port, one per line), and the local
# git server is one more on a stack that runs it.
set -euo pipefail

ACTION="${1:?usage: tunnels.sh up|down|status|addresses}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "$SCRIPT_DIR/../lib/auth.sh"
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

# A stack with no GitHub App keeps its code on the git server make dev runs, which a hosted agent
# and a rented instance clone from like they call the API.
read_services() {
  grep -Ev '^\s*(#|$)' "$CONF_FILE"
  if [[ "$(lib_git_provider)" == "plain" ]]; then
    echo "git ${HL_GIT_PORT}"
  fi
}

# Stops one tunnel's process and forgets its pid; succeeds only when a process was running.
stop() {
  local name="$1" pid_file="$PID_DIR/$1.pid" pid running=1
  [[ -f "$pid_file" ]] || return 1
  pid="$(cat "$pid_file")"
  if kill -0 "$pid" 2>/dev/null; then
    kill "$pid"
    for _ in $(seq 1 20); do
      kill -0 "$pid" 2>/dev/null || break
      sleep 0.2
    done
    kill -9 "$pid" 2>/dev/null || true
    running=0
    echo "tunnels: stopped $name (pid=$pid)"
  fi
  rm -f "$pid_file"
  return "$running"
}

down() {
  local stopped=0
  if [[ -d "$PID_DIR" ]]; then
    for pid_file in "$PID_DIR"/*.pid; do
      [[ -e "$pid_file" ]] || continue
      if stop "$(basename "$pid_file" .pid)"; then
        stopped=1
      fi
    done
  fi
  if [[ "$stopped" -eq 0 ]]; then
    echo "tunnels: none running"
  fi
}

# Starts only the tunnels that are down: restarting one re-randomizes its URL, and a new api URL
# means reinstalling every attached cluster.
up() {
  require_cloudflared
  [[ -f "$CONF_FILE" ]] || { echo "tunnels: no config at $CONF_FILE" >&2; exit 1; }
  mkdir -p "$PID_DIR" "$LOG_DIR"

  while read -r name port; do
    if url_of "$name" >/dev/null; then
      continue
    fi
    stop "$name" || true
    local log_file="$LOG_DIR/$name.log"
    : >"$log_file"
    nohup cloudflared tunnel --url "http://localhost:$port" >"$log_file" 2>&1 &
    echo $! >"$PID_DIR/$name.pid"
  done < <(read_services)

  echo "tunnels: waiting for URLs..."
  for _ in $(seq 1 120); do
    if addresses >/dev/null 2>&1; then
      status
      return
    fi
    sleep 0.5
  done
  status
  echo "tunnels: not every tunnel got a URL within a minute" >&2
  exit 1
}

# The URL of one tunnel the edge has registered, or why it is down and a failure. A quick tunnel's
# process outlives its tunnel (the edge forgets it and every reconnect answers "Tunnel not found"),
# so the only up is cloudflared's own /ready on the metrics server it logs.
url_of() {
  local name="$1" pid_file="$PID_DIR/$1.pid" log_file="$LOG_DIR/$1.log" url metrics
  if [[ ! -f "$pid_file" ]] || ! kill -0 "$(cat "$pid_file")" 2>/dev/null; then
    echo "not running"
    return 1
  fi
  url="$(grep -oE 'https://[a-zA-Z0-9.-]+\.trycloudflare\.com' "$log_file" | head -1 || true)"
  metrics="$(grep -oE 'Starting metrics server on [0-9.:]+' "$log_file" | head -1 | awk '{ print $NF }' || true)"
  if [[ -z "$url" || -z "$metrics" ]]; then
    echo "starting, see $log_file"
    return 1
  fi
  if ! curl -fsS --max-time 2 -o /dev/null "http://$metrics/ready" 2>/dev/null; then
    echo "$url has no registered connection, see $log_file"
    return 1
  fi
  echo "$url"
}

# One line per service, "<name> <url>", read from the running tunnels: what `make dev-hosted`
# renders into the settings. Fails when a tunnel is down, so a caller never takes an address
# nothing answers at.
addresses() {
  local name port url
  while read -r name port; do
    if ! url="$(url_of "$name")"; then
      echo "tunnels: $name is down ($url); run make tunnels" >&2
      return 1
    fi
    echo "$name $url"
  done < <(read_services)
}

status() {
  local name port url
  while read -r name port; do
    if url="$(url_of "$name")"; then
      echo "  $name (:$port) -> $url"
    else
      echo "  $name (:$port) -> down: $url"
    fi
  done < <(read_services)
}

case "$ACTION" in
  up) up ;;
  down) down ;;
  status) status ;;
  addresses) addresses ;;
  *) echo "usage: tunnels.sh up|down|status|addresses" >&2; exit 1 ;;
esac
