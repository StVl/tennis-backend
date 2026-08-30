-- Схема доставки пушей Live Activity (Task 2).
-- Канонические DDL живут в tennis-data-storage; здесь рабочая копия.
--
--   docker exec -i tennis-pg psql -U tennis -d tennis -v ON_ERROR_STOP=1 < db/live_push.sql

create table if not exists device_push_tokens (
  user_id    uuid        not null references profiles(user_id) on delete cascade,
  token      text        not null,
  -- Токен sandbox от боевого по виду не отличить, а отвечает на чужой хост
  -- BadDeviceToken. Без этой колонки причину ищут в коде, а она в окружении.
  env        text        not null,
  updated_at timestamptz not null,
  primary key (user_id, token),
  constraint device_push_tokens_env_check check (env in ('sandbox', 'production'))
);

create index if not exists device_push_tokens_user_idx on device_push_tokens (user_id);

-- Одна карточка одного пользователя по одному матчу.
--
-- Токенов два, и это не дублирование: push-to-start принадлежит приложению и
-- лежит в device_push_tokens, а update_token выдаётся iOS уже запущенной
-- активности и приходит от клиента отдельным запросом. Пока он не пришёл,
-- завершить карточку пушем нечем — отсюда фаза 'starting'.
create table if not exists live_activity_sessions (
  id           bigserial   primary key,
  user_id      uuid        not null references profiles(user_id) on delete cascade,
  match_id     bigint      not null references matches(id) on delete cascade,
  update_token text,
  phase        text        not null,
  started_at   timestamptz not null,
  ended_at     timestamptz,
  constraint live_activity_sessions_phase_check
    check (phase in ('starting', 'active', 'ended'))
);

-- Повторный push-to-start тому, у кого карточка уже висит, — дубль на локскрине.
create unique index if not exists live_activity_sessions_open_idx
  on live_activity_sessions (user_id, match_id) where ended_at is null;

create index if not exists live_activity_sessions_match_idx
  on live_activity_sessions (match_id) where ended_at is null;

-- Счётчик попыток на событии outbox'а: без него событие, которое падает
-- всегда, разбирается на каждом тике вечно.
alter table live_events add column if not exists attempts int not null default 0;
alter table live_events add column if not exists last_error text;
