-- ЛОКАЛЬНАЯ РАЗРАБОТКА. НЕ ПЕРЕНОСИТЬ в tennis-data-storage.
--
-- Зачем: в базе 481 матч и все они completed, ни одной строки со status='scheduled'
-- не было никогда. Флипать нечего, поэтому Phase 1..7 нечем проверить.
-- Этот файл создаёт пару синтетических scheduled-матчей, на которых работают
-- ручные проверки из плана.
--
-- Применить:
--   docker exec -i tennis-pg psql -U tennis -d tennis -v ON_ERROR_STOP=1 < db/dev_fixtures.sql
-- Убрать:
--   delete from matches where import_key like 'devfix_%';   -- participants уйдут по FK
--
-- Идемпотентно: import_key уникален, повторный прогон ничего не меняет.
-- scheduled_at задаётся относительно now(), чтобы фикстура не устаревала и
-- всегда попадала в окно WATCHING. Правило «никаких now() в SQL этой фичи»
-- относится к боевому коду, а не к dev-скрипту.

begin;

-- Розыгрыш us_open_2026 существует (66 tournament_entries, 0 матчей), а раунд
-- R128 есть в rounds — matches.round_code это FK, произвольный код не вставить.
with edition as (
  select id from tournament_editions where slug = 'us_open_2026'
),
fixtures(import_key, round_code, offset_hours, slug_a, slug_b) as (
  values
    ('devfix_uso_r128_a', 'R128', 2, 'sinner',   'alcaraz'),
    ('devfix_uso_r128_b', 'R128', 4, 'djokovic', 'zverev')
),
inserted as (
  insert into matches (edition_id, round_code, scheduled_at, status, import_key)
  select e.id, f.round_code, now() + make_interval(hours => f.offset_hours),
         'scheduled', f.import_key
  from fixtures f cross join edition e
  on conflict (import_key) do nothing
  returning id, import_key
)
insert into match_participants (match_id, side, slot, player_id)
select i.id, s.side, 1, p.id
from inserted i
join fixtures f on f.import_key = i.import_key
cross join lateral (values (1, f.slug_a), (2, f.slug_b)) as s(side, slug)
join players p on p.slug = s.slug
on conflict do nothing;

commit;

-- Что получилось
select m.id, m.import_key, m.status, m.round_code, m.scheduled_at,
       string_agg(p.slug, ' vs ' order by mp.side) as players
from matches m
join match_participants mp on mp.match_id = m.id
join players p on p.id = mp.player_id
where m.import_key like 'devfix_%'
group by m.id, m.import_key, m.status, m.round_code, m.scheduled_at
order by m.scheduled_at;
