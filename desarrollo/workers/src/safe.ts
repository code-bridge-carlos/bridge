// Helpers de seguridad del worker (F1): safePath y preempción a 300s.
import path from "node:path";

/**
 * safePath valida que `rel` NO escape del working directory dado.
 * Devuelve la ruta absoluta normalizada si es segura, o `null` si intenta
 * salir (traversal), es absoluta fuera del cwd, o contiene ".." malicioso.
 */
export function safePath(cwd: string, rel: string): string | null {
  if (!rel || rel.trim() === "") return null;
  const norm = path.normalize(rel);
  if (norm.startsWith("..") || norm.includes(path.sep + "..") || path.isAbsolute(norm)) return null;
  const abs = path.resolve(cwd, norm);
  const root = path.resolve(cwd);
  if (abs === root) return null;
  if (!abs.startsWith(root + path.sep)) return null;
  return abs;
}

/**
 * preemptTimeout genera un AbortController con timeout configurable.
 * Devuelve el controlador + un callback para limpiarlo.
 */
export function preemptTimeout(ms: number): { signal: AbortSignal; done: () => void } {
  const ctrl = new AbortController();
  const t = setTimeout(() => ctrl.abort(), ms);
  return { signal: ctrl.signal, done: () => clearTimeout(t) };
}

/** Constante de preempción del plan (F1: 300s máx por subtarea). */
export const PREEMPT_MS = Number(Bun.env.PREEMPT_MS ?? 300_000);