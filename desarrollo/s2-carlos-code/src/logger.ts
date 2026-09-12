// Logger mínimo de s2: prefijo [s2] + nivel + JSON opcional.
export function log(level: "info" | "warn" | "error", msg: string, extra?: unknown) {
  const line = `[s2] ${new Date().toISOString()} ${level}: ${msg}`;
  if (extra !== undefined) {
    console.log(line, JSON.stringify(extra));
  } else {
    console.log(line);
  }
}

export const info = (m: string, x?: unknown) => log("info", m, x);
export const warn = (m: string, x?: unknown) => log("warn", m, x);
export const error = (m: string, x?: unknown) => log("error", m, x);