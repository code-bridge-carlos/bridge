# s2-carlos-code — orquestador (OpenCode headless + Gentle AI)
# Imagen base Bun; en Render se arranca con `bun src/index.ts`.
FROM oven/bun:1

WORKDIR /app

COPY package.json bun.lock* ./
RUN bun install --frozen-lockfile --production

COPY src ./src
COPY agent-orquestador.md ./agent-orquestador.md

ENV PORT=9002
ENV AGENT_MODE=plan
ENV CONTEXT_MAX_TOKENS=200000
ENV CONTEXT_COMPACT_AT=170000

EXPOSE 9002

CMD ["bun", "src/index.ts"]