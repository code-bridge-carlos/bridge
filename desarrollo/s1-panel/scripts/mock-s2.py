#!/usr/bin/env python3
"""Mock de s2 para desarrollo (Fase 4-6).

Simula el orquestador OpenCode:
- POST /api/tasks            : recibe una tarea desde s1 (B3)
- POST /api/notify           : recibe el fin de una subtarea desde s1 (B4)
- GET  /emitir?task_id=...   : SIMULA a s2 partiendo el plan en subtareas y
                               enviándoselas a s1 (verificación B4)
- GET  /health               : healthcheck

Uso:  python3 mock-s2.py [puerto=9001]
"""
import json
import sys
import urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 9001
S1 = "http://localhost:8080"  # s1 dev
PENDING = []                  # últimas tareas recibidas para emitir subtareas

def post(url, payload):
    req = urllib.request.Request(url, data=json.dumps(payload).encode(),
                                 headers={"Content-Type": "application/json",
                                          "X-Control-Token": "dev-token-b4"})
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            return resp.status, resp.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()
    except Exception as e:
        return 0, str(e)


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):  # silenciar logs de acceso
        pass

    def _json(self, obj, code=200):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        raw = self.rfile.read(length) if length else b"{}"
        try:
            data = json.loads(raw)
        except Exception:
            data = {}

        if self.path == "/api/tasks":
            # s1 nos envía una tarea (B3)
            print(f"[mock-s2] POST /api/tasks -> {data}", flush=True)
            PENDING.append(data)
            self._json({"ok": True, "task_id": data.get("task_id", ""),
                        "recibida_en": "mock-s2"})
        elif self.path == "/api/notify":
            # s1 nos avisa del fin de una subtarea (B4)
            print(f"[mock-s2] POST /api/notify -> {data}", flush=True)
            self._json({"ok": True})
        else:
            self._json({"ok": False, "error": "ruta desconocida"}, 404)

    def do_GET(self):
        if self.path.startswith("/emitir"):
            # simula el plan dividido: s2 envía 3 subtareas a s1
            from urllib.parse import parse_qs, urlparse
            qs = parse_qs(urlparse(self.path).query)
            task_id = qs.get("task_id", [""])[0]
            if not PENDING and not task_id:
                self._json({"ok": False, "error": "no hay tareas pendientes"}, 400)
                return
            if not task_id:
                task_id = PENDING[-1].get("task_id", "")
            subs = [
                {"file_path": "src/core/valorar.go", "prompt": "Analiza 'valorar.go' de tarea %s: revisa funciones exportadas." % task_id[:8], "contexto": "solo lectura"},
                {"file_path": "src/core/decidir.go", "prompt": "Analiza 'decidir.go' de tarea %s: identifica puntos de fallo." % task_id[:8], "contexto": "solo lectura"},
                {"file_path": "src/core/ejecutar.go", "prompt": "Analiza 'ejecutar.go' de tarea %s: resume flujo principal." % task_id[:8], "contexto": "solo lectura"},
            ]
            code, body = post(S1 + "/api/subtareas",
                              {"task_id": task_id, "subtareas": subs})
            print(f"[mock-s2] emitir {len(subs)} subtareas de {task_id[:8]} -> s1 ({code}) {body[:200]}", flush=True)
            self._json({"ok": True, "emitidas": len(subs), "s1_resp": body,
                        "code_s1": code})
        elif self.path == "/health":
            self._json({"ok": True, "servicio": "mock-s2", "puerto": PORT})
        else:
            self._json({"ok": False, "error": "ruta desconocida"}, 404)


if __name__ == "__main__":
    print(f"[mock-s2] escuchando en :{PORT}", flush=True)
    HTTPServer(("0.0.0.0", PORT), Handler).serve_forever()