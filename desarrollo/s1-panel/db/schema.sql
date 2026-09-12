-- ============================================================================
-- SISTEMA MULTIAGENTE — SCHEMA SUPABASE (PostgreSQL)
-- Fase 2 / Sub-fase B1: Base de datos
-- Ejecutar en el SQL Editor de Supabase o via psql con role service_key
-- ============================================================================

-- ============================================================================
-- 1. tasks — tarea global gestionada por s2 (plan → ejecución → fin)
-- ============================================================================
create table if not exists public.tasks (
  id           uuid primary key default gen_random_uuid(),
  prompt       text not null,
  estado       text not null default 'planificando'
               check (estado in ('planificando','ejecutando','completada','fallida')),
  plan_json    jsonb,
  created_at   timestamptz not null default now(),
  finished_at  timestamptz
);

create index if not exists idx_tasks_estado      on public.tasks (estado);
create index if not exists idx_tasks_created_at  on public.tasks (created_at desc);

-- C2: contador de reasignaciones de subtareas (límite de reintentos en s2).
alter table public.tasks
  add column if not exists intentos_reasignacion integer not null default 0;

-- ============================================================================
-- 2. subtask_queue — cola de subtareas gestionada por s1 (nunca en RAM)
-- ============================================================================
create table if not exists public.subtask_queue (
  id           uuid primary key default gen_random_uuid(),
  task_id      uuid not null references public.tasks(id) on delete cascade,
  worker_id    text,                -- w6..w10; null = sin asignar
  file_path    text,                -- archivo(s) objetivo de la subtarea
  prompt       text not null,       -- prompt específico preparado por s2
  contexto     text,                -- contexto completo (lo que s2 preparó)
  estado       text not null default 'pendiente'
               check (estado in ('pendiente','asignada','en_progreso',
                                 'completada','fallida','reasignada')),
  resultado    jsonb,               -- salida del worker: {exit, files, summary, ...}
  notificada_at timestamptz,        -- long-poll entregado (B4)
  created_at   timestamptz not null default now(),
  updated_at   timestamptz not null default now()
);

-- Columna notificada_at añadida en B4 (re-aplicaciones idempotentes)
alter table public.subtask_queue add column if not exists notificada_at timestamptz;

-- Columna s2_notified_at añadida (corrección A3): timestamp de la notificación
-- de fin de subtarea entregada a s2. NULL = pendiente de notificar; s1 la
-- reintenta (cola de reintentos en BD, nunca en RAM) hasta entregarla.
alter table public.subtask_queue add column if not exists s2_notified_at timestamptz;

-- Columna heartbeat_at añadida (corrección A4): última señal de vida del worker
-- mientras procesa la subtarea. NULL = el worker aún no reporta; se usa para
-- detección de colgadas (60s sin heartbeat → reasignar desde cero + restart).
alter table public.subtask_queue add column if not exists heartbeat_at timestamptz;

create index if not exists idx_subtask_estado  on public.subtask_queue (estado);
create index if not exists idx_subtask_worker  on public.subtask_queue (worker_id)
  where worker_id is not null;
create index if not exists idx_subtask_task    on public.subtask_queue (task_id);
create index if not exists idx_subtask_created on public.subtask_queue (created_at);
create index if not exists idx_subtask_s2_pend on public.subtask_queue (estado, s2_notified_at)
  where estado in ('completada','fallida') and s2_notified_at is null;
create index if not exists idx_subtask_heartbeat on public.subtask_queue (heartbeat_at)
  where estado in ('asignada','en_progreso');

-- ============================================================================
-- 3. services_status — estado de los 10 servicios (keep-alive de s1 escribe aquí)
-- ============================================================================
create table if not exists public.services_status (
  id_servicio  text primary key
               check (id_servicio in ('s1','s2','s3','s4','s5',
                                      'w6','w7','w8','w9','w10')),
  ram_mb       integer,
  estado       text not null default 'down'
               check (estado in ('up','down','starting')),
  url_base     text,                -- URL base del servicio (health-check)
  url_tui      text,                -- URL de la TUI (solo servicios con agente)
  latencia_ms  integer,             -- latencia del último ping keep-alive
  updated_at   timestamptz not null default now()
);

