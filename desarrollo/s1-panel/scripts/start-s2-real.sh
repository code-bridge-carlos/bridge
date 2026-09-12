#!/usr/bin/env bash
# Dev: apunta s1 al ORQUESTADOR REAL (s2-carlos-code :9002) en lugar del mock.
# Uso: bash start-s2-real.sh        -> s1 ↔ s2 real (requiere s2 arrancado con start-s2.sh)
#      bash start-s2-real.sh stop   -> vuelve al dev default (mock-s2 :9001, start-dev.sh)
set -u
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [ "${1:-}" = "stop" ]; then
  # Detener s1 y mock, y reiniciar el default
  cd "$DIR" && bash scripts/start-dev.sh
  exit 0
fi

# Solo matar s1 (dentro de script: argv limpio, sin auto-match)
pkill -f "/tmp/opencode/s1" 2>/dev/null
pkill -f "mock-s2.py" 2>/dev/null
sleep 1

cd "$DIR" || exit 1
setsid nohup env \
  DATABASE_URL="postgres://postgres:test@localhost:5433/testdb?sslmode=disable" \
  PANEL_USER=admin \
  PANEL_PASSWORD=admin123 \
  SESSION_SECRET=miSecretoSuficientementeLargo123 \
  S2_URL="http://localhost:9002/api/tasks" \
  S2_NOTIFY_URL="http://localhost:9002/api/notify" \
  CONTROL_TOKEN="dev-token-b4" \
  SERVICES_BASE_URL="http://localhost:8080" \
  PROXY_MAX_IMPORT=12 \
  PROXY_CHECK_BATCH=4 \
  /tmp/opencode/s1 -port 8080 </dev/null >/tmp/opencode/s1.log 2>&1 &
sleep 2
if curl -s -o /dev/null --max-time 3 http://localhost:8080/health; then
  echo "s1 UP → apuntando a s2 REAL (:9002) [mock detenido]"
else
  echo "s1 FALLO" && cat /tmp/opencode/s1.log
fi