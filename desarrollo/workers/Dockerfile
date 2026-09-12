# Workers w6-w10 (mismo artefacto, WORKER_ID por env). Base oven/bun slim.
FROM oven/bun:1-slim

WORKDIR /app

COPY package.json bun.lock ./
RUN bun install --frozen-lockfile --production

COPY agent-worker.md ./
COPY src ./src

# Render inyecta PORT y WORKER_ID (w6..w10). Sin model, con worker headless
# en :900N para health checks del keep-alive de s1.
ENV NODE_ENV=production
EXPOSE 9000
ENV PORT=9000
CMD ["sh", "-c", "bun run src/index.ts"]