-- Seed inicial de servicios.
-- url_base: TODO servicio tiene una (para el health-check del keep-alive).
-- url_tui: SOLO los que ejecutan agentes y programan (s2 y w6-w10).
-- s1 es el panel web (no tiene terminal), s3/s4/s5 son infraestructura
-- (memoria/MCPs/reservado): solo status JSON, sin TUI.
insert into public.services_status (id_servicio, estado, url_base, url_tui) values
  ('s1','down','http://localhost:8080',null),
  ('s2','down','http://localhost:9002','http://localhost:9002/tui'),
  ('s3','down','http://localhost:9003',null),
  ('s4','down','http://localhost:9004',null),
  ('s5','down','http://localhost:9005',null),
  ('w6','down','http://localhost:9006','http://localhost:9006/tui'),
  ('w7','down','http://localhost:9007','http://localhost:9007/tui'),
  ('w8','down','http://localhost:9008','http://localhost:9008/tui'),
  ('w9','down','http://localhost:9009','http://localhost:9009/tui'),
  ('w10','down','http://localhost:9010','http://localhost:9010/tui')
on conflict (id_servicio) do nothing;

-- Re-aplicación: poblar url_base y url_tui de filas ya existentes.
-- url_base: todos. url_tui: solo s2 y w6-w10 (los que ejecutan agentes).
do $$
declare sv text; n int; base text; tui text;
begin
  foreach sv in array array['s1','s2','s3','s4','s5','w6','w7','w8','w9','w10'] loop
    if sv = 's1' then
      base := 'http://localhost:8080';
    else
      n := substr(sv, 2)::int;  -- 2..10
      base := 'http://localhost:' || (9000 + n);
    end if;
    update public.services_status
      set url_base = coalesce(url_base, base), url_tui = null
      where id_servicio = sv;
  end loop;
  -- url_tui solo para s2 y w6-w10
  foreach sv in array array['s2','w6','w7','w8','w9','w10'] loop
    if sv = 's2' then
      tui := 'http://localhost:9002/tui';
    else
      n := substr(sv, 2)::int;
      tui := 'http://localhost:' || (9000 + n) || '/tui';
    end if;
    update public.services_status
      set url_tui = coalesce(url_tui, tui)
      where id_servicio = sv;
  end loop;
  -- Limpiar url_tui de los que NO tienen TUI (s1, s3, s4, s5)
  update public.services_status set url_tui = null
    where id_servicio in ('s1','s3','s4','s5');
end;
$$;

-- ============================================================================
-- 4. tareas_panel — tareas escritas desde el botón "Tarea" de s1
-- ============================================================================
create table if not exists public.tareas_panel (
  id            uuid primary key default gen_random_uuid(),
  prompt        text not null,
  creada_at     timestamptz not null default now(),
  enviada_a_s2  boolean not null default false,
  subtask_count integer default 0,
  task_id       uuid references public.tasks(id) on delete set null
);

-- Columna task_id añadida en B3 (re-aplicaciones idempotentes)
alter table public.tareas_panel
  add column if not exists task_id uuid references public.tasks(id) on delete set null;

create index if not exists idx_tareas_panel_creada on public.tareas_panel (creada_at desc);

-- ============================================================================
-- 5. engram_observations — memoria persistente de s3 (Engram)
-- ============================================================================
create table if not exists public.engram_observations (
  id           uuid primary key default gen_random_uuid(),
  title        text not null,
  content      text not null,       -- formato: What/Why/Where/Learned
  type         text default 'manual',
  project      text,
  scope        text default 'project',
  topic_key    text,
  created_at   timestamptz not null default now(),
  updated_at   timestamptz not null default now()
);

create index if not exists idx_engram_project on public.engram_observations (project);
create index if not exists idx_engram_topic   on public.engram_observations (topic_key);
create index if not exists idx_engram_created on public.engram_observations (created_at desc);

-- Búsqueda full-text para mem_search (FTS5 en SQLite local; aquí tsvector)
alter table public.engram_observations
  add column if not exists search_tsv tsvector
  generated always as (
    to_tsvector('simple', coalesce(title,'') || ' ' || coalesce(content,''))
  ) stored;

create index if not exists idx_engram_search on public.engram_observations
  using gin (search_tsv);

-- ============================================================================
-- 6. proxy_pool — pool de proxies obtenidos de proxmint/free-proxy-list
-- ============================================================================
create table if not exists public.proxy_pool (
  id           uuid primary key default gen_random_uuid(),
  proxy_url    text not null unique,  -- ej: http://1.2.3.4:8080 o 1.2.3.4:8080
  status       text not null default 'libre'
               check (status in ('libre','asignado','fallido')),
  last_checked timestamptz,
  source       text default 'proxmint/free-proxy-list'
);

