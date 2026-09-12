// Servidor HTTP de s2 (Bun.serve). Rutas compatibles con lo que s1 espera:
//
// Flujo real (C2):
//   POST /api/tasks         → s1 envía una tarea
//   → s2 llama a OpenCode (plan) → guarda plan_json → divide en subtareas
//   → envía lista a s1 → evalúa resultados (fallidas → reasignar)
//
// Flujo dev (S2_EMIT_ON_TASK=1):
//   Igual que antes: divide en 3 subtareas hardcoded sin LLM.
//
// Rutas adicionales:
//   POST /api/task/{id}/aprobar  → usuario aprueba el plan (TUI o panel)
//   POST /api/task/{id}/rechazar → usuario rechaza / pide corrección
import { config } from "./config";
import { db, getTask, subtareasOf, setTaskState, savePlan, setTaskEstado, failedSubtasks, taskStats } from "./db";
import { info, warn } from "./logger";
import { ContextWindow } from "./compact";
import { OpenCodeClient } from "./opencode-client";
import { validarTokenTUI, paginaTUI, redirigirPanel } from "./tui";
import { isPreflight, preflightResponse, withCORS } from "./cors";

/** Cuerpo de POST /api/tasks (lo que s1 envía). */
interface TareaEntrante {
  tarea_id?: string;  // uuid de tareas_panel (s1)
  task_id: string;    // uuid de tasks (creado por s1 al enviar)
  prompt: string;
}

/** Cuerpo de POST /api/notify (fin de subtarea). */
interface NotifyFin {
  task_id: string;
  subtarea_id: string;
  worker_id: string;
  estado: "completada" | "fallida";
}

/** Cuerpo de POST /api/task/{id}/aprobar (opcional: corrige el plan; vacío = usa plan_json). */
interface AprobarReq {
  subtareas?: Array<{
    file_path: string;
    prompt: string;
    contexto?: string;
  }>;
}

/** Ventana de contexto del orquestador (sección 200k, auto-compact 170k). */
export const ctxWindow = new ContextWindow();

/** Cliente OpenCode (se reutiliza entre requests). */
const opencode = new OpenCodeClient();

// ── Prompt de planificación para OpenCode ────────────────────────────────────

const PLAN_PROMPT = (taskPrompt: string, taskId: string) => `Eres CARLOS_CODE, el orquestador del sistema multiagentente.

TAREA RECIBIDA (task ${taskId}):
${taskPrompt}

INSTRUCCIONES:
1. Analiza la tarea y genera un plan de trabajo dividido en subtareas atómicas.
2. Cada subtarea debe ser autocontenida (el worker no tiene memoria entre tareas).
3. Las subtareas deben ser paralelizables entre 5 workers (w6-w10).
4. Cada subtarea tiene: file_path (archivo/área), prompt (instrucción completa), contexto (referencias).

RESPONDE EXCLUSIVAMENTE con un JSON válido (sin markdown, sin fences):
{
  "plan_resumen": "resumen del plan en 1-2 líneas",
  "subtareas": [
    {
      "file_path": "ruta/del/archivo.go",
      "prompt": "instrucción completa y autocontenida para el worker",
      "contexto": "referencias, docs, archivos relacionados"
    }
  ]
}

REGLAS:
- Mínimo 2, máximo 10 subtareas.
- Cada prompt debe ser suficiente para que el worker ejecute sin preguntar.
- file_path debe ser específico (no "archivos varios").
- Si la tarea es de solo lectura/analisis, indica contexto="solo lectura" en cada una.`;

// ── Envío de subtareas a s1 ──────────────────────────────────────────────────

/** Envía la lista de subtareas a s1 (usado tanto por flujo real como dev). */
async function emitSubtareasA(
  s1: string,
  token: string,
  taskId: string,
  subtareas: Array<{ file_path: string; prompt: string; contexto: string }>,
) {
  const res = await fetch(`${s1}/api/subtareas`, {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-Control-Token": token },
    body: JSON.stringify({ task_id: taskId, subtareas }),
  });
  const body = await res.text();
  info("enviadas subtareas a s1", {
    taskId: taskId.slice(0, 8),
    count: subtareas.length,
    status: res.status,
    s1_resp: body.slice(0, 200),
  });
  return res.status;
}

