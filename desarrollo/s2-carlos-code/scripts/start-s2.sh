#!/usr/bin/env bash
# s2-carlos-code (orquestador real): arranque/parada dev.
# Uso: bash start-s2.sh            -> arranca en :9002 (S2_EMIT_ON_TASK=1 para bucle de pruebas)
#      bash start-s2.sh stop       -> detiene
#      S2_EMIT_ON_TASK=0 S2_MOCK_PLAN=1 bash start-s2.sh -> FLUJO REAL de planificación con plan mock (C2, sin LLM)
#      S2_EMIT_ON_TASK=0 bash start-s2.sh  -> producción-like (planificación real vía OpenCode)
set -u
BUN="${BUN:-$HOME/.bun/bin/bun}"
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PID_FILE=/tmp/opencode/s2.pid
LOG=/tmp/opencode/s2.log

stop_s2() {
  if [ -f "$PID_FILE" ]; then
    kill "$(cat "$PID_FILE")" 2>/dev/null && echo "s2 detenido (pid $(cat "$PID_FILE"))"
    rm -f "$PID_FILE"
  else
    echo "s2 no estaba corriendo"
  fi
  exit 0
}

[ "${1:-}" = "stop" ] && stop_s2

export S2_EMIT_ON_TASK="${S2_EMIT_ON_TASK:-1}"
export CONTROL_TOKEN="${CONTROL_TOKEN:-dev-token-b4}"
export DATABASE_URL="${DATABASE_URL:-postgres://postgres:test@localhost:5433/testdb?sslmode=disable}"
export S1_URL="${S1_URL:-http://localhost:8080}"

mkdir -p /tmp/opencode
setsid "$BUN" "$DIR/src/index.ts" >"$LOG" 2>&1 &
echo $! > "$PID_FILE"
sleep 1
if kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
  echo "s2 arrancado: pid $(cat "$PID_FILE") · log $LOG · http://localhost:9002/health"
else
  echo "s2 falló al arrancar — log:"
  tail -20 "$LOG"
  rm -f "$PID_FILE"
  exit 1
fi