-- Маппинг турниров Live Tennis API -> наши tournament_editions.
-- Владелец — этот сервис; см. db/live_ingest.sql про разделение владения.
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

-- ЧТО ИМЕННО ИДЕНТИФИЦИРУЕТ id ТУРНИРА У ИСТОЧНИКА.
-- Каталог /tournaments отдаёт РОВНО ОДИН id на турнир в пределах тура, а
-- разные туры и разряды одного события — это разные id:
--
--   US Open 1217 = atp singles, 1218 = wta singles, 1221 = doubles
--   Cincinnati   1209 = wta singles, 1210 = atp singles
--
-- Квалификация отдельного id НЕ имеет: под 1217 приходят и is_qualifying=true,
-- и is_qualifying=false (проверено на живой ленте 2026-08-28: 16 строк atp
-- singles под 1217, из них 6 — основная сетка).
--
-- ОТСЮДА ГЛАВНОЕ: «id встречался в ленте» НЕ подтверждает, что это наш турнир.
-- Оно подтверждает только, что id существует. У нас календарь ATP (одиночки),
-- поэтому подтверждением служит каталог, отфильтрованный по туру:
--   GET /tournaments?tour=atp&draw=singles&limit=200
-- Снимок: db/live_tournaments_catalogue.json (198 строк,
-- has_more=false, один запрос).
--
-- Предыдущая версия этого файла держала 1209 (wta Cincinnati) и 1218 (wta
-- US Open) как ПОДТВЕРЖДЁННЫЕ — именно потому, что они встречались в ленте.
-- Ущерба не было только потому, что писатель требует, чтобы разрешились ОБА
-- игрока, а в players у нас лишь ATP; то есть спасала не проверка, а
-- случайность. Такие id здесь больше не лежат.
--
-- Связь остаётся N:1 (много внешних ключей на один наш розыгрыш) — как только
-- у розыгрыша найдётся второй легитимный ATP-id, он встанет рядом. Индекс по
-- (source, entity_type, entity_id) не превращать в unique.
--
-- Сид заведомо НЕПОЛОН, и это нормально. Незнакомый id турнира отправляет
-- фикстуру в live_unmatched с reason='edition_unmapped' — это и есть очередь,
-- по которой файл пополняется. Догадок здесь нет и быть не должно: неверно
-- угаданный розыгрыш создаёт матч в чужой сетке.

insert into external_ids (source, entity_type, external_key, entity_id, confirmed_at)
select 'livetennisapi', 'edition', v.external_key, te.id, now()
from (values
  -- Все пять — из каталога с tour=atp&draw=singles, то есть подтверждено, что
  -- это наш турнир, а не одноимённое событие другого тура.
  ('1217', 'us_open_2026'),       -- в каталоге «US Open», grand_slam
  ('1210', 'cincinnati_2026'),    -- «Cincinnati», masters_1000
  ('1667', 'shanghai_2026'),      -- «Shanghai»; «Shanghai 2» (3700) — ДРУГОЙ турнир
  ('1725', 'paris_masters_2026'), -- «Paris», masters_1000
  ('2794', 'atp_finals_2026')     -- «Finals - Turin», tour_finals
) as v(external_key, edition_slug)
join tournament_editions te on te.slug = v.edition_slug
-- do update, а не do nothing: этот файл и есть источник истины по маппингу,
-- и повторный прогон должен подтягивать изменения, а не игнорировать их.
-- coalesce, а не excluded: файл применяется на каждом старте сервиса, и без
-- этого отметка подтверждения переставлялась бы на время последнего деплоя.
-- Первое подтверждение сохраняется; неподтверждённый id подтвердить можно.
on conflict (source, entity_type, external_key) do update
  set entity_id = excluded.entity_id,
      confirmed_at = coalesce(external_ids.confirmed_at, excluded.confirmed_at);

-- Снятые ранее строки. 1209 и 1218 — женские сетки, 1221 — парная; 1211, 8656,
-- 1726 и 2795 в ATP-каталоге отсутствуют и, судя по разбивке выше, тоже
-- принадлежат другому туру или разряду. Отсутствующий id безопаснее неверного:
-- он приведёт фикстуру в live_unmatched, а неверный — в чужую сетку.
delete from external_ids
where source = 'livetennisapi' and entity_type = 'edition'
  and external_key in ('1209', '1211', '1218', '1219', '1221', '1726', '2795', '8656');

-- Что получилось
select e.external_key, te.slug, t.name, e.confirmed_at is not null as confirmed
from external_ids e
join tournament_editions te on te.id = e.entity_id
join tournaments t on t.id = te.tournament_id
where e.source = 'livetennisapi' and e.entity_type = 'edition'
order by te.start_date, e.external_key;
