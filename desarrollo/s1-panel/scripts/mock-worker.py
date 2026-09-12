#!/usr/bin/env python3
"""Mock de un worker (w6..w10) para desarrollo (Fase 5 + preempción A4).

Ciclo:
1. Long-poll a GET /api/notificaciones/{worker_id} (30s, repetir si 204).
2. Al recibir una subtarea: simula trabajo (~6s) enviando heartbeats a
   POST /api/subtareas/{id}/heartbeat cada 2s. Si un heartbeat responde
   restart=true, la subtarea ya no es suya (preempción): loguea y vuelve a
   poll sin enviar el fin.
3. Envía el resultado al POST /api/subtareas/{id}/fin.
4. Repite.

Uso: python3 mock-worker.py w6 [s1=localhost:8080]
"""
import json
import sys
import time
import urllib.request

WORKER = sys.argv[1] if len(sys.argv) > 1 else "w6"
S1 = f"http://{'localhost:8080' if len(sys.argv) < 3 else sys.argv[2]}"
TOKEN = "dev-token-b4"  # CONTROL_TOKEN de dev (configurado en start-dev.sh)


def request(method, url, payload=None, timeout=35):
    data = json.dumps(payload).encode() if payload is not None else None
    req = urllib.request.Request(url, data=data, method=method,
                                 headers={"Content-Type": "application/json",
                                          "X-Control-Token": TOKEN})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read()
            return resp.status, (json.loads(raw) if raw else None)
    except urllib.error.HTTPError as e:
        body = e.read() if e.read() else b""
        try:
            return e.code, json.loads(body)
        except Exception:
            return e.code, {"raw": body.decode(errors="replace")}
    except Exception as e:
        return 0, {"error": str(e)}


print(f"[{WORKER}] iniciando (s1={S1})", flush=True)
while True:
    code, body = request("GET", f"{S1}/api/notificaciones/{WORKER}")
    if code == 200 and body and body.get("id"):
        sub = body
        print(f"[{WORKER}] recibi subtarea {sub['id'][:8]} de task {sub['task_id'][:8]} "
              f"-> {sub['file_path']}", flush=True)
        # informar que empezó a trabajar (asignada → en_progreso)
        p_code, _ = request("POST", f"{S1}/api/subtareas/{sub['id']}/progreso")
        if p_code != 200:
            print(f"[{WORKER}] aviso en_progreso -> s1 ({p_code})", flush=True)
        # trabajo simulado con heartbeats (el real tardaría minutos)
        reasignada = False
        for step in range(3):
            time.sleep(2)
            h_code, h_body = request("POST", f"{S1}/api/subtareas/{sub['id']}/heartbeat")
            if h_code == 200 and h_body and h_body.get("restart") is True:
                print(f"[{WORKER}] heartbeat: subtarea {sub['id'][:8]} reasignada "
                      f"por s1 (preempción) → reinicio OpenCode", flush=True)
                reasignada = True
                break
        if reasignada:
            continue  # no enviar fin: ya no es suya
        fin_code, fin_body = request(
            "POST", f"{S1}/api/subtareas/{sub['id']}/fin",
            {"estado": "completada",
             "resultado": {"exit": 0, "worker": WORKER, "files": [sub["file_path"]],
                           "summary": "analisis ok (mock)"}})
        print(f"[{WORKER}] fin subtarea {sub['id'][:8]} -> s1 ({fin_code})", flush=True)
    elif code == 204:
        print(f"[{WORKER}] long-poll sin novedad, repito", flush=True)
    else:
        print(f"[{WORKER}] long-poll raro: {code} {body}", flush=True)
        time.sleep(3)