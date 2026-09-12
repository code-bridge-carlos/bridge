#!/usr/bin/env bash
# Mock workers para la cola de B4 (long-poll timeout).
# Uso: start-workers.sh w6 [w8] [w10] ...  (por defecto: w6 w7 w8)
#      start-workers.sh stop                  (solo detener)
pkill -f "mock-worker.py" 2>/dev/null
sleep 1
if [ "$1" = "stop" ]; then echo "workers detenidos"; exit 0; fi
cd /workspaces/ok/desarrollo/s1-panel || exit 1
workers=("$@")
if [ ${#workers[@]} -eq 0 ]; then workers=(w6 w7 w8); fi
for w in "${workers[@]}"; do
  setsid nohup python3 scripts/mock-worker.py "$w" </dev/null >"/tmp/opencode/worker-$w.log" 2>&1 &
done
sleep 1
echo "workers lanzados: ${workers[*]} (logs en /tmp/opencode/worker-w*.log)"