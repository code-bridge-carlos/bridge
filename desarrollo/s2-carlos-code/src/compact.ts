// Ventana de contexto del orquestador (sección de 200k tokens) con
// auto-compactación: cuando el total estimado alcanza CONTEXT_COMPACT_AT
// (170k), los segmentos más antiguos marcados como compactables se agrupan
// en un resumen sintético y el detalle se descarga a Supabase (descarga).
//
// Estimación de tokens: chars/4 (aprox. por token en código/inglés). Fina
// en C2 conectando con el conteo real de tokens del modelo (o se deja el
// ratio como constante configurable).
import { config } from "./config";

export type SegmentKind = "input" | "material" | "plan" | "resultado" | "tui";

export interface Segment {
  kind: SegmentKind;
  text: string;
  createdAt: number;
  tokens: number;
  /** true = candidato a compactar (materiales y resultados viejos). */
  compactable: boolean;
}

/** Estima tokens de un texto (chars/4, redondeando arriba). */
export function estimateTokens(text: string): number {
  return Math.ceil(text.length / 4);
}

export class ContextWindow {
  segments: Segment[] = [];
  totalTokens = 0;

  /** Añade un segmento; compacta si hace falta. */
  append(kind: SegmentKind, text: string, compactable = false) {
    const tokens = estimateTokens(text);
    this.segments.push({
      kind, text, createdAt: Date.now(), tokens, compactable,
    });
    this.totalTokens += tokens;
    if (this.totalTokens >= config.contextCompactAt) {
      return this.compact();
    }
    return null;
  }

  /**
   * Compactación: suma los segmentos compactables más antiguos hasta dejar
   * el total por debajo del umbral y devuelve el resumen creado (para
   * descargarlo a Supabase). El resto se mantiene intacto.
   */
  compact(): { resumen: string; descargados: number } | null {
    const sobre = this.totalTokens - config.contextCompactAt;
    if (sobre <= 0) return null;

    let aQuitar = 0;
    let out: Segment[] = [];
    for (const seg of this.segments) {
      if (seg.compactable && aQuitar < sobre + config.contextMaxTokens * 0.1) {
        out.push(seg);
        aQuitar += seg.tokens;
      }
    }
    if (out.length === 0) return null; // nada compactable: no compactar

    const resumen =
      `[compactado ${new Date().toISOString()}]\n` +
      out.map((s) => `- [${s.kind}] (${s.tokens} tok): ${s.text.slice(0, 140)}`).join("\n");
    this.segments = this.segments.filter((s) => !out.includes(s));
    this.totalTokens = this.segments.reduce((a, s) => a + s.tokens, 0);
    this.segments.push({
      kind: "resultado", text: resumen, createdAt: Date.now(),
      tokens: estimateTokens(resumen), compactable: false,
    });
    this.totalTokens += this.segments.at(-1)!.tokens;
    return { resumen, descargados: out.length };
  }

  /** Texto completo de la ventana (para el prompt al LLM). */
  render(): string {
    return this.segments.map((s) => `--- [${s.kind}] ---\n${s.text}`).join("\n\n");
  }

  stats() {
    return {
      totalTokens: this.totalTokens,
      segments: this.segments.length,
      maxTokens: config.contextMaxTokens,
      compactAt: config.contextCompactAt,
    };
  }
}