# tennis-backend

Go-бэкенд для [tennis-tracker](https://github.com/StVl/tennis-tracker) — iOS-приложения и виджета для фанатов тенниса ATP. Отдаёт игроков, турниры, матчи и рейтинги из Postgres, собирает готовые ответы для экранов приложения и хранит анонимных пользователей с подписками.

Схема БД и скрипты миграции живут в [tennis-data-storage](https://github.com/StVl/tennis-data-storage) (`db/schema.sql`, `docs/db_schema.md`, `docs/api_design.md`).

## Стек

- Go, [chi](https://github.com/go-chi/chi) (HTTP), [pgx/v5](https://github.com/jackc/pgx) (Postgres), [robfig/cron](https://github.com/robfig/cron) (планировщик апдейтеров)
- Postgres (Railway)

## Запуск

```bash
DATABASE_URL="postgresql://user:pass@host:port/db" go run ./cmd/server
```

| Переменная | Default | Что делает |
|---|---|---|
| `DATABASE_URL` | — (обязательна) | строка подключения к Postgres |
| `HTTP_PORT` | `8080` | порт HTTP-сервера |
| `DB_MAX_CONNS` | `10` | размер пула соединений |
| `TOURNAMENTS_CRON` | `*/30 * * * *` | расписание апдейтера турниров (пока заглушка) |
| `PLAYERS_CRON` | `0 */2 * * *` | расписание апдейтера игроков (пока заглушка) |
| `UPDATE_TIMEOUT` | `5m` | таймаут одного прогона апдейтера |

## Общие соглашения

- Все ответы — JSON. Списки завёрнуты в `{"items": [...]}`, если не сказано иное.
- Идентификаторы в URL — слаги: игрок `sinner`, турнир `wimbledon`, розыгрыш `wimbledon_2026`.
- `?lang=ru|en` (default `en`) — язык локализуемых полей (описания стилей, названия раундов, traits) с фолбэком en → ru.
- Время — ISO 8601 с таймзоной. Параметр `?tz=Area/City` задаёт, в какой таймзоне считать «сегодня».
- Пагинация: `?limit=` (default 50) и `?offset=`.
- Ошибки: HTTP-статус + `{"error": {"code": "not_found", "message": "..."}}`.

## Сервисные

| Метод | Путь | Описание |
|---|---|---|
| `GET` | `/health` | пинг БД: `{"status":"ok"}` или 503 |
| `GET` | `/hello` | smoke-тест |

## Композитные (один запрос = один экран)

Замена config.json: приложение и виджет получают готовую выдачу одним запросом. До появления серверных подписок клиент передаёт их сам через `?player_ids=` (из App Group); авторизованные варианты с подписками из БД — в разделе «Пользователи».

### `GET /v1/home?player_ids=sinner,alcaraz&lang=en`

Главный экран целиком:

```json
{
  "your_season": [
    {
      "player": {"slug": "sinner", "name": "Jannik Sinner", "photo_url": "...",
                 "rank": 1, "rank_delta": 0, "play_style": "Aggressive Baseliner", ...},
      "next_match": { ...матч глазами игрока... },
      "next_tournament": null
    }
  ],
  "all_players": [
    {"slug": "sinner", "name": "Jannik Sinner", "photo_url": "...",
     "rank": 1, "rank_delta": 0, "followed": true},
    ...все 102 игрока ростера...
  ]
}
```

- `your_season` — карточки подписанных в порядке `player_ids`; у каждой ближайший матч, а если его нет — ближайший турнир (`next_tournament` заполняется только при `next_match: null`).
- `all_players` — **всегда весь ростер** с флагом `followed` (эхо переданного списка), отсортирован по рейтингу; `rank`/`rank_delta` могут быть `null`, если нет свежего снапшота. Пустой `player_ids` → пустой `your_season` + полная сетка: режим онбординга.

### `GET /v1/widget?player_ids=...&tz=Europe/Belgrade`

Готовый таймлайн виджета — вся логика состояний на сервере, клиент только рендерит:

```json
{
  "state": "rows | split | no_follows | no_matches",
  "rows": [
    {"type": "match", "player": {...}, "opponent": {...}, "tournament_name": "US Open",
     "surface": "hard", "start_at": "2026-08-31T18:00:00Z", "is_today": false},
    {"type": "tournament", "player": {...}, "tournament_name": "...",
     "surface": "clay", "start_date": "...", "end_date": "...", "is_today": false}
  ],
  "today_column": [
    {"p1_last_name": "Shelton", "p2_last_name": "Tabilo", "start_at": "..."}
  ]
}
```

Правила (из README виджета):
- слот на каждого подписанного: ближайший scheduled/live матч, иначе ближайший upcoming-турнир; сортировка по дате, максимум 3 строки;
- `no_follows` — пустые подписки; `no_matches` — подписки есть, событий нет;
- `split` — когда **ни один** подписанный не играет сегодня, но сегодня есть матчи не-подписанных: `today_column` содержит до 5 матчей, отсортированных по лучшему текущему рейтингу участника;
- «сегодня» считается в `tz`; `is_today` проставлен у матчей-строк.

## Игроки

### `GET /v1/players?tracked=true&search=&lang=`
Список ростера, отсортирован по рейтингу. `?tracked=false` — все игроки, включая соперников вне ростера. `?search=` — подстрока имени (регистронезависимо). Поля: slug, имя, фото, `rank`, `rank_delta`, стиль.

### `GET /v1/players/{slug}?include=last_matches,next_match,next_tournament`
Полный профиль: имя/фамилия, фото, дата рождения, рука, рост, страна, traits, pro_tip, links, стиль с описанием, текущий рейтинг/очки. `include` доклеивает блоки детального экрана — карточка игрока собирается одним запросом:
- `last_matches` — 3 последних сыгранных;
- `next_match` — ближайший scheduled/live;
- `next_tournament` — ближайший upcoming-турнир.

### `GET /v1/players/{slug}/matches?status=&limit=&offset=`
Матчи **глазами игрока**: `opponent`, `result` (won/lost), `score_text` со стороны игрока. `status` — CSV из `scheduled,live,completed,cancelled` (default все). Только `status=completed` → новые сверху, иначе ближайшие сверху.

### `GET /v1/players/{slug}/tournaments?status=upcoming,ongoing`
Турниры, на которые игрок заявлен: розыгрыш, даты, покрытие, посев. `status` — CSV из `upcoming,ongoing,completed`.

### `GET /v1/players/{slug}/ranking-history`
Снапшоты рейтинга по датам: `rank`, `points`, `race_points` (под график).

### `GET /v1/players/{a}/h2h/{b}`
Личные встречи глазами игрока `a`: `{"wins": 2, "losses": 1, "matches": [...]}`.

## Турниры и рейтинг

### `GET /v1/tournaments?status=upcoming|ongoing|completed&year=2026`
Розыгрыши. Статус **вычисляется из дат** на сервере. Поля: слаги розыгрыша и турнира, название, даты, покрытие, локация, лого, чемпион/финалист (у завершённых).

### `GET /v1/tournaments/{edition_slug}`
Карточка розыгрыша (`wimbledon_2026`): описание, условия (`conditions`: зал, высокогорье...), draw_size, призовой фонд, чемпион/финалист + `entries` — заявленные игроки с посевом и текущим рейтингом.

### `GET /v1/tournaments/{edition_slug}/draw`
Сетка: `{"rounds": [{"round": "QF", "label": "Quarterfinal", "matches": [...]}]}` — раунды в турнирном порядке (R1 → F), внутри по позиции в сетке.

### `GET /v1/tournaments/{tournament_slug}/history`
История розыгрышей **бренда** (`wimbledon`) по годам: чемпион и финалист каждого года.

### `GET /v1/rankings?tour=atp&limit=100`
Текущий рейтинг: место, очки, race-очки, `rank_delta` к предыдущему снапшоту.

### `GET /v1/play-styles?lang=`
Справочник стилей игры с локализованными описаниями.

## Матчи

### `GET /v1/matches?date=&tz=&status=&player=&edition=&sort=&limit=&offset=`

Нейтральная форма матча (не глазами игрока):

```json
{
  "id": 501, "edition": "wimbledon_2026", "tournament_name": "The Championships, Wimbledon",
  "round": "F", "scheduled_at": "2026-07-12T15:00:00+01:00", "court": "Centre Court",
  "status": "completed", "surface": "grass",
  "winner_side": 1, "outcome": "normal",
  "sides": [
    {"side": 1, "players": [{"slug": "sinner", "name": "Jannik Sinner", "last_name": "Sinner", "rank": 1, ...}]},
    {"side": 2, "players": [{"slug": "zverev", ...}]}
  ],
  "sets": [[6,7,7],[7,6,2],[6,3,null],[6,4,null]],
  "score_text": "6-7(7), 7-6(2), 6-3, 6-4",
  "live": null
}
```

- `sets` — `[side1_games, side2_games, tiebreak_loser_points|null]`; `score_text` — та же строка со стороны side 1;
- отсутствие стороны в `sides` = TBD; `live` заполнен только при `status=live` (текущий гейм/подача);
- `outcome`: `normal | retirement | walkover | default`.

Фильтры: `date=today|YYYY-MM-DD` (+`tz`), `status` (CSV), `player=slug`, `edition=slug`.
Сортировки: `sort=start_at` (default) | `sort=best_rank` — по лучшему текущему рейтингу участника (порядок колонки TODAY).

### `GET /v1/matches/{id}`
Один матч в той же форме.

## Пользователи (анонимный auth + подписки)

Токен opaque (`tt_...`), выдаётся один раз при регистрации; в БД хранится только sha256. Клиент держит его в Keychain. Все `/v1/users/me/*` требуют заголовок `Authorization: Bearer <token>`, иначе 401.

### `POST /v1/users`
Тело (опционально): `{"device_id": "..."}`. Ответ `201`:
```json
{"user_id": "2f134f14-...", "token": "tt_32406d5b..."}
```

### `GET /v1/users/me`
Профиль: `user_id`, `email` (null до привязки), `settings`, `created_at`, `follows_count`.

### Подписки

| Метод | Путь | Описание |
|---|---|---|
| `GET` | `/v1/users/me/follows` | список подписок в порядке добавления (slug, имя, фото, дата) |
| `PUT` | `/v1/users/me/follows/{slug}` | подписаться; идемпотентно (204); 404 если игрока нет |
| `DELETE` | `/v1/users/me/follows/{slug}` | отписаться; идемпотентно (204) |
| `PUT` | `/v1/users/me/follows` | заменить весь список: `{"player_slugs": ["sinner", ...]}` (онбординг). Ответ: итоговый список + `unknown_slugs` |

### `GET /v1/users/me/home` и `GET /v1/users/me/widget?tz=`
Те же `home`/`widget`, но подписки берутся из БД (порядок = порядок добавления). Формат ответов идентичен публичным вариантам.

Пример полного флоу:

```bash
TOKEN=$(curl -s -X POST localhost:8080/v1/users -d '{"device_id":"dev1"}' | jq -r .token)
curl -s -X PUT -H "Authorization: Bearer $TOKEN" localhost:8080/v1/users/me/follows \
  -d '{"player_slugs":["sinner","alcaraz"]}'
curl -s -H "Authorization: Bearer $TOKEN" "localhost:8080/v1/users/me/widget?tz=Europe/Belgrade"
```

## Структура кода

```
cmd/server/          вход: конфиг → пул БД → cron-планировщик → HTTP
api/                 роутер и HTTP-хендлеры (по доменам: players, matches, ...)
internal/storage/    SQL-запросы и типы ответов (pgx)
internal/updater/    апдейтеры данных по крону (players, tournaments — пока заглушки)
internal/scheduler/  обвязка robfig/cron
internal/config/     env-конфигурация
```

Апдейтеры — задел под обновление БД из теннисного API (live score и т.д.); сейчас данные поступают через пайплайн tennis-data-storage (LLM правит JSON-шарды → `migrate_data.py` зеркалит их в Postgres).
