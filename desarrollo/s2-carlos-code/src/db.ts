// Acceso a la base de datos compartida (Postgres / Supabase).
// s2 lee/escribe tasks y consulta subtask_queue para evaluar resultados.
import postgres from "postgres";
import { config } from "./config";
import { error } from "./logger";

let sql: ReturnType<typeof postgres> | null = null;

export function db(): ReturnType<typeof postgres> {
  if (!sql) {
    sql = postgres(config.databaseUrl, { max: 4, idle_timeout: 20 });
  }
  return sql;
}

export async function health() {
  try {
    await db()`SELECT 1`;
    return true;
  } catch (e) {
    error("db health", { err: String(e) });
    return false;
  }
}

/** Estados de tasks según schema. */
export const TASK_STATES = [
  "planificando", "ejecutando", "completada", "fallida",
] as const;
export type TaskState = (typeof TASK_STATES)[number];

export interface TaskRow {
  id: string;
  estado: string;
  [k: string]: unknown;
}

/**
 * Marca el estado de una tarea. Devuelve la fila o null si no existe.
 * s1 crea la task al enviarla; s2 la actualiza durante su ciclo de vida.
 */
export async function setTaskState(
  taskId: string,
  estado: TaskState,
): Promise<TaskRow | null> {
  // tasks NO tiene columna updated_at (solo created_at / finished_at).
  const rows =
    await db()`
      UPDATE tasks SET estado = ${estado}
      WHERE id = ${taskId}
      RETURNING id, estado`;
  return rows.length ? (rows[0] as unknown as TaskRow) : null;
}

/** Alias de setTaskState que acepta string (evita casting en evaluarResultados). */
export async function setTaskEstado(taskId: string, estado: string) {
  return setTaskState(taskId, estado as TaskState);
}

/** Lee una tarea (todo, incluido plan_json e intentos_reasignacion si existe). */
export async function getTask(taskId: string) {
  const rows = await db()`
    SELECT id, prompt, estado, plan_json, intentos_reasignacion, finished_at, created_at
    FROM tasks WHERE id = ${taskId}`;
  return rows.length ? rows[0] : null;
}

/** Guarda el plan_json en una tarea. postgres.js serializa el objeto a jsonb. */
export async function savePlan(taskId: string, plan: object) {
  const v = plan as unknown; // postgres.js acepta objetos serializables en runtime
  await db()`
    UPDATE tasks SET plan_json = ${v as never}
    WHERE id = ${taskId}`;
}

/** Subtareas de un task con su resultado (para evaluar en C2). */
export async function subtareasOf(taskId: string) {
  return await db()`
    SELECT id, file_path, prompt, estado, resultado, worker_id
    FROM subtask_queue WHERE task_id = ${taskId}
    ORDER BY created_at`;
}

/** Devuelve subtareas fallidas de un task (para reasignar). */
export async function failedSubtasks(taskId: string) {
  return await db()`
    SELECT id, worker_id, prompt
    FROM subtask_queue WHERE task_id = ${taskId} AND estado = 'fallida'
    ORDER BY created_at`;
}

/** Estadísticas de progreso de un task. */
export interface TaskStats {
  total: number;
  completadas: number;
  fallidas: number;
  activas: number;
}

export async function taskStats(taskId: string): Promise<TaskStats | null> {
  const rows = await db()`
    SELECT
      count(*)::int as total,
      count(*) filter (where estado='completada')::int as completadas,
      count(*) filter (where estado='fallida')::int as fallidas,
      count(*) filter (where estado in ('pendiente','asignada','en_progreso'))::int as activas
    FROM subtask_queue WHERE task_id = ${taskId}`;
  return rows.length ? (rows[0] as unknown as TaskStats) : null;
}
