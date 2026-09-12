// workers-carlos-code: worker w6..w10 del sistema multiagente.
//
// Arranque: servidor HTTP (health/estado) + bucle de trabajo (long-poll a s1
// → ejecutar subtarea con OpenCode build → reportar fin → repetir).
import { config } from "./config";
import { error, info } from "./logger";
import { startServer } from "./server";
import { run } from "./worker";

async function main() {
  info("workers-carlos-code arrancando", {
    worker: config.workerId,
    puerto: config.port,
    modo: config.mockBuild ? "mock-build" : config.agentMode,
  });

  // Sobre cargar el worker, no es crítico conectar aquí: el ciclo de trabajo
  // habla con s1 (que sí lee la BD). La BD del worker (si hace falta en D2)
  // se conecta bajo demanda.

  const srv = startServer();

  // Bucle de trabajo en paralelo con el servidor HTTP.
  const workerPromise = run().catch((e) => {
    error("worker murió", { err: String(e) });
    process.exit(2);
  });

  for (const sig of ["SIGTERM", "SIGINT"] as const) {
    process.on(sig, async () => {
      info(`recibido ${sig}, apagando...`);
      srv.stop(true);
      process.exit(0);
    });
  }

  await workerPromise;
}

main().catch((e) => {
  error("arranque fallido", { err: String(e) });
  process.exit(1);
});