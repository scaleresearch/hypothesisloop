#!/usr/bin/env bash
# Lifecycle for the git:// daemon that serves experiment code repos (agents/coordinator/experiments/*'s
# per-experiment bare repos) to experimentator agent containers over the git protocol.
#
# Previously this was an ad-hoc `git daemon ...` command re-invented by whoever was
# coordinating a run, with two sharp edges discovered the hard way:
#   1. `--export-all` alone does NOT enable push (`receive-pack`) -- every repo silently
#      rejects `git push` unless `--enable=receive-pack` is passed.
#   2. `--export-all` does NOT retroactively export a bare repo created after the daemon
#      process started -- a freshly-created experiment repo is unfetchable until the daemon is
#      restarted, not just left running.
# This script always passes --enable=receive-pack and always restarts (kills any prior
# instance) on `start`, so point 2 can never bite silently again.
set -euo pipefail

ACTION="${1:?usage: git-daemon.sh start|stop|status|destroy}"
BASE_PATH="${GIT_DAEMON_BASE_PATH:-/home/ttuser/git-repos}"
PORT="${GIT_DAEMON_PORT:-9418}"
PID_FILE="$BASE_PATH/git-daemon.pid"
LOG_FILE="$BASE_PATH/logs/git-daemon.log"

stop() {
  local stopped=0
  if [[ -f "$PID_FILE" ]] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
    kill "$(cat "$PID_FILE")"
    stopped=1
  fi
  rm -f "$PID_FILE"

  # A daemon started by an earlier session (or whose pid file was removed/went stale) is
  # invisible to the check above -- fall back to matching the process directly so `up` never
  # fails with the port already bound by an orphaned daemon.
  if pkill -f "git daemon --base-path=$BASE_PATH" 2>/dev/null; then
    stopped=1
  fi

  if [[ "$stopped" -eq 1 ]]; then
    echo "git-daemon: stopped"
  else
    echo "git-daemon: not running"
  fi
}

start() {
  stop
  if (echo >"/dev/tcp/127.0.0.1/$PORT") 2>/dev/null; then
    echo "git-daemon: port $PORT still in use after stop, aborting" >&2
    exit 1
  fi
  mkdir -p "$BASE_PATH" "$(dirname "$LOG_FILE")"
  git daemon \
    --base-path="$BASE_PATH" \
    --export-all \
    --enable=receive-pack \
    --reuseaddr \
    --detach \
    --pid-file="$PID_FILE" \
    --listen=0.0.0.0 \
    --port="$PORT" \
    >>"$LOG_FILE" 2>&1
  sleep 1
  if [[ -f "$PID_FILE" ]] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
    echo "git-daemon: started on port $PORT, base-path=$BASE_PATH, pid=$(cat "$PID_FILE")"
  else
    echo "git-daemon: failed to start, see $LOG_FILE" >&2
    exit 1
  fi
}

status() {
  if [[ -f "$PID_FILE" ]] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
    echo "git-daemon: running, pid=$(cat "$PID_FILE"), base-path=$BASE_PATH, port=$PORT"
  else
    echo "git-daemon: not running"
  fi
}

# Stops the daemon (if running) and deletes BASE_PATH entirely -- every bare repo, workspace,
# and log it holds. Irreversible; only use when you want a clean slate (e.g. between unrelated
# runs), not for a routine restart -- that's `stop` followed by `start`.
destroy() {
  stop
  rm -rf "$BASE_PATH"
  echo "git-daemon: destroyed $BASE_PATH"
}

case "$ACTION" in
  start) start ;;
  stop) stop ;;
  status) status ;;
  destroy) destroy ;;
  *) echo "usage: git-daemon.sh start|stop|status|destroy" >&2; exit 1 ;;
esac
