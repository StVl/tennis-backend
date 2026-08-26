-- Маппинг турниров Live Tennis API -> наши tournament_editions.
-- Канонические DDL живут в tennis-data-storage; здесь рабочая копия и сид.
--
-- Применить:
--   docker exec -i tennis-pg psql -U tennis -d tennis -v ON_ERROR_STOP=1 < db/live_edition_ids.sql
--
-- Зачем: matches.edition_id — not null с внешним ключом, поэтому создать строку
-- матча из фикстуры (Phase 8) нельзя, не зная, к какому нашему розыгрышу она
-- относится. Резолвим строго по id источника; по названиям НЕ матчим — у нас
-- они спонсорские ("Western & Southern Open", "Rolex Paris Masters"), у
-- источника городские ("Cincinnati", "Paris"), и это ровно то нечёткое
-- сопоставление, ради устранения которого вся схема с external_ids и заведена.
--
-- ВАЖНО ПРО ФОРМУ ЭТОГО МАППИНГА. У источника id турнира — НЕ один на розыгрыш:
--   * в каталоге /tournaments на каждый турнир приходится пара id
--     (Cincinnati 1210/1211, Paris 1725/1726, ATP Finals 2794/2795);
--   * в реальных ответах по матчам встречаются ДРУГИЕ id того же турнира
--     (Cincinnati 1209, US Open 1217/1218/1221) — каталожные и «боевые» id
--     не совпадают;
--   * один id может нести и основную сетку, и квалификацию сразу
--     (Winston-Salem 1214 отдаёт и is_qualifying=false, и true).
-- Поэтому связь N:1, как и у игроков: много внешних ключей на один наш
-- розыгрыш. Не превращать индекс в unique.
--
-- ОСНОВНАЯ СЕТКА US OPEN ЕЩЁ НЕ ОТОБРАЖЕНА. Во всех снятых срезах у US Open
-- встречались только 1217 и 1218 (квалификация, 129 строк) и 1221 (пара).
-- Писатель квалификацию пропускает, разбор отбрасывает пары — значит при
-- включённом LIVE_CREATE_MATCHES во время US Open не создастся НИЧЕГО, и
-- выглядеть это будет как сломанный писатель. На самом деле id основной сетки
-- появится в live_unmatched с reason='edition_unmapped' на первом же
-- обновлении расписания после публикации сетки; добавьте его сюда строкой.
--
-- Сид заведомо НЕПОЛОН, и это нормально. Незнакомый id турнира отправляет
-- фикстуру в live_unmatched с reason='edition_unmapped' — это и есть очередь,
-- по которой файл пополняется. Догадок здесь нет и быть не должно: неверно
-- угаданный розыгрыш создаёт матч в чужой сетке.

-- confirmed_at заполнен ТОЛЬКО у id, реально наблюдавшихся в ответах по матчам.
-- Остальные взяты из каталога /tournaments: турнир они называют верно, но роль
-- каждого id (основная сетка / квалификация / пара) каталог не сообщает, а
-- угадывать её — ровно то, чего этот файл делать не должен. Такие строки
-- работают, но помечены как ждущие подтверждения.
insert into external_ids (source, entity_type, external_key, entity_id, confirmed_at)
select 'livetennisapi', 'edition', v.external_key, te.id,
       case when v.observed then now() end
from (values
  -- US Open. 1217 и 1218 наблюдались на квалификации, 1221 — парный разряд;
  -- парные до писателя матчей не доходят (отсекаются на разборе), а
  -- квалификация отсекается самим писателем.
  ('1217', 'us_open_2026', true),
  ('1218', 'us_open_2026', true),
  -- 1219 — из каталога, в ответах по матчам не встречался ни разу.
  ('1219', 'us_open_2026', false),
  ('1221', 'us_open_2026', true),
  -- Cincinnati: 1209 из ответов по матчам, 1210/1211 из каталога
  ('1209', 'cincinnati_2026', true),
  ('1210', 'cincinnati_2026', false),
  ('1211', 'cincinnati_2026', false),
  -- Shanghai. «Shanghai 2» (3700/9189) сюда НЕ входит: это отдельный турнир
  -- в том же городе, а не второй id того же самого.
  ('1667', 'shanghai_2026', false),
  ('8656', 'shanghai_2026', false),
  -- Rolex Paris Masters
  ('1725', 'paris_masters_2026', false),
  ('1726', 'paris_masters_2026', false),
  -- Nitto ATP Finals (у источника «Finals - Turin»)
  ('2794', 'atp_finals_2026', false),
  ('2795', 'atp_finals_2026', false)
) as v(external_key, edition_slug, observed)
join tournament_editions te on te.slug = v.edition_slug
on conflict (source, entity_type, external_key) do nothing;

-- Что получилось
select e.external_key, te.slug, t.name
from external_ids e
join tournament_editions te on te.id = e.entity_id
join tournaments t on t.id = te.tournament_id
where e.source = 'livetennisapi' and e.entity_type = 'edition'
order by te.start_date, e.external_key;
