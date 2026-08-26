-- Схема ingest'а live-статусов матчей.
-- Канонические DDL живут в tennis-data-storage (db/schema.sql); этот файл —
-- рабочая копия и то, что нужно туда перенести. См. docs/live-status-ingest.md.
--
-- Применить локально:
--   docker exec -i tennis-pg psql -U tennis -d tennis -v ON_ERROR_STOP=1 < db/live_ingest.sql
--
-- Правило владения: этот сервис пишет ТОЛЬКО matches.status (и то под guard'ом
-- по текущему значению). Счёт, победитель и сетка принадлежат пайплайну
-- tennis-data-storage. Всё приватное состояние ingest'а живёт в таблицах live_*,
-- а не колонками в matches — matches принадлежит другому репозиторию.

-- ---------------------------------------------------------------------------
-- Прогоны. Создаётся первым: на него ссылаются live_observations и live_flags.
-- ---------------------------------------------------------------------------

-- Одна строка на цикл (любого из двух джобов). Единственное окно в то,
-- почему карточка появилась или не появилась.
create table if not exists live_ingest_runs (
  id                      bigserial   primary key,
  -- 'live' | 'live-schedule'. Без этого поля нельзя ни посчитать квоту
  -- (запросы обоих джобов тратят один и тот же лимит), ни посчитать
  -- пропуски (прогон Job A отсутствием матча не является).
  job                     text        not null,
  source                  text        not null,
  started_at              timestamptz not null,
  finished_at             timestamptz,
  -- сколько строк вернул источник всего
  rows_parsed             int,
  -- сколько из них про наших игроков; именно этот счётчик защищает карточку,
  -- потому что борт источника — весь мир (ATP/WTA/Challenger/ITF)
  rows_in_scope           int,
  -- сколько удалось привязать к нашим матчам
  rows_matched            int,
  -- сколько отброшено молча (ни один игрок не наш) — не ошибка, а норма
  rows_dropped_unresolved int,
  -- запросов к источнику за этот цикл: с пагинацией и ретраями их больше одного,
  -- поэтому счётчик прогонов != счётчику запросов. Инкрементируется по ходу,
  -- а не на закрытии: иначе редеплой Railway теряет уже потраченное.
  requests_made           int         not null default 0,
  -- режим, в котором был принят решение опрашивать: active | watching |
  -- stale_safe | asleep. Пишется на КАЖДОМ тике, не только на пропуске.
  mode                    text,
  -- почему запрос не делали: asleep | too_soon | quota_exhausted | lock_held | disabled
  skipped_reason          text,
  error                   text
);

create index if not exists live_ingest_runs_job_idx
  on live_ingest_runs (job, started_at desc);
-- для суммы requests_made за сутки (квота считается по UTC)
create index if not exists live_ingest_runs_day_idx
  on live_ingest_runs (started_at);

-- ---------------------------------------------------------------------------
-- Наблюдения: append-only журнал того, что сказал источник.
-- ---------------------------------------------------------------------------

-- Статус НИКОГДА не пишется напрямую из ответа опроса — он выводится из этой
-- истории. Именно это делает джоб устойчивым к перезапускам и делает неверный
-- флип диагностируемым после факта.
create table if not exists live_observations (
  id          bigserial   primary key,
  -- прогон, в котором наблюдение получено. Отсутствие матча считается по
  -- прогонам, а не по времени: прогоны строго упорядочены, поэтому
  -- «три подряд» — точное утверждение, а не эвристика по таймстемпам.
  --
  -- Оба FK — on delete cascade, и это обязательно. Без каскада наша служебная
  -- таблица начинает БЛОКИРОВАТЬ чужие удаления: пайплайн не смог бы удалить
  -- матч или розыгрыш (удаление розыгрыша каскадится в matches и упиралось бы
  -- сюда), а чистка старых прогонов из Phase 9 упиралась бы в run_id.
  -- Ровно поэтому у external_ids.entity_id FK нет вообще: FK, который ломает
  -- импорт пайплайна, невосстановим, а висячий маппинг — восстановим.
  run_id      bigint      not null references live_ingest_runs(id) on delete cascade,
  match_id    bigint      not null references matches(id) on delete cascade,
  source      text        not null,
  -- 'on_court' | 'finished' | 'suspended'.
  -- Значения 'scheduled' здесь быть НЕ ДОЛЖНО: Job A в эту таблицу не пишет
  -- вообще. Если бы записал — сдвинул бы live_flags.last_seen_run_id и молча
  -- сбросил счётчик пропусков живого матча.
  state       text        not null,
  -- сырое event_status источника: его enum уже дрейфует (наблюдали "Finished",
  -- которого нет в документации), поэтому храним как есть для логов и тестов
  event_status text,
  observed_at timestamptz not null
);

