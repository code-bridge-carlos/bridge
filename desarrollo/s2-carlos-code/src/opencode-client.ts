// Cliente del servidor OpenCode headless (`opencode serve`).
//
// Ciclo documentado en el análisis (desarrollo/docs/analisis-opencode-gentle.md):
//   1. POST /api/session                      → crea sesión {id}
//   2. POST /api/session/{id}/prompt          → envía el prompt (agent/mode plan|build)
//   3. GET  /api/event (SSE)                  → eventos; 'session.idle' = terminó
//   4. POST /api/session/{id}/wait            → bloquea hasta idle
//   5. GET  /api/session/{id}/message?limit=1 → lectura del último mensaje (respuesta)
//
// s2 usa AGENT_MODE=plan (agente orquestador); los workers w6-w10 usarán
// AGENT_MODE=build. Usado a partir de C2 (Fase 8); aquí queda listo y
// tipado con verificación estática.
import { config } from "./config";
import { error, info } from "./logger";

export interface OpenCodeSession {
  id: string;
}

export interface OpenCodeMessage {
  id?: string;
  role?: string;
  text?: string;
}

export class OpenCodeClient {
  private baseUrl: string;

  constructor(baseUrl = config.opencodeUrl) {
    this.baseUrl = baseUrl;
  }

  /** Crea una sesión nueva. */
  async startSession(): Promise<OpenCodeSession> {
    const res = await fetch(`${this.baseUrl}/api/session`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({}),
    });
    if (!res.ok) throw new Error(`opencode: crear sesión ${res.status}`);
    return (await res.json()) as OpenCodeSession;
  }

  /** Envía un prompt a una sesión (mode plan para el orquestador). */
  async sendPrompt(sessionId: string, message: string): Promise<void> {
    const res = await fetch(`${this.baseUrl}/api/session/${sessionId}/prompt`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        message,
        agent: config.agentMode,          // plan | build
        model: Bun.env.MODEL,
      }),
    });
    if (!res.ok) throw new Error(`opencode: prompt ${res.status}: ${await res.text()}`);
  }

  /**
   * Espera a que la sesión quede idle leyendo el stream SSE de eventos
   * (timeout opcional; el servidor responde con eventos hasta session.idle).
   */
  async waitIdle(sessionId: string, timeoutMs = 300_000): Promise<void> {
    const ctrl = new AbortController();
    const t = setTimeout(() => ctrl.abort(), timeoutMs);
    try {
      const res = await fetch(`${this.baseUrl}/api/event`, { signal: ctrl.signal });
      if (!res.body) throw new Error("opencode: evento sin body");
      const reader = res.body.getReader();
      const dec = new TextDecoder();
      let buffer = "";
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        buffer += dec.decode(value, { stream: true });
        const lines = buffer.split("\n");
        buffer = lines.pop() ?? "";
        for (const line of lines) {
          if (line.startsWith("data:")) {
            try {
              const ev = JSON.parse(line.slice(5).trim());
              if (ev.session?.status === "idle") {
                ctrl.abort();
                return;
              }
            } catch {
              /* evento no-JSON: ignorar */
            }
          }
        }
      }
    } catch (e) {
      if ((e as Error).name === "AbortError") {
        info("waitIdle: timeout/cancelado");
        return;
      }
      throw e;
    } finally {
      clearTimeout(t);
    }
  }

  /** Último mensaje de la sesión (respuesta del agente). */
  async lastMessage(sessionId: string): Promise<OpenCodeMessage | null> {
    const res = await fetch(
      `${this.baseUrl}/api/session/${sessionId}/message?limit=1`,
    );
    if (!res.ok) return null;
    const arr = (await res.json()) as OpenCodeMessage[];
    return arr.at(-1) ?? null;
  }

  /** Ciclo completo: sesión → prompt → idle → respuesta. Devuelve el texto. */
  async runPrompt(message: string, timeoutMs?: number): Promise<string> {
    const s = await this.startSession();
    info("opencode: sesión creada", { id: s.id, agent: config.agentMode });
    await this.sendPrompt(s.id, message);
    await this.waitIdle(s.id, timeoutMs);
    const msg = await this.lastMessage(s.id);
    return msg?.text ?? "(sin respuesta)";
  }
}