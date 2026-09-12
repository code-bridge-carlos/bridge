// TUI del servicio (E1): página HTML ligera que requiere un token temporal
// emitido por s1 (POST /api/tui/token) y lo valida contra s1
// (GET /api/tui/valida). Sin dependencias externas; fetch + render nativo.
import { config } from "./config";
import { warn } from "./logger";

/** Válida un token TUI contra s1. Devuelve el usuario o "" si no es válido. */
export async function validarTokenTUI(req: Request, servicio: string): Promise<string> {
  const url = new URL(req.url);
  const token = url.searchParams.get("token") ?? "";
  if (!token) return "";
  try {
    const v = await fetch(
      `${config.s1Url}/api/tui/valida?servicio=${servicio}&token=${encodeURIComponent(token)}`,
      {
        method: "GET",
        headers: { "X-Control-Token": config.controlToken },
        signal: AbortSignal.timeout(8000),
      },
    );
    if (!v.ok) return "";
    const data = (await v.json()) as { ok?: string; usuario?: string };
    return data.usuario ?? "";
  } catch (e) {
    warn("validar token TUI falló", { err: String(e) });
    return "";
  }
}

/** Construye la página HTML del TUI del servicio (dark, monospace). */
export function paginaTUI(opts: {
  servicio: string;
  titulo: string;
  secciones: string; // HTML extra (formularios por servicio)
  js: string;        // JS extra (fetch de estado por servicio)
}): string {
  return `<!DOCTYPE html>
<html lang="es"><head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>${opts.titulo}</title>
<style>
  *{box-sizing:border-box;margin:0;padding:0}
  body{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;background:#0b0e14;color:#d6dee8;padding:18px}
  h1{font-size:15px;color:#7aa2f7;margin-bottom:14px;display:flex;justify-content:space-between;align-items:center}
  h1 .svc{color:#48b16c}
  h1 .back a{color:#565f89;font-size:12px;text-decoration:none}
  .grid{display:grid;grid-template-columns:1fr;gap:12px}
  .card{background:#12161f;border:1px solid #1e2632;border-radius:8px;padding:12px}
  .card h2{font-size:12px;color:#565f89;text-transform:uppercase;letter-spacing:1px;margin-bottom:8px}
  pre{white-space:pre-wrap;word-break:break-word;font-size:12px;line-height:1.45;color:#a9b1d6}
  .ok{color:#48b16c}.bad{color:#f7768e}.warn{color:#e0af68}
  input[type=text],textarea{width:100%;background:#0b0e14;border:1px solid #1e2632;color:#a9b1d6;border-radius:6px;padding:8px;font-family:inherit;font-size:12px}
  button{background:#1f2a3f;border:1px solid #2c3a55;color:#a9b1d6;border-radius:6px;padding:7px 12px;font-size:12px;cursor:pointer}
  button:hover{background:#2c3a55}
  table{width:100%;border-collapse:collapse;font-size:12px}
  th,td{text-align:left;padding:5px 8px;border-bottom:1px solid #1e2632}
  th{color:#565f89;font-size:11px;text-transform:uppercase}
  .row{display:flex;gap:8px;margin-bottom:8px}
  .row>*{flex:1}
  #log{font-size:11px;color:#565f89;margin-top:10px;white-space:pre-wrap}
</style></head><body>
<h1>
  <span>${opts.titulo} <span class="svc">[${opts.servicio}]</span></span>
  <span class="back"><a href="/health" target="_blank">/health</a> · <a href="/estado" target="_blank">/estado</a></span>
</h1>
<div class="grid">
  ${opts.secciones}
  <div class="card"><h2>Log</h2><div id="log">(esperando...)</div></div>
</div>
<script>
const SERVICIO = ${JSON.stringify(opts.servicio)};
async function json(url){const r=await fetch(url);return r.json()}
function esc(s){const d=document.createElement('div');d.textContent=s??'';return d.innerHTML}
function set(id,s){const el=document.getElementById(id);if(el)el.textContent=s}
${opts.js}
</script></body></html>`;
}

/** Redirige al panel s1 (expiración/ausencia de token). */
export function redirigirPanel(req: Request): Response {
  const url = new URL(req.url);
  const back = url.searchParams.get("back");
  const destino = back ?? `${config.s1Url}/login`;
  return Response.redirect(destino, 302);
}