create index if not exists live_observations_match_idx
  on live_observations (match_id, observed_at desc);

-- ---------------------------------------------------------------------------
-- Наши флаги: единственный источник истины о том, какие матчи мы держим live.
-- ---------------------------------------------------------------------------

-- Одна строка на матч, который прямо сейчас помечен НАМИ как live.
-- Полностью выводима из live_observations + live_ingest_runs (запрос
-- пересборки — в конце файла), то есть это материализация, а не второй
-- источник истины.
--
-- Почему отдельная таблица, а не колонки в matches: matches принадлежит
-- пайплайну tennis-data-storage. Приватное состояние этого сервиса в общем
-- контракте — это гарантированный дрейф.
create table if not exists live_flags (
  match_id         bigint         primary key references matches(id) on delete cascade,
  -- 'livetennisapi' | 'dev'. Ручные флипы из dev-эндпоинтов исключаются из
  -- прохода по пропускам, иначе матч, поднятый руками для теста iOS, молча
  -- вернётся назад через три цикла.
  source           text           not null,
  external_key     text,
  -- 'on_court' | 'suspended'. Нужно, чтобы дождь давал ОДНО событие
  -- suspended, а не по одному на каждый цикл в течение двух часов.
  state            text           not null,
  -- что восстановить на выходе. Под guard'ом входа (только из 'scheduled')
  -- здесь всегда 'scheduled'; колонка нужна для dev-флипов, которые guard
  -- обходят и поднимают завершённые матчи.
  prior_status     match_status_t not null,
  flipped_at       timestamptz    not null,
  -- последний прогон, в котором мы видели матч в борте. Пропуски считаются
  -- как число подходящих прогонов Job B с id больше этого.
  -- null допустим и означает «в опросе мы этот матч не видели ни разу»:
  -- так выглядят ручные флипы из dev-эндпоинтов, за которыми нет ни одного
  -- прогона. Проход по пропускам их и так исключает по source='dev'.
  last_seen_run_id bigint         references live_ingest_runs(id)
);

-- для аварийного выхода по максимальному возрасту live
create index if not exists live_flags_flipped_idx on live_flags (flipped_at);

-- ---------------------------------------------------------------------------
-- Расписание наших игроков, как его отдаёт источник.
-- ---------------------------------------------------------------------------

-- Кэш, а не истина: строки живут до следующего обновления и на matches не
-- влияют. Окно опроса выводится ИЗ ЭТОЙ таблицы, а не из v_tournament_editions:
-- в нашем календаре 25 розыгрышей с апреля по ноябрь против 60+ событий тура
-- с января, поэтому гейт по своему календарю уводил бы поллер в тишину
-- на целые месяцы — и молча.
create table if not exists live_schedule (
  source        text        not null,
  external_key  text        not null,        -- id матча у источника
  -- id турнира у источника. На борте upcoming он есть всегда (200/200 в срезе),
  -- на live-борте бывает null (9 из 19) — поэтому создание матчей (§8) на него
  -- опирается, а live-путь не имеет права.
  tournament_key text,
  -- null = порядок игры ещё не опубликован. Это РЕАЛЬНОЕ состояние, а не пробел:
  -- такую фикстуру смотрим весь день по event_date.
  scheduled_at  timestamptz,
  event_date    date,
  player_keys   text[]      not null,        -- id игроков у источника
  round_code    text,
  tournament    text,
  refreshed_at  timestamptz not null,
  primary key (source, external_key)
);

