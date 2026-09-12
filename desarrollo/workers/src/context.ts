// Ventana VISUAL del worker: sección de 50k tokens que NO se envía al LLM.
// Sirve para la TUI/estado del servicio (E1) y para auditoría del ciclo de
// vida. Auto-eliminación: cuando el total alcanza VISUAL_MAX_TOKENS, se
// descarta el historial más antiguo manteniendo el resumen del trimestre.
import { config } from "./config";

export interface VisualEntry {
  kind: "recibida" | "procesando" | "resultado";
  text: string;
  createdAt: number;
  tokens: number;
}

export function estimateTokens(text: string): number {
  return Math.ceil(text.length / 4);
}

export class VisualWindow {
  entries: VisualEntry[] = [];
  totalTokens = 0;

  append(kind: VisualEntry["kind"], text: string) {
    const tokens = estimateTokens(text);
    this.entries.push({ kind, text, createdAt: Date.now(), tokens });
    this.totalTokens += tokens;
    if (this.totalTokens >= config.visualMaxTokens) {
      this.trim();
    }
  }

  /**
   * Auto-eliminación a 50k: deja solo el último bloque y lo resume, para que
   * la ventana nunca crezca sin límite (el LLM no la ve).
   */
  private trim() {
    const resumen =
      `[historial recortado ${new Date().toISOString()}] ` +
      `${this.entries.length} entradas anteriores descartadas.`;
    this.entries = [{
      kind: "resultado", text: resumen, createdAt: Date.now(),
      tokens: estimateTokens(resumen),
    }];
    this.totalTokens = this.entries.reduce((a, e) => a + e.tokens, 0);
  }

  stats() {
    return {
      workerId: config.workerId,
      totalTokens: this.totalTokens,
      entries: this.entries.length,
      maxTokens: config.visualMaxTokens,
    };
  }

  lastActivity(): string | null {
    return this.entries.at(-1)?.text.slice(0, 200) ?? null;
  }
}

/** Instancia compartida de la ventana visual del worker. */
export const visual = new VisualWindow();