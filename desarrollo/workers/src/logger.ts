// Logging compacto a stdout (lo captura Render / /tmp/opencode en dev).
import { config } from "./config";

export function log(level: "info" | "warn" | "error", msg: string, extra?: Record<string, unknown>) {
  const line = `[${config.workerId}] ${new Date().toISOString()} ${level}: ${msg}` +
    (extra && Object.keys(extra).length ? " " + JSON.stringify(extra) : "");
  if (level === "error") console.error(line);
  else console.log(line);
}

export const info = (m: string, e?: Record<string, unknown>) => log("info", m, e);
export const warn = (m: string, e?: Record<string, unknown>) => log("warn", m, e);
export const error = (m: string, e?: Record<string, unknown>) => log("error", m, e);