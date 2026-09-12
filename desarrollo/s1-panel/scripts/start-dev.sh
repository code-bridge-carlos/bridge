#!/usr/bin/env bash
# Arranque de s1 + mock-s2 en dev (fuera del proceso del tool para sobrevivir timeouts)
pkill -f "opencode/s1" 2>/dev/null
pkill -f "mock-s2.py" 2>/dev/null
sleep 1
cd /workspaces/ok/desarrollo/s1-panel || exit 1
setsid nohup env \
  DATABASE_URL="postgres://postgres:test@localhost:5433/testdb?sslmode=disable" \
  PANEL_USER=admin \
  PANEL_PASSWORD=admin123 \
  SESSION_SECRET=miSecretoSuficientementeLargo123 \
  S2_URL="http://localhost:9001/api/tasks" \
  S2_NOTIFY_URL="http://localhost:9001/api/notify" \
  CONTROL_TOKEN="dev-token-b4" \
  PROXY_MAX_IMPORT=12 \
  PROXY_CHECK_BATCH=4 \
  /tmp/opencode/s1 -port 8080 </dev/null >/tmp/opencode/s1.log 2>&1 &
setsid nohup python3 scripts/mock-s2.py 9001 </dev/null >/tmp/opencode/mock-s2.log 2>&1 &
sleep 2
if curl -s -o /dev/null --max-time 3 http://localhost:8080/health; then
  echo "s1 UP (+ mock-s2 :9001)"
else
  echo "s1 FALLO" && cat /tmp/opencode/s1.log
fi