-- Columnas B5 (clasificación con verificación real). Re-aplicación idempotente.
alter table public.proxy_pool add column if not exists tipo        text    not null default 'http';
alter table public.proxy_pool add column if not exists clasificacion text not null default 'nuevo'
  check (clasificacion in ('nuevo','validado','descartado'));
alter table public.proxy_pool add column if not exists ip_vista    text;
alter table public.proxy_pool add column if not exists oculta      boolean;     -- ip_vista != ip del servidor
alter table public.proxy_pool add column if not exists latencia_ms integer;
alter table public.proxy_pool add column if not exists motivo      text;        -- por qué se descartó

create index if not exists idx_proxy_pool_status on public.proxy_pool (status);
create index if not exists idx_proxy_pool_clasif on public.proxy_pool (clasificacion) where clasificacion='validado';

-- ============================================================================
-- 7. proxy_assignments — asignación proxy → servicio (auto cada hora / manual)
-- ============================================================================
create table if not exists public.proxy_assignments (
  id           uuid primary key default gen_random_uuid(),
  servicio     text not null references public.services_status(id_servicio) on delete cascade,
  proxy_url    text not null,
  asignado_at  timestamptz not null default now(),
  origen       text not null default 'auto'
               check (origen in ('auto','manual')),
  activo       boolean not null default true
);

-- Columnas B5: confirmación del target y retirada (estados OK / reserva / liberada).
alter table public.proxy_assignments add column if not exists confirmado   boolean not null default false;
alter table public.proxy_assignments add column if not exists confirmado_at timestamptz;
alter table public.proxy_assignments add column if not exists retirado_at   timestamptz;

create index if not exists idx_proxy_assign_servicio on public.proxy_assignments (servicio)
  where activo;
create index if not exists idx_proxy_assign_url on public.proxy_assignments (proxy_url);

-- ============================================================================
-- 8. tui_access_tokens — tokens temporales emitidos por s1 para acceder a TUIs
-- ============================================================================
create table if not exists public.tui_access_tokens (
  id           uuid primary key default gen_random_uuid(),
  token_hash   text not null unique,   -- SHA-256 del token (nunca el token en crudo)
  servicio     text not null references public.services_status(id_servicio) on delete cascade,
  usuario      text not null default 'admin',
  expira_at    timestamptz not null,
  usado        boolean not null default false,
  created_at   timestamptz not null default now()
);

create index if not exists idx_tui_tokens_expira on public.tui_access_tokens (expira_at);
create index if not exists idx_tui_tokens_usado  on public.tui_access_tokens (usado)
  where not usado;

-- ============================================================================
-- 9. settings — configuración clave-valor del sistema (toggle keep-alive, etc.)
-- ============================================================================
create table if not exists public.settings (
  key        text primary key,
  value      text,
  updated_at timestamptz not null default now()
);

insert into public.settings (key, value) values ('keep_alive', 'off')
on conflict (key) do nothing;

-- ============================================================================
-- Trigger genérico updated_at
-- ============================================================================
create or replace function public.set_updated_at()
returns trigger language plpgsql as $$
begin
  new.updated_at = now();
  return new;
end;
$$;

do $$
begin
  if not exists (select 1 from pg_trigger where tgname = 'trg_subtask_updated_at') then
    create trigger trg_subtask_updated_at
      before update on public.subtask_queue
      for each row execute function public.set_updated_at();
  end if;
end;
$$;

do $$
begin
  if not exists (select 1 from pg_trigger where tgname = 'trg_engram_updated_at') then
    create trigger trg_engram_updated_at
      before update on public.engram_observations
      for each row execute function public.set_updated_at();
  end if;
end;
$$;

do $$
begin
  if not exists (select 1 from pg_trigger where tgname = 'trg_settings_updated_at') then
    create trigger trg_settings_updated_at
      before update on public.settings
      for each row execute function public.set_updated_at();
  end if;
end;
$$;

-- ============================================================================
-- NOTAS DE SEGURIDAD
-- - Los servicios backend (s1, s2, s3, workers) usan la service_role key de
--   Supabase. NUNCA exponer la anon key con permisos de escritura.
-- - RLS (Row Level Security) se deshabilita por defecto; si se activa,
--   crear políticas que permitan acceso solo al rol service_role.
-- - Los tokens TUI se guardan SOLO como hash SHA-256; el token en crudo
--   viaja únicamente en la respuesta HTTP de emisión (s1 → usuario).
-- ============================================================================