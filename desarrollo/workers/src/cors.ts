// CORS controlado (F1): allowlist explícita + preflight OPTIONS.
import { config } from "./config";

const CURRENT_ORIGIN = new URL(config.s1Url).origin;

const ALLOWED_ORIGINS = new Set<string>([
  CURRENT_ORIGIN,
  "http://localhost:8080",
  "http://localhost:9002",
  "http://localhost:9006",
  "http://localhost:9007",
  "http://localhost:9008",
  "http://localhost:9009",
  "http://localhost:9010",
  "https://s1-panel.onrender.com",
]);

export function allowedOrigin(origin: string | null): string {
  if (!origin) return "";
  try {
    const o = new URL(origin).origin;
    if (o === "null") return "";
    return ALLOWED_ORIGINS.has(o) ? o : "";
  } catch {
    return "";
  }
}

export function applyCORS(headers: Headers, originHeader: string | null): void {
  const origin = allowedOrigin(originHeader);
  if (origin) {
    headers.set("Access-Control-Allow-Origin", origin);
    headers.set("Vary", "Origin");
  }
  headers.set("Access-Control-Allow-Methods", "GET, POST, OPTIONS");
  headers.set("Access-Control-Allow-Headers", "Content-Type, X-Control-Token, Authorization");
  headers.set("Access-Control-Max-Age", "600");
}

export function isPreflight(req: Request): boolean {
  return req.method === "OPTIONS";
}

export function preflightResponse(originHeader: string | null): Response {
  const h = new Headers();
  applyCORS(h, originHeader);
  return new Response(null, { status: 204, headers: h });
}

export function withCORS(res: Response, originHeader: string | null): Response {
  const headers = new Headers(res.headers);
  applyCORS(headers, originHeader);
  return new Response(res.body, { status: res.status, statusText: res.statusText, headers });
}