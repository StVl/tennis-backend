-- Маппинг игроков Live Tennis API -> наши players.
-- Таблицей external_ids и этим сидом владеет ЭТОТ сервис — см. db/live_ingest.sql
-- про разделение владения со схемой контента.
--
-- confirmed_at = null означает «угадано машиной, ждёт ревью» (см. docs/live-status-ingest.md).
-- Источник ставит НЕСКОЛЬКО своих id на одного игрока (дубли в его индексе:
-- Tabur = 175 и 8824, Misolic = 13409 и 14985), поэтому связь
-- external_key -> entity_id именно N:1, а не 1:1. Не превращать индекс в unique.
--
-- Сид идёт через slug, а не через players.id: id — generated always as identity,
-- то есть зависит от порядка вставки и на проде может отличаться. slug уникален
-- и стабилен, поэтому файл можно безопасно прогнать в любом окружении.
-- Игроки, которых нет в целевой базе, молча пропускаются (join, а не values).

create table if not exists external_ids (
  source       text   not null,          -- 'livetennisapi'
  entity_type  text   not null,          -- 'player' | 'edition' | 'match'
  external_key text   not null,          -- id на стороне источника
  entity_id    bigint not null,          -- наш id
  confirmed_at timestamptz,              -- null = машинная догадка
  primary key (source, entity_type, external_key)
);

-- один наш игрок может иметь несколько внешних ключей, поэтому индекс, а не unique
create index if not exists external_ids_entity_idx
  on external_ids (source, entity_type, entity_id);

insert into external_ids (source, entity_type, external_key, entity_id, confirmed_at)
select 'livetennisapi', 'player', v.external_key, p.id,
       case when v.confirmed then now() end