// ── Flujo real de planificación (C2) ─────────────────────────────────────────

async function planificarYEnviar(taskId: string, tareaId: string | undefined, prompt: string) {
  info("iniciando planificación real", { taskId: taskId.slice(0, 8) });

  // 1. Marcar como planificando
  await setTaskState(taskId, "planificando");
  ctxWindow.append("input", `Tarea ${taskId}\n${prompt}`);

  // 2. Llamar a OpenCode para generar el plan (o mock en dev sin LLM)
  let respuesta: string;
  if (config.mockPlan) {
    respuesta = mockPlanResponse(prompt, taskId);
    info("plan mock usado (S2_MOCK_PLAN=1)", { taskId: taskId.slice(0, 8) });
  } else {
    try {
      respuesta = await opencode.runPrompt(PLAN_PROMPT(prompt, taskId), 300_000);
    } catch (e) {
      warn("OpenCode falló al planificar", { err: String(e) });
      await setTaskState(taskId, "fallida");
      return { ok: false, error: `OpenCode falló: ${String(e)}` };
    }
  }

  info("respuesta de OpenCode recibida", {
    taskId: taskId.slice(0, 8),
    longitud: respuesta.length,
  });

  // 3. Parsear el plan JSON
  let plan: { plan_resumen: string; subtareas: Array<{ file_path: string; prompt: string; contexto: string }> };
  try {
    // Limpiar fences markdown si el modelo los incluye
    let limpia = respuesta.trim();
    if (limpia.startsWith("```")) {
      limpia = limpia.replace(/^```(?:json)?\n?/, "").replace(/\n?```$/, "");
    }
    plan = JSON.parse(limpia);
  } catch (e) {
    warn("respuesta de OpenCode no es JSON válido", { err: String(e), snippet: respuesta.slice(0, 300) });
    await setTaskState(taskId, "fallida");
    return { ok: false, error: "El plan de OpenCode no es JSON válido", respuesta_bruta: respuesta.slice(0, 500) };
  }

  if (!plan.subtareas || !Array.isArray(plan.subtareas) || plan.subtareas.length === 0) {
    warn("plan sin subtareas", { plan });
    await setTaskState(taskId, "fallida");
    return { ok: false, error: "El plan no contiene subtareas" };
  }

  // 4. Guardar plan_json en tasks
  await savePlan(taskId, plan);

  // 5. Guardar en ventana de contexto
  ctxWindow.append("plan", `Plan: ${plan.plan_resumen}\nSubtareas: ${plan.subtareas.length}`);

  // 5b. Sin auto-approve: la tarea espera aprobación del usuario (TUI/panel).
  if (!config.autoApprove) {
    info("plan creado; esperando aprobación (S2_AUTO_APPROVE=0)", {
      taskId: taskId.slice(0, 8),
      subtareas: plan.subtareas.length,
    });
    await setTaskState(taskId, "planificando");
    return {
      ok: true,
      task_id: taskId,
      plan_resumen: plan.plan_resumen,
      subtareas: plan.subtareas.length,
      estado: "esperando_aprobacion",
    };
  }

  // 6. Enviar subtareas a s1
  const status = await emitSubtareasA(config.s1Url, config.controlToken, taskId, plan.subtareas);
  if (status !== 200) {
    warn("s1 rechazó subtareas", { status });
    return { ok: false, error: `s1 rechazó subtareas (HTTP ${status})` };
  }

  // 7. Marcar como ejecutando
  await setTaskState(taskId, "ejecutando");

  return {
    ok: true,
    task_id: taskId,
    plan_resumen: plan.plan_resumen,
    subtareas: plan.subtareas.length,
  };
}

// ── Mock de planificación (dev: sin LLM, ejerce el flujo real) ───────────────

function mockPlanResponse(prompt: string, taskId: string): string {
  const resumen = `Plan de ejemplo para '${prompt.slice(0, 60)}' (${taskId.slice(0, 8)}): 3 subtareas de análisis.`;
  return JSON.stringify({
    plan_resumen: resumen,
    subtareas: [
      {
        file_path: "src/core/valorar.go",
        prompt: `Valora la tarea ${taskId.slice(0, 8)}: '${prompt.slice(0, 40)}' (relevancia y dificultad).`,
        contexto: "solo lectura; mock-plan",
      },
      {
        file_path: "src/core/decidir.go",
        prompt: `Decide: ¿el plan de ${taskId.slice(0, 8)} es viable o requiere más contexto?`,
        contexto: "solo lectura; mock-plan",
      },
      {
        file_path: "src/core/ejecutar.go",
        prompt: `Ejecuta el análisis de ${taskId.slice(0, 8)} y resume el flujo principal.`,
        contexto: "solo lectura; mock-plan",
      },
    ],
  });
}

