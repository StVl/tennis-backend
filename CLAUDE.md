# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
go build ./...                                   # compile
go vet ./...
DATABASE_URL="postgresql://..." go run ./cmd/server   # DATABASE_URL is mandatory, no default
```

There are no tests in the repo yet (`go test ./...` finds nothing). When adding them: `go test ./internal/storage -run TestName`.

`gofmt -l .` currently reports drift in `internal/scheduler/scheduler.go` and `internal/storage/{feed,matches,players,tournaments}.go` (misaligned `var`/struct blocks). Format files you touch; don't reformat the rest as drive-by noise.

Smoke-testing endpoints requires a live Postgres — there is no local DB setup, no migrations, and no fixtures in this repo. Schema and data pipeline live in [tennis-data-storage](https://github.com/StVl/tennis-data-storage) (`db/schema.sql`, `docs/db_schema.md`, `docs/api_design.md`); the iOS client is [tennis-tracker](https://github.com/StVl/tennis-tracker).

## Architecture

Two layers, no service/repository indirection:

- `api/` — chi router + handlers. `Handler` holds only `*pgxpool.Pool`. One file per domain (`players.go`, `matches.go`, `tournaments.go`, `feed.go`, `users.go`); shared helpers in `respond.go` (`writeError`, `respondQueryError`, `langParam`, `intParam`, `statusList`).
- `internal/storage/` — raw SQL + the response types that get JSON-encoded straight to the client. Functions are **package-level and take the pool as an argument** (`storage.GetPlayer(ctx, pool, lang, slug)`), not methods on a struct. Storage structs carry the `json` tags — there are no separate DTOs, so changing a storage struct changes the wire format.
- `cmd/server/main.go` — config → pool → cron scheduler → HTTP, with graceful shutdown.
- `internal/updater/{players,tournaments}` — cron jobs behind the `updater.Updater` interface (`Name()`, `Update(ctx)`). Both are **stubs that just `SELECT 1`**; real data currently arrives via the tennis-data-storage pipeline, not through this service. `internal/scheduler` wraps robfig/cron and gives each run a `UPDATE_TIMEOUT` context.

Adding an endpoint: storage function + json-tagged type in `internal/storage/<domain>.go` → handler method in `api/<domain>.go` → route in `api/router.go` → document it in `README.md` (the README is the API reference and is kept complete).

Code comments and README are in Russian; match that when editing existing files.

### Database contract

The service is read-mostly and leans heavily on views defined in the other repo — treat these names as a hard dependency:

- `v_current_rankings` — current rank/points/`delta_vs_prev`, always joined with `tour_code = 'atp'`.
- `v_player_matches` — the "player's-eye view" of a match: `opponent_id`, `result` (won/lost), `score_text` from that player's side. Every player-scoped match query goes through it.
- `v_tournament_editions` — editions with `status` (upcoming/ongoing/completed) **computed from dates**; status is never stored or filtered in Go.

Tables touched directly: `players`, `matches`, `match_participants`, `match_sets`, `tournaments`, `tournament_editions`, `tournament_entries`, `rounds`, `play_styles`, `ranking_snapshots`, and for auth `profiles`, `auth_tokens`, `follows`.

### Domain distinctions that bite

- **Tournament vs edition slug.** `wimbledon` is the brand, `wimbledon_2026` an edition. `/v1/tournaments/{slug}` and `/{slug}/draw` take an *edition* slug; `/{slug}/history` takes a *tournament* slug. Same chi param name, different lookups.
- **Neutral `Match` vs `PlayerMatch`.** `Match` (matches.go) has `sides`/`sets`/`winner_side` and is symmetric; `PlayerMatch` (players.go) is asymmetric with `opponent`/`result`. Composite feeds use `PlayerMatch`.
- **Composite endpoints own the screen logic.** `/v1/home` and `/v1/widget` return finished screens; the widget's four-state machine (`rows | split | no_follows | no_matches`), the 3-row cap and the 5-match TODAY column all live in `storage.GetWidgetFeed`. Client just renders — keep logic server-side.
- **Public and authed variants share storage.** `/v1/home` (`?player_ids=`) and `/v1/users/me/home` (follows from DB) both call `storage.GetHomeFeed`; same for widget. Change the storage function, both stay in sync — never fork the logic into a handler.

### Recurring implementation patterns

- **Shared SELECT prefixes.** `matchSelect` and `editionSelect` consts are concatenated with per-query `where`/`order by` (see `ListMatches`, `GetMatch`, `TournamentDraw`). Adding a column means updating the matching `scan*` function.
- **Dynamic filters** use the `arg()` closure in `ListMatches` to append placeholders — all user input stays parameterized; only fixed SQL fragments are string-concatenated.
- **Nested collections come back as `json_agg` in lateral joins** (participants, sets), scanned as text and unmarshalled in Go (`scanMatch`). Sets are `[side1_games, side2_games, tiebreak_loser_points|null]`; `score_text` is derived in Go by `scoreText()`.
- **jsonb passthrough**: localized/free-form columns (`traits`, `links`, `settings`, `conditions`, `description`) are cast `::text` in SQL and held as `json.RawMessage` via `rawOrNull` (nil → literal `null`).
- **i18n** is `coalesce(col->>$lang, col->>'en', col->>'ru')` in SQL; `langParam` defaults to `en`. No Go-side translation table.
- **Timezones**: handlers parse `?tz=` into a `*time.Location`, compute local day boundaries, then pass UTC instants (`DayFrom`/`DayTo`) into SQL. Bad tz → 400 `bad_tz`.
- **Response shape**: lists are `{"items": [...]}`; slices that must not serialize as `null` are pre-initialized (`[]SeasonCard{}` in feed.go). Errors are `{"error": {"code", "message"}}`; `respondQueryError` maps `pgx.ErrNoRows` → 404 `not_found` and everything else → 500 with the raw error text.
- **Status params** go through `statusList(r, allowed...)` — empty means all allowed, unknown value returns 400 rather than being ignored.

### Auth

Anonymous only. `POST /v1/users` mints an opaque `tt_<hex>` token, returns it once, stores just its sha256 in `auth_tokens`. The `/v1/users/me` subtree is wrapped by `handler.requireUser`, which resolves the Bearer token via `storage.AuthUser` (that call also bumps `last_seen_at`) and stashes the user id under an unexported `ctxKey`; read it with the `userID(r)` helper, never from the header again. Follow mutations are idempotent (204); `ReplaceFollows` runs in a transaction and encodes user-chosen order as millisecond offsets on `created_at`, which is what makes follow order stable in home/widget output.

## Config

Env-only, `internal/config`. `DATABASE_URL` required; `HTTP_PORT` (8080), `DB_MAX_CONNS` (10), `TOURNAMENTS_CRON` (`*/30 * * * *`), `PLAYERS_CRON` (`0 */2 * * *`), `UPDATE_TIMEOUT` (5m). Deployed on Railway.
