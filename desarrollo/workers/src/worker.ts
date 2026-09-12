// Ciclo de vida del worker (D1/D2 base + D2 ciclo real):
//
// 1. Long-poll a s1: GET /api/notificaciones/{worker_id} (repite si 204).
// 2. Al recibir una subtarea: la ejecuta con OpenCode (modo build) o con un
//    resultado mock (dev sin LLM).
// 3. Guía rígida: exit!=0 o error de OpenCode → estado 'fallida' (s2 decide
//    reasignar); resultado exitoso → 'completada'.
// 4. POST /api/subtareas/{id}/fin con {estado, resultado:{exit,files,summary}}.
// 5. Espera sin consultar mientras procesa; vuelve a long-poll al terminar.
import path from "node:path";
import { config } from "./config";
import { info, warn, error as errLog } from "./logger";
import { OpenCodeClient } from "./opencode-client";
import { safePath, PREEMPT_MS } from "./safe";
import { visual } from "./context";

/** Subtarea tal como la entrega s1 en el long-poll (POST /api/subtareas/{id}/fin). */
export interface Subtarea {
  id: string;
  task_id: string;
  worker_id: string;
  file_path: string;
  prompt: string;
  contexto?: string;
  resultado?: unknown;
}

export interface ResultadoSubtarea {
  exit: number;
  files: string[];
  summary: string;
  [k: string]: unknown;
}

const oc = new OpenCodeClient();
let ocupado = false;

/** GET /api/notificaciones/{worker_id} — long-poll (retorna null en 204). */
async function longPoll(): Promise<Subtarea | null> {
  const ctrl = new AbortController();
  const t = setTimeout(() => ctrl.abort(), config.pollTimeoutMs + 5000);
  try {
    const res = await fetch(`${config.s1Url}/api/notificaciones/${config.workerId}`, {
      headers: { "X-Control-Token": config.controlToken },
      signal: ctrl.signal,
    });
    if (res.status === 204) return null;
    if (!res.ok) {
      warn("long-poll: respuesta rara", { status: res.status });
      return null;
    }
    return (await res.json()) as Subtarea;
  } catch (e) {
    // timeout esperado del long-poll: el worker repite
    return null;
  } finally {
    clearTimeout(t);
  }
}

/** Ejecuta la subtarea con OpenCode build (o mock en dev). */
async function ejecutar(sub: Subtarea): Promise<ResultadoSubtarea> {
  const file = safePath(config.workDir, sub.file_path ?? "");
  if (sub.file_path && !file) {
    warn("safePath: archivo fuera del cwd, rechazado", { path: sub.file_path });
    return { exit: 1, files: [], summary: `rechazado por safePath: ${sub.file_path}` };
  }
  info("ejecutando subtarea", { sub: sub.id.slice(0, 8), file: file ?? sub.file_path });
  visual.append("procesando", `Subtarea ${sub.id} → ${sub.prompt.slice(0, 200)}`);

  if (config.mockBuild) {
    await new Promise((r) => setTimeout(r, 1500)); // simula trabajo
    return {
      exit: 0,
      files: file ? [file] : [],
      summary: `[mock-build] análisis de ${file || sub.file_path || sub.id} completado (${config.workerId}).`,
    };
  }

  const prompt =
    `Eres el worker ${config.workerId} del sistema multiagente.\n` +
    `Ejecuta esta subtarea (sin memoria entre tareas):\n\n` +
    `ARCHIVO: ${file ?? sub.file_path}\n` +
    (sub.contexto ? `CONTEXTO: ${sub.contexto}\n\n` : "") +
    `INSTRUCCIÓN:\n${sub.prompt}\n\n` +
    `Al terminar responde con un resumen ejecutivo de lo realizado (qué tocaste, resultado, pruebas).`;

  const res = await oc.runPrompt(prompt, PREEMPT_MS);
  return {
    exit: 0,
    files: file ? [file] : [],
    summary: res,
  };
}

/** POST /api/subtareas/{id}/fin — entrega el resultado a s1. */
async function reportarFin(sub: Subtarea, resultado: ResultadoSubtarea): Promise<void> {
  const res = await fetch(`${config.s1Url}/api/subtareas/${sub.id}/fin`, {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-Control-Token": config.controlToken },
    body: JSON.stringify({
      estado: resultado.exit === 0 ? "completada" : "fallida",
      resultado,
    }),
  });
  if (!res.ok) {
    warn("s1 no aceptó el fin de subtarea", { status: res.status, sub: sub.id.slice(0, 8) });
  } else {
    info("subtarea finalizada", { sub: sub.id.slice(0, 8), estado: resultado.exit === 0 ? "completada" : "fallida" });
  }
}

/** Bucle principal: poll → ejecutar → reportar → repetir. */
export async function run() {
  info("worker arrancando", {
    worker: config.workerId,
    agente: config.agentMode,
    s1: config.s1Url,
    mockBuild: config.mockBuild,
    puerto: config.port,
  });
  let backoffMs = 1000;

  while (true) {
    if (ocupado) {
      await new Promise((r) => setTimeout(r, 200));
      continue;
    }
    const sub = await longPoll();
    if (!sub) {
      await new Promise((r) => setTimeout(r, Math.min(backoffMs, 5000)));
      backoffMs = Math.max(1000, Math.min(backoffMs * 2, 10_000));
      continue;
    }

    backoffMs = 1000;
    ocupado = true;
    visual.append("recibida", `Subtarea ${sub.id} (${config.workerId}) → ${sub.file_path}`);

    const resultado: ResultadoSubtarea = {
      exit: 0,
      files: [],
      summary: "(sin ejecutar)",
    };
    try {
      const r = await ejecutar(sub);
      Object.assign(resultado, r);
    } catch (e) {
      errLog("error ejecutando subtarea", { sub: sub.id.slice(0, 8), err: String(e) });
      resultado.exit = 1;
      resultado.summary = `error: ${String(e)}`;
    } finally {
      visual.append("resultado", `Subtarea ${sub.id} → exit ${resultado.exit}`);
      await reportarFin(sub, resultado);
      ocupado = false;
    }
  }
}