from (values
    ('13', 'alcaraz', true),                     -- tracked Carlos Alcaraz  [full_name]
    ('104', 'altmaier', true),                   -- tracked Daniel Altmaier  [full_name]
    ('32', 'atmane', true),                      -- tracked Terence Atmane  [full_name]
    ('31', 'auger_aliassime', true),             -- tracked Felix Auger-Aliassime  [full_name]
    ('1615', 'baez', true),                      -- tracked Sebastián Báez  [full_name]
    ('200', 'bautista_agut', true),              -- tracked Roberto Bautista-Agut  [api_search]
    ('225', 'bellucci', true),                   -- tracked Mattia Bellucci  [full_name]
    ('528', 'bergs', true),                      -- tracked Zizou Bergs  [full_name]
    ('25', 'berrettini', true),                  -- tracked Matteo Berrettini  [full_name]
    ('526', 'blockx', true),                     -- tracked Alexander Blockx  [full_name]
    ('503', 'borges', true),                     -- tracked Nuno Borges  [full_name]
    ('192', 'brooksby', true),                   -- tracked Jenson Brooksby  [full_name]
    ('654', 'bublik', true),                     -- tracked Alexander Bublik  [full_name]
    ('109', 'burruchaga', true),                 -- tracked Roman Andres Burruchaga  [last_name]
    ('228', 'buse', true),                       -- tracked Ignacio Buse  [full_name]
    ('97', 'carreno_busta', true),               -- tracked Pablo Carreno-Busta  [full_name]
    ('10', 'cazaux', true),                      -- tracked Arthur Cazaux  [full_name]
    ('8', 'cilic', true),                        -- tracked Marin Cilic  [full_name]
    ('652', 'cobolli', true),                    -- tracked Flavio Cobolli  [full_name]
    ('20', 'collignon', true),                   -- tracked Raphael Collignon  [full_name]
    ('521', 'comesana', true),                   -- tracked Francisco Comesana  [full_name]
    ('506', 'darderi', true),                    -- tracked Luciano Darderi  [full_name]
    ('1006', 'davidovich_fokina', true),         -- tracked Alejandro Davidovich Fokina  [full_name]
    ('229', 'de_jong', true),                    -- tracked Jesper De Jong  [full_name]
    ('653', 'de_minaur', true),                  -- tracked Alex De Minaur  [full_name]
    ('3', 'diallo', true),                       -- tracked Gabriel Diallo  [full_name]
    ('529', 'dimitrov', true),                   -- tracked Grigor Dimitrov  [full_name]
    ('1218', 'djokovic', true),                  -- tracked Novak Djokovic  [full_name]
    ('812', 'draper', true),                     -- tracked Jack Draper  [full_name]
    ('64', 'duckworth', true),                   -- tracked James Duckworth  [full_name]
    ('204', 'dzumhur', true),                    -- tracked Damir Dzumhur  [full_name]
    ('24', 'etcheverry', true),                  -- tracked Tomas Martin Etcheverry  [full_name]
    ('1', 'f_cerundolo', true),                  -- tracked Francisco Cerundolo  [full_name]
    ('1179', 'fearnley', true),                  -- tracked Jacob Fearnley  [full_name]
    ('27', 'fils', true),                        -- tracked Arthur Fils  [full_name]
    ('530', 'fonseca', true),                    -- tracked Joao Fonseca  [full_name]
    ('18', 'fritz', true),                       -- tracked Taylor Fritz  [full_name]
    ('531', 'fucsovics', true),                  -- tracked Marton Fucsovics  [full_name]
    ('524', 'garin', true),                      -- tracked Cristian Garin  [full_name]
    ('990', 'giron', true),                      -- tracked Marcos Giron  [full_name]
    ('512', 'griekspoor', true),                 -- tracked Tallon Griekspoor  [full_name]
    ('29', 'halys', true),                       -- tracked Quentin Halys  [full_name]
    ('230', 'hanfmann', true),                   -- tracked Yannick Hanfmann  [full_name]
    ('4', 'humbert', true),                      -- tracked Ugo Humbert  [full_name]
    ('532', 'hurkacz', true),                    -- tracked Hubert Hurkacz  [full_name]
    ('1002', 'jm_cerundolo', true),              -- tracked Juan Manuel Cerundolo  [full_name]
    ('23', 'jodar', true),                       -- tracked Rafael Jodar  [full_name]
    ('536', 'kecmanovic', true),                 -- tracked Miomir Kecmanovic  [full_name]
    ('16', 'khachanov', true),                   -- tracked Karen Khachanov  [full_name]
    ('1632', 'kopriva', true),                   -- tracked Vit Kopřiva  [full_name]
    ('14', 'korda', true),                       -- tracked Sebastian Korda  [full_name]
    ('189', 'kovacevic', true),                  -- tracked Aleksandar Kovacevic  [full_name]
    ('195', 'kypson', true),                     -- tracked Patrick Kypson  [full_name]
    ('22', 'lehecka', true),                     -- tracked Jiri Lehecka  [full_name]
    ('507', 'machac', true),                     -- tracked Tomas Machac  [full_name]
    ('30', 'majchrzak', true),                   -- tracked Kamil Majchrzak  [full_name]
    ('508', 'mannarino', true),                  -- tracked Adrian Mannarino  [full_name]
    ('504', 'marozsan', true),                   -- tracked Fabian Marozsan  [full_name]
    ('110', 'medjedovic', true),                 -- tracked Hamad Medjedovic  [full_name]
    ('37', 'medvedev', true),                    -- tracked Daniil Medvedev  [full_name]
    ('8730', 'mensik', true),                    -- tracked Jakub Menšik  [fuzzy:1.00]
    ('36', 'michelsen', true),                   -- tracked Alex Michelsen  [full_name]
    ('13409', 'misolic', true),                  -- tracked Filip Misolic  [api_search]
    ('33', 'moutet', true),                      -- tracked Corentin Moutet  [full_name]
    ('1215', 'mpetshi_perricard', true),         -- tracked Giovanni Mpetshi Perricard  [full_name]
    ('91', 'muller', true),                      -- tracked Alexandre Muller  [full_name]
    ('813', 'munar', true),                      -- tracked Jaume Munar  [full_name]
    ('664', 'musetti', true),                    -- tracked Lorenzo Musetti  [full_name]
    ('7', 'nakashima', true),                    -- tracked Brandon Nakashima  [full_name]
    ('208', 'navone', true),                     -- tracked Mariano Navone  [full_name]
    ('535', 'norrie', true),                     -- tracked Cameron Norrie  [full_name]
    ('124', 'ofner', true),                      -- tracked Sebastian Ofner  [full_name]
    ('17', 'opelka', true),                      -- tracked Reilly Opelka  [full_name]
    ('19', 'paul', true),                        -- tracked Tommy Paul  [full_name]
    ('194', 'popyrin', true),                    -- tracked Alexei Popyrin  [full_name]
    ('21', 'quinn', true),                       -- tracked Ethan Quinn  [full_name]
    ('538', 'rinderknech', true),                -- tracked Arthur Rinderknech  [full_name]
    ('411', 'royer', true),                      -- tracked Valentin Royer  [full_name]
    ('539', 'rublev', true),                     -- tracked Andrey Rublev  [full_name]
    ('537', 'ruud', true),                       -- tracked Casper Ruud  [full_name]
    ('651', 'shapovalov', true),                 -- tracked Denis Shapovalov  [full_name]
    ('11', 'shelton', true),                     -- tracked Ben Shelton  [full_name]
    ('12', 'shevchenko', true),                  -- tracked Alexander Shevchenko  [full_name]
    ('34', 'sinner', true),                      -- tracked Jannik Sinner  [full_name]
    ('884', 'sonego', true),                     -- tracked Lorenzo Sonego  [full_name]
    ('1644', 'spizzirri', true),                 -- tracked Eliot Spizzirri  [full_name]
    ('525', 'struff', true),                     -- tracked Jan-Lennard Struff  [full_name]
    ('198', 'svajda', true),                     -- tracked Zachary Svajda  [full_name]
    ('35', 'tabilo', true),                      -- tracked Alejandro Tabilo  [full_name]
    ('175', 'tabur', true),                      -- tracked Clement Tabur  [api_search]
    ('9', 'tiafoe', true),                       -- tracked Frances Tiafoe  [full_name]
    ('511', 'tien', true),                       -- tracked Learner Tien  [full_name]
    ('2', 'tirante', true),                      -- tracked Thiago Agustin Tirante  [last_name]
    ('28', 'tsitsipas', true),                   -- tracked Stefanos Tsitsipas  [full_name]
    ('472', 'ugo_carabelli', true),              -- tracked Camilo Ugo Carabelli  [full_name]
    ('26', 'vacherot', true),                    -- tracked Valentin Vacherot  [full_name]
    ('207', 'van_de_zandschulp', true),          -- tracked Botic Van De Zandschulp  [full_name]
    ('234', 'vukic', true),                      -- tracked Aleksandar Vukic  [full_name]
    ('159', 'walton', true),                     -- tracked Adam Walton  [full_name]
    ('527', 'wawrinka', true),                   -- tracked Stan Wawrinka  [full_name]
    ('6', 'zverev', true),                       -- tracked Alexander Zverev  [full_name]
    ('502', 'arnaldi', false),                   --         Matteo Arnaldi  [last_name]
    ('221', 'basilashvili', false),              --         Nikoloz Basilashvili  [last_name]
    ('522', 'bonzi', false),                     --         Benjamin Bonzi  [last_name]
    ('94', 'choinski', false),                   --         Jan Choinski  [last_name]
    ('5', 'damm_jr', false),                     --         Martin Damm  [birthyear_correction]
    ('494', 'diaz_acosta', true),                --         Facundo Diaz Acosta  [last_name]
    ('201', 'droguet', false),                   --         Titouan Droguet  [last_name]
    ('115', 'faria', false),                     --         Jaime Faria  [last_name]
    ('566', 'fery', false),                      --         Arthur Fery  [last_name]
    ('233', 'gaston', false),                    --         Hugo Gaston  [last_name]
    ('113', 'gaubas', false),                    --         Vilius Gaubas  [last_name]
    ('578', 'gea', false),                       --         Arthur Gea  [last_name]
    ('190', 'hijikata', false),                  --         Rinky Hijikata  [last_name]
    ('1634', 'lajovic', false),                  --         Dušan Lajović  [last_name]
    ('15', 'landaluce', false),                  --         Martin Landaluce  [last_name]
    ('191', 'mcdonald', false),                  --         Mackenzie McDonald  [last_name]
    ('1178', 'mochizuki', false),                --         Shintaro Mochizuki  [last_name]
    ('6147', 'molcan', false),                   --         A. Molcan  [last_name]
    ('106', 'nava', false),                      --         Emilio Nava  [last_name]
    ('182', 'pellegrino', false),                --         Andrea Pellegrino  [last_name]
    ('500', 'prizmic', false),                   --         Dino Prizmic  [last_name]
    ('224', 'rodionov', false),                  --         Jurij Rodionov  [last_name]
    ('95', 'safiullin', false),                  --         Roman Safiullin  [last_name]
    ('397', 'samuel', false),                    --         Toby Samuel  [last_name]
    ('844', 'shimabukuro', false),               --         Sho Shimabukuro  [last_name]
    ('399', 'svrcina', false),                   --         Dalibor Svrcina  [last_name]
    ('842', 'sweeny', false),                    --         Dane Sweeny  [last_name]
    ('219', 'travaglia', false),                 --         Stefano Travaglia  [last_name]
    ('164', 'trungelliti', false),               --         Marco Trungelliti  [last_name]
    ('197', 'vallejo', false),                   --         Daniel Adolfo Vallejo  [last_name]
    ('232', 'van_assche', true),                 --         Luca van Assche  [last_name]
    ('501', 'virtanen', false),                  --         Otto Virtanen  [last_name]
    ('446', 'wu_yibing', true)                   --         Yibing Wu  [token_set]
) as v(external_key, slug, confirmed)
join players p on p.slug = v.slug
on conflict (source, entity_type, external_key) do nothing;

-- Не сматчено и требует ручного решения:
--   rune (Holger Rune, TRACKED) — в индексе источника отсутствует вовсе,
--     /players?search=rune его не находит. Разрешится сам, когда он появится
--     в live-выдаче: резолвер увидит незнакомый api id и положит строку в live_unmatched.
--   38 нетрекаемых игроков-заглушек (только фамилия: 'Boyer', 'Zheng M.', 'C. Hewitt' ...).
