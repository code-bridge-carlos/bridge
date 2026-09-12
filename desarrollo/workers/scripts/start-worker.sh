#!/usr/bin/env bash
# worker-carlos-code: arranque/parada dev de un worker (w6..w10).
# Uso: bash start-worker.sh w8              -> arranca w8 en :9008
#      bash start-worker.sh w8 stop         -> detiene w8
#      WORKER_MOCK_BUILD=1 bash start-worker.sh w8 -> ciclo real con resultado sintético (sin LLM)
set -u
BUN="${BUN:-$HOME/.bun/bin/bun}"
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WID="${1:-w6}"
PID_FILE="/tmp/opencode/worker-$WID.pid"
LOG="/tmp/opencode/worker-$WID.log"

NUM="${WID//[^0-9]/}"
if [ -z "$NUM" ] || [ "$NUM" -lt 6 ] || [ "$NUM" -gt 10 ]; then
  echo "worker inválido: $WID (w6..w10)"; exit 1
fi
PORT=$((9000 + NUM))

stop_worker() {
  if [ -f "$PID_FILE" ]; then
    kill "$(cat "$PID_FILE")" 2>/dev/null && echo "worker-$WID detenido (pid $(cat "$PID_FILE"))"
    rm -f "$PID_FILE"
  else
    echo "worker-$WID no estaba corriendo"
  fi
  exit 0
}

[ "${2:-}" = "stop" ] && stop_worker

export WORKER_ID="$WID"
export PORT="${PORT:-$PORT}"
export CONTROL_TOKEN="${CONTROL_TOKEN:-dev-token-b4}"
export S1_URL="${S1_URL:-http://localhost:8080}"
export WORKER_MOCK_BUILD="${WORKER_MOCK_BUILD:-1}"

mkdir -p /tmp/opencode
setsid "$BUN" "$DIR/src/index.ts" >"$LOG" 2>&1 &
echo $! > "$PID_FILE"
sleep 1
if kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
  echo "worker-$WID arrancado: pid $(cat "$PID_FILE") · :$PORT · log $LOG"
else
  echo "worker-$WID falló al arrancar — log:"
  tail -20 "$LOG"
  rm -f "$PID_FILE"
  exit 1
fi