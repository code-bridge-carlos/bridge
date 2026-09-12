// s2-carlos-code: orquestador del sistema multiagente.
//
// Arranque: configuración → conexión BD → servidor HTTP. El orquestador
// trabaja en modo plan permanente (AGENT_MODE=plan → agent-orquestador.md)
// sobre OpenCode headless; en C1 la emisión de subtareas es dev-only.
import { config } from "./config";
import { db, health } from "./db";
import { error, info } from "./logger";
import { buildServer } from "./server";

async function main() {
  info("s2-carlos-code arrancando", {
    agente: config.agentMode,
    ventana: { max: config.contextMaxTokens, compactaEn: config.contextCompactAt },
    emitOnTask: config.emitOnTask,
  });

  // Conexión a la BD compartida (falla alto con mensaje claro).
  try {
    if (!(await health())) throw new Error("SELECT 1 falló");
    info("base de datos compartida OK");
  } catch (e) {
    error("no pude conectar a la base de datos", { err: String(e) });
    process.exit(1);
  }

  const server = buildServer();
  info(`escuchando en http://localhost:${server.port}`);

  // Shutdown limpio (SIGTERM/SIGINT — Render envía SIGTERM).
  for (const sig of ["SIGTERM", "SIGINT"] as const) {
    process.on(sig, async () => {
      info(`recibido ${sig}, apagando...`);
      server.stop();
      await db().end({ timeout: 2 });
      process.exit(0);
    });
  }
}

main().catch((e) => {
  error("arranque fallido", { err: String(e) });
  process.exit(1);
});