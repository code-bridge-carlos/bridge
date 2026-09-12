// Configuración central de un worker (w6..w10). Todo desde variables de
// entorno; defaults pensados para desarrollo local (Supabase en producción).
export const config = {
  /** Puertos: 9006 (w6), 9007 (w7), ... 9010 (w10). */
  port: Number(Bun.env.PORT ?? (9006 + workerIndex(Bun.env.WORKER_ID ?? "w6"))),

  /** Id del worker: w6..w10 (debe coincidir con la vista del panel). */
  workerId: (Bun.env.WORKER_ID ?? "w6").toLowerCase(),

  /** Panel s1: fuente de subtareas (long-poll) y destino del resultado. */
  s1Url: Bun.env.S1_URL ?? "http://localhost:8080",

  /** Token de control compartido con s1 (M2M; F13 endurece). */
  controlToken: Bun.env.CONTROL_TOKEN ?? "dev-token-b4",

  /** Servidor OpenCode headless (`opencode serve`). */
  opencodeUrl: Bun.env.OPENCODE_URL ?? "http://localhost:18000",

  /**
   * Modo permanente del worker: build. Los workers ejecutan, no planifican.
   * Mapea a .opencode/agent/build.md (ver agent-worker.md).
   */
  agentMode: Bun.env.AGENT_MODE ?? "build",

  /**
   * Dev: WORKER_MOCK_BUILD=1 responde con un resultado sintético en vez de
   * llamar a OpenCode. Ejercita el ciclo REAL del worker (long-poll, lectura
   * de la subtarea, fin con resultado) sin LLM. El modo build real se activa
   * sin esta variable.
   */
  mockBuild: Bun.env.WORKER_MOCK_BUILD === "1",

  /** Long-poll a s1: cuánto espera cada intento (s1 aguanta ~30s). */
  pollTimeoutMs: Number(Bun.env.POLL_TIMEOUT_MS ?? 28_000),

  /** Directorio de trabajo seguro: los archivos deben residir aquí (F1). */
  workDir: Bun.env.WORKER_WORKDIR ?? process.cwd(),

  /** Modelo a usar (opcional; OpenCode usa su configuración si no se pone). */
  model: Bun.env.MODEL,

  /** Ventana visual del worker: sección de 50k tokens que NO va al LLM. */
  visualMaxTokens: Number(Bun.env.VISUAL_MAX_TOKENS ?? 50_000),
};

function workerIndex(id: string): number {
  const n = parseInt(id.replace(/[^0-9]/g, ""), 10);
  if (Number.isNaN(n) || n < 6 || n > 10) return 0;
  return n - 6;
}

export type Config = typeof config;