// ── Evaluar resultados (C2) ──────────────────────────────────────────────────

async function evaluarResultados(taskId: string) {
  const stats = await taskStats(taskId);
  if (!stats) return;

  info("evaluando resultados", {
    taskId: taskId.slice(0, 8),
    total: stats.total,
    completadas: stats.completadas,
    fallidas: stats.fallidas,
    activas: stats.activas,
  });

  // Si quedan activas, aún no evaluamos
  if (stats.activas > 0) return;

  // Si hay fallidas, reintentar hasta `limite` veces (tasks.intentos_reasignacion).
  if (stats.fallidas > 0) {
    const fallidas = await failedSubtasks(taskId);
    if (fallidas.length > 0) {
      const t = await getTask(taskId);
      const intentos = Number(t?.intentos_reasignacion ?? 0);
      const limite = 2;
      if (intentos >= limite) {
        warn("límite de reintentos alcanzado", { taskId: taskId.slice(0, 8), intentos, limite });
        await setTaskEstado(taskId, "fallida");
        return;
      }
      info("reasignando subtareas fallidas", { count: fallidas.length, intento: intentos + 1 });
      for (const sub of fallidas) {
        // Resetear a pendiente para que el dispatcher la reasigne
        await db()`
          UPDATE subtask_queue SET estado='pendiente', worker_id=NULL, resultado=NULL
          WHERE id = ${sub.id} AND estado='fallida'`;
      }
      // Incrementar el contador y seguir ejecutando
      await db()`
        UPDATE tasks SET intentos_reasignacion = intentos_reasignacion + 1, estado='ejecutando'
        WHERE id = ${taskId}`;
      return;
    }
  }

  // Todas completadas (o reasignadas)
  if (stats.fallidas === 0 && stats.completadas === stats.total) {
    await setTaskEstado(taskId, "completada");
    info("task completada", { taskId: taskId.slice(0, 8) });
  }
}

// ── Servidor ─────────────────────────────────────────────────────────────────

export function buildServer() {
  return Bun.serve({
    port: config.port,
    // La planificación real con OpenCode tarda hasta 300s: Bun no debe cortar.
    idleTimeout: 0,
    async fetch(req) {
      // OPTIONS preflight — CORS controlado por allowlist (F1)
      if (isPreflight(req)) return preflightResponse(req.headers.get("origin"));
      const res = await dispatch(req);
      return withCORS(res, req.headers.get("origin"));
    },
  });
}

