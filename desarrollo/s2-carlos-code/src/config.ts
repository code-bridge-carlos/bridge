// Configuración central de s2. Todo desde variables de entorno;
// defaults pensados para desarrollo local (Supabase en producción).
export const config = {
  /** Puerto del servicio HTTP de s2. */
  port: Number(Bun.env.PORT ?? 9002),

  /** Postgres compartido (mismo que s1; Supabase en producción). */
  databaseUrl:
    Bun.env.DATABASE_URL ??
    "postgres://postgres:test@localhost:5433/testdb?sslmode=disable",

  /** Panel s1: destino de las subtareas y de donde venimos. */
  s1Url: Bun.env.S1_URL ?? "http://localhost:8080",

  /** Token de control compartido con s1 (F13 endurece; aquí solo M2M). */
  controlToken: Bun.env.CONTROL_TOKEN ?? "dev-token-b4",

  /** Servidor OpenCode headless (`opencode serve`). */
  opencodeUrl: Bun.env.OPENCODE_URL ?? "http://localhost:18000",

  /**
   * Modo permanente del orquestador: plan (modelo capaz con memoria).
   * Mapea a .opencode/agent/plan.md (ver agent-orquestador.md).
   */
  agentMode: Bun.env.AGENT_MODE ?? "plan",

  /**
   * Dev: al recibir una tarea, s2 la divide en 3 subtareas de ejemplo y se
   * las envía a s1 (cierra el bucle de pruebas sin LLM). Producción usa el
   * flujo real de planificación (Fase 8).
   */
  emitOnTask: Bun.env.S2_EMIT_ON_TASK === "1",

  /**
   * Dev: S2_MOCK_PLAN=1 responde con un plan JSON de ejemplo en vez de
   * llamar a OpenCode. Ejercita el flujo REAL de planificación (guardar
   * plan_json, dividir, enviar a s1, evaluar resultados) sin LLM. La
   * planificación real (OpenCode) se activa sin esta variable.
   */
  mockPlan: Bun.env.S2_MOCK_PLAN === "1",

  /**
   * C2: gate de aprobación. Con S2_AUTO_APPROVE ≠ "0", tras crear el plan s2
   * divide y envía las subtareas a s1 inmediatamente (dev / sin TUI). Con
   * "0", la tarea queda en 'planificando' esperando POST /api/task/{id}/aprobar
   * (el usuario corrige/aprueba el plan; usará la TUI en Grupo E).
   */
  autoApprove: Bun.env.S2_AUTO_APPROVE !== "0",

  /**
   * Ventana de contexto del orquestador: sección de 200k tokens con
   * auto-compactación al llegar a 170k (los segmentos más antiguos se
   * resumen y descargan a Supabase; ver compact.ts).
   */
  contextMaxTokens: Number(Bun.env.CONTEXT_MAX_TOKENS ?? 200_000),
  contextCompactAt: Number(Bun.env.CONTEXT_COMPACT_AT ?? 170_000),
};

export type Config = typeof config;