create index if not exists live_schedule_scheduled_idx on live_schedule (scheduled_at);
create index if not exists live_schedule_date_idx on live_schedule (event_date);

-- ---------------------------------------------------------------------------
-- Очередь ревью и исходящие события.
-- ---------------------------------------------------------------------------

-- Строки, которые резолвер не смог привязать. Очередь ревью, а не лог ошибок:
-- сюда попадает только то, где хотя бы один игрок наш. Если ни один не наш —
-- строка молча отбрасывается и лишь считается в rows_dropped_unresolved,
-- иначе таблица получала бы сотни строк за цикл и перестала быть очередью.
--
-- payload — уже ОЧИЩЕННАЯ проекция (external_key, player_keys, round_code,
-- scheduled_at, event_status), а не сырое тело ответа. Правило 1: ничего
-- похожего на счёт не должно попасть ни сюда, ни в лог, ни в ответ API.
create table if not exists live_unmatched (
  id          bigserial   primary key,
  source      text        not null,
  payload     jsonb       not null,
  reason      text        not null,
  observed_at timestamptz not null,
  constraint live_unmatched_reason_check check (reason in (
    'no_match_row', 'ambiguous', 'one_side_unresolved',
    'edition_unmapped', 'round_unmapped'
  ))
);

create index if not exists live_unmatched_observed_idx on live_unmatched (observed_at desc);

-- Outbox переходов. Не pg_notify: Railway редеплоится на каждый push,
-- LISTEN-соединения в этот момент умирают, и всё отправленное в зазор
-- исчезает без следа — ровно тот отказ, при котором у пользователя нет
-- карточки и нет способа узнать почему. Таблица переживает перезапуск,
-- даёт at-least-once с явным claim и воспроизводима.
create table if not exists live_events (
  id          bigserial   primary key,
  match_id    bigint      not null references matches(id) on delete cascade,
  event       text        not null,
  payload     jsonb       not null default '{}'::jsonb,
  reason      text,
  created_at  timestamptz not null,
  claimed_at  timestamptz,
  consumed_at timestamptz,
  constraint live_events_event_check check (event in (
    'live', 'finished', 'suspended', 'resumed'
  ))
);

-- частичный индекс: потребитель (Task 2) читает только необработанные
create index if not exists live_events_pending_idx
  on live_events (created_at) where consumed_at is null;

-- ---------------------------------------------------------------------------
-- Негативный кэш ленивого резолвера.
-- ---------------------------------------------------------------------------

-- Без него неизвестный id игрока запрашивался бы каждый цикл вечно. Holger Rune
-- — как раз такой случай: его нет в индексе источника вообще, то есть он
-- перезапрашивался бы бесконечно, тратя квоту на заведомо пустой ответ.
create table if not exists live_resolve_attempts (
  source        text        not null,
  external_key  text        not null,
  last_tried_at timestamptz not null,
  attempts      int         not null default 1,
  primary key (source, external_key)
);

-- ---------------------------------------------------------------------------
-- Восстановление live_flags из журнала (на случай потери материализации).
-- ---------------------------------------------------------------------------
--
-- Именно этот запрос делает утверждение «append-only журнал — источник истины»
-- проверяемым, а не декларативным:
--
--   select o.match_id, o.source, o.state, o.observed_at, o.run_id
--   from live_observations o
--   join (select match_id, max(id) as id from live_observations group by match_id) last
--     on last.id = o.id
--   join matches m on m.id = o.match_id
--   where o.state = 'on_court' and m.status = 'live';
--
-- prior_status при пересборке берётся как 'scheduled' (guard входа это
-- гарантирует), кроме флипов с source='dev'.