/** Router interno (las respuestas se envuelven con CORS en fetch). */
async function dispatch(req: Request): Promise<Response> {
      const url = new URL(req.url);
      const path = url.pathname;

      // GET /health — keep-alive de s1
      if (req.method === "GET" && path === "/health") {
        return Response.json({
          ok: true,
          servicio: "s2-carlos-code",
          estado: "arrancado",
          agente: config.agentMode,
          ventana: ctxWindow.stats(),
          ram_mb: Math.round(process.memoryUsage().rss / 1024 / 1024),
        });
      }

      // GET /estado — estado detallado (para la TUI)
      if (req.method === "GET" && path === "/estado") {
        return Response.json({
          servicio: "s2-carlos-code",
          agente: config.agentMode,
          modo: config.mockPlan ? "mock-plan" : "real",
          autoApprove: config.autoApprove,
          emitOnTask: config.emitOnTask,
          ventana: ctxWindow.stats(),
          opencode: config.opencodeUrl,
          s1: config.s1Url,
        });
      }

      // GET /tui — TUI del orquestador (modo coordenador, sin formularios).
      if (req.method === "GET" && path === "/tui") {
        const user = await validarTokenTUI(req, "s2");
        if (user === "") return redirigirPanel(req);
        return new Response(paginaTUI({
          servicio: "s2",
          titulo: "CARLOS_CODE · Orquestador",
          // S2-A1: Eliminado panel TAREA CONCRETA, APROBAR PLAN y LOG separados.
          // La síntesis es corta: decisión, resultado, siguiente acción.
          secciones: `
  <div class="card"><h2>💭 Contexto</h2><pre id="contexto">Escribe / para ver comandos</pre></div>
  <div class="card"><h2>📜 Historial</h2><pre id="historial">Sin historial</pre></div>
  <div class="card"><h2>⚙️ Estado</h2><pre id="estado">(cargando...</pre></div>`,
          js: `
async function cargarEstado(){
  try{
    const h=await json('/health');
    const st=await json('/estado');
    set('estado', JSON.stringify({health:h, estado:st}, null, 2));
  }catch(e){set('estado','ERROR: '+e.message)}
  document.getElementById('log').textContent='TUI coordinador activo';
}
function anyadirRegistro(mensaje, tipo='info'){
  const log=document.getElementById('log');
  const div=document.createElement('div');
  div.textContent = `${new Date().toLocaleTimeString() } [${tipo}] ${mensaje}`;
  log.appendChild(div);
  log.scrollTop = log.scrollHeight;
}
async function enviarChat(){
  const input=document.getElementById('entrada');
  const msg = input.value.trim();
  if(!msg)return;
  anyadirRegistro('tú: ' + msg, 'usuario');
  input.value = '';
  // Ejecutar comando interno
  if(msg.startsWith('/')){
    await ejecutarComando(msg);
  }else{
    // Enviar como pregunta al coordinador
    anyadirRegistro('coordinador: ' + msg, 'asistente');
    fetch('/api/contexto', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({pregunta: msg})})
      .then(r=>r.json()).then(d=>anyadirRegistro('coordinador: ' + (d.respuesta||'Sin respuesta'), 'asistente'));
  }
}
async function ejecutarComando(cmd){
  const c = cmd.slice(1);
  if(c === 'clear'){ set('historial', 'Sin historial'); return; }
  if(c === 'models'){ set('historial', 'Modelos: openai, anthropic, gemini, deepseek (configurar via /set-model)'); return; }
  if(c === 'start'){ set('historial', 'Plan aprobado. S2 dividirá en sub-planes con /ready.'); return; }
  if(c === 'ready'){ set('historial', 'Sub-planes presentados. Esperando aprobación de sub-tareas.'); return; }
  if(c === 'clear-cache'){ set('historial', 'Cache limpiado'); return; }
  anyadirRegistro('Comando desconocido: ' + cmd, 'warning');
}
document.getElementById('entrada').focus();

// Streaming de tokens consumidos
async function actualizarTokens(){
  try{
    const r=await fetch('/estado');
    const d=await r.json();
    if(d.ram_mb)anyadirRegistro(`RAM: ${d.ram_mb}MB`, 'info');
  }catch(e){}
}
setInterval(actualizarTokens, 3000);
actualizarTokens();`}),
        { headers: { "Content-Type": "text/html; charset=utf-8" } });
      }

      // POST /api/tasks — s1 entrega una tarea
      if (req.method === "POST" && path === "/api/tasks") {
        const tarea = (await req.json()) as TareaEntrante;
        if (!tarea.task_id || !tarea.prompt) {
          return Response.json({ ok: false, error: "task_id y prompt obligatorios" }, { status: 400 });
        }
        info("tarea recibida de s1", {
          task_id: tarea.task_id,
          tarea_id: tarea.tarea_id ?? null,
          prompt: tarea.prompt.slice(0, 100),
        });

        // Modo dev: emitir subtareas hardcoded (sin LLM)
        if (config.emitOnTask) {
          const devSubs = [
            {
              file_path: "src/core/valorar.go",
              prompt: `Valora la tarea ${tarea.task_id.slice(0, 8)}: '${tarea.prompt.slice(0, 60)}' (relevancia y dificultad).`,
              contexto: "solo lectura; dev",
            },
            {
              file_path: "src/core/decidir.go",
              prompt: `Decide: ¿el plan de ${tarea.task_id.slice(0, 8)} es viable o requiere más contexto?`,
              contexto: "solo lectura; dev",
            },
            {
              file_path: "src/core/ejecutar.go",
              prompt: `Ejecuta el análisis de ${tarea.task_id.slice(0, 8)} y resume el flujo principal.`,
              contexto: "solo lectura; dev",
            },
          ];
          const st = await emitSubtareasA(config.s1Url, config.controlToken, tarea.task_id, devSubs);
          if (st !== 200) {
            return Response.json({ ok: false, error: "s1 rechazó subtareas" }, { status: 502 });
          }
          return Response.json({ ok: true, task_id: tarea.task_id, modo: "dev" });
        }

        // Flujo real: planificar con OpenCode
        const result = await planificarYEnviar(tarea.task_id, tarea.tarea_id, tarea.prompt);
        return Response.json(result, { status: result.ok ? 200 : 500 });
      }

      // POST /api/notify — s1 avisa del fin de una subtarea
      if (req.method === "POST" && path === "/api/notify") {
        const fin = (await req.json()) as NotifyFin;
        info("fin de subtarea notificado por s1", {
          task_id: fin.task_id?.slice(0, 8),
          subtarea_id: fin.subtarea_id?.slice(0, 8),
          worker_id: fin.worker_id,
          estado: fin.estado,
        });
        ctxWindow.append("resultado",
          `Subtarea ${fin.subtarea_id} (worker ${fin.worker_id}) → ${fin.estado}`, true);

        // Evaluar si el task completo terminó
        await evaluarResultados(fin.task_id);

        return Response.json({ ok: true });
      }

      // GET /api/task/{id} — consulta de estado (debug/verificación)
      if (req.method === "GET" && path.startsWith("/api/task/")) {
        const id = path.slice("/api/task/".length);
        const t = await getTask(id);
        if (!t) return Response.json({ ok: false, error: "no existe" }, { status: 404 });
        const subs = await subtareasOf(id);
        return Response.json({ ok: true, tarea: t, subtareas: subs });
      }

      // POST /api/task/{id}/aprobar — en modo S2-A1, la aprobación se indica
      // mediante comando /start en la TUI, no mediante panel separado.
// Body opcional: { subtareas: [...] } para corregir; si llega vacío se
      // usa el plan_json ya guardado.
      if (req.method === "POST" && path.match(/^\/api\/task\/[^/]+\/aprobar$/)) {
        const id = path.split("/")[3];
        let body = {} as AprobarReq;
        try {
          body = (await req.json()) as AprobarReq;
        } catch {
          body = {};
        }
        const t = await getTask(id);
        if (!t) return Response.json({ ok: false, error: "no existe" }, { status: 404 });
        if (t.estado !== "planificando") {
          return Response.json({ ok: false, error: `estado actual: ${t.estado}, esperado: planificando` }, { status: 400 });
        }
        // En modo S2-A1, la aprobación real se gestiona por comandos /start /ready en TUI.
        // Aún así, si se envían subtraciones, las procesamos tal cual.
        let aprobadas = body.subtareas ?? [];
        if (aprobadas.length === 0) {
          const plan = t.plan_json as { subtareas?: Array<{ file_path: string; prompt: string; contexto?: string }> } | null;
          if (plan?.subtareas?.length) {
            aprobadas = plan.subtareas;
          }
        }
        if (aprobadas.length === 0) {
          return Response.json({ ok: false, error: "subtareas vacías (ni plan_json)" }, { status: 400 });
        }
        // Enviar subtareas aprobadas a s1 (s1 gestiona la cola)
        const aprobadasNorm = aprobadas.map((s) => ({
          file_path: s.file_path,
          prompt: s.prompt,
          contexto: s.contexto ?? "",
        }));
        const st = await emitSubtareasA(config.s1Url, config.controlToken, id, aprobadasNorm);
        if (st !== 200) {
          return Response.json({ ok: false, error: `s1 rechazó (HTTP ${st})` }, { status: 502 });
        }
        await setTaskState(id, "ejecutando");
        return Response.json({ ok: true, subtareas: aprobadasNorm.length, modo: "s2-a1" });
      }

      // GET /api/contexto — ventana del orquestador (debug)
      if (req.method === "GET" && path === "/api/contexto") {
        return Response.json({ stats: ctxWindow.stats(), render: ctxWindow.render().slice(0, 500) });
      }

      return Response.json({ ok: false, error: "ruta desconocida en s2" }, { status: 404 });
}
