// Servidor HTTP del worker: /health (keep-alive de s1), /estado y /tui (E1).
import { config } from "./config";
import { visual } from "./context";
import { info } from "./logger";
import { validarTokenTUI, paginaTUI } from "./tui";
import { isPreflight, preflightResponse, withCORS } from "./cors";

export function buildServer() {
  return Bun.serve({
    port: config.port,
    // /simular hace long-poll a s1 (hasta ~30s): sin idleTimeout Bun corta a 10s.
    idleTimeout: 0,
    async fetch(req) {
      // OPTIONS preflight — CORS controlado por allowlist (F1)
      if (isPreflight(req)) return preflightResponse(req.headers.get("origin"));
      const res = await dispatch(req);
      return withCORS(res, req.headers.get("origin"));
    },
  });
}

async function dispatch(req: Request): Promise<Response> {
  const url = new URL(req.url);
  const path = url.pathname;

      if (req.method === "GET" && path === "/health") {
        return Response.json({
          ok: true,
          servicio: `worker-${config.workerId}`,
          estado: "libre",
          ventana: visual.stats(),
          ram_mb: Math.round(process.memoryUsage().rss / 1024 / 1024),
        });
      }

      if (req.method === "GET" && path === "/estado") {
        return Response.json({
          worker: config.workerId,
          agente: config.agentMode,
          ventana: visual.stats(),
          ultima_actividad: visual.lastActivity(),
        });
      }

      // TUI del worker (E1): requiere token temporal de s1.
      if (req.method === "GET" && path === "/tui") {
        const user = await validarTokenTUI(req, config.workerId);
        if (user === "") {
          return Response.redirect(`${config.s1Url}/login`, 302);
        }
        return new Response(paginaTUI({
          servicio: config.workerId,
          titulo: `Worker ${config.workerId.toUpperCase()} · Build`,
          secciones: `
  <div class="card"><h2>Estado</h2><pre id="estado">(cargando...)</pre></div>
  <div class="card"><h2>Ventana visual (50k)</h2><pre id="ventana">(cargando...)</pre></div>
  <div class="card"><h2>Acciones</h2>
    <div class="row"><button onclick="simularTrabajo()">Forzar 1 simulación (mock)</button></div>
    <pre id="sim"></pre>
  </div>`,
          js: `
async function cargarEstado(){
  try{
    const h=await json('/health');
    const st=await json('/estado');
    set('estado', JSON.stringify({health:h, estado:st}, null, 2));
    set('ventana', JSON.stringify(st.ventana, null, 2));
  }catch(e){set('estado','ERROR: '+e.message)}
  document.getElementById('log').textContent='TUI cargado · token validado vs s1';
}
async function simularTrabajo(){
  try{
    const r=await fetch('/simular', {method:'POST'});
    const d=await r.clone().json().catch(()=>({ok:false, texto:(await r.text()).slice(0,200)}));
    set('sim', JSON.stringify(d,null,2));
    cargarEstado();
  }catch(e){set('sim','ERROR: '+e.message)}
}
setInterval(cargarEstado, 2500); cargarEstado();`}),
        { headers: { "Content-Type": "text/html; charset=utf-8" } });
      }

      // POST /simular — solo dev/mock: ejecuta una subtarea pendiente de s1.
      if (req.method === "POST" && path === "/simular") {
        try {
          const res = await fetch(
            `${config.s1Url}/api/notificaciones/${config.workerId}?token=${encodeURIComponent(config.controlToken)}`,
            { signal: AbortSignal.timeout(30_000) },
          );
          if (res.status === 204) {
            return Response.json({ ok: true, simulada: false, motivo: "sin subtareas pendientes" });
          }
          const sub = (await res.json()) as { id: string; task_id: string; file_path: string; prompt: string };
          const fin = await fetch(`${config.s1Url}/api/subtareas/${sub.id}/fin`, {
            method: "POST",
            headers: { "Content-Type": "application/json", "X-Control-Token": config.controlToken },
            body: JSON.stringify({
              estado: "completada",
              resultado: { exit: 0, files: config.mockBuild ? [] : [sub.file_path], summary: `[manual TUI] ${sub.file_path}` },
            }),
          });
          return Response.json({ ok: true, simulada: true, subtarea: sub.id.slice(0, 8), fin: fin.status });
        } catch (e) {
          return Response.json({ ok: false, simulada: false, error: String(e) });
        }
      }

      return Response.json({ ok: false, error: "ruta desconocida" }, { status: 404 });
}

export function startServer() {
  const srv = buildServer();
  info(`escuchando en http://localhost:${srv.port}`);
  return srv;
}