# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
go build ./...                                   # compile
go vet ./...
go test ./...                                    # unit tests only — DB-backed ones SKIP
TEST_DATABASE_URL="postgresql://tennis:tennis@localhost:55432/tennis" go test ./...   # all of them
DATABASE_URL="postgresql://..." go run ./cmd/server   # DATABASE_URL is mandatory, no default
RUN_ONCE=live-schedule ... go run ./cmd/server    # run one cron job and exit
```

**`TEST_DATABASE_URL` matters.** Without it `go test ./...` is green but silently skips every DB-backed test — including the four covering the advisory-lock hazard, which is the highest-risk code in the live ingester. Green without it does not mean the suite ran.

`go test ./...` runs the suite (`-race` is clean). Tests live beside the code they cover — `internal/scheduler`, `internal/livesource`, `internal/storage`, `internal/updater/live`; run one with `go test ./internal/scheduler -run TestName`. Pure logic is deliberately kept out of closures and SQL so it can be tested without a DB or network.

`gofmt -l .` currently reports drift in `internal/storage/tournaments.go` (misaligned `var`/struct blocks). Format files you touch; don't reformat the rest as drive-by noise.

Smoke-testing endpoints requires a live Postgres. There is no migration *tool* here — what `db/` has is a bootstrap (see below), and the canonical schema lives in [tennis-data-storage](https://github.com/StVl/tennis-data-storage) (`db/schema.sql`, `docs/db_schema.md`, `docs/api_design.md`), which also owns all content data. **See [`docs/database.md`](docs/database.md) for who owns what and how to build the database; read it before touching `db/`.**

Building one takes three steps and the order matters: `db/schema.sql` (there) → `scripts/migrate_data.py` (there) → the live-ingest DDL and mapping seeds (here). **The third step applies itself:** the service embeds `db/live_{ingest,push,external_ids,edition_ids}.sql` and runs them on every boot under an advisory lock (`LIVE_SCHEMA_AUTO_APPLY`, default true) — all four are idempotent, and a failure is logged rather than fatal, because it breaks only the live feature. The seeds still have to come after the content load, since they join on `players.slug` / `tournament_editions.slug` and on an empty database match zero rows and succeed silently; if that happens Job A logs `no tracked players are mapped to the source`.

`db/` here holds the live-ingest DDL — the schema half went over in [tennis-data-storage#1](https://github.com/StVl/tennis-data-storage/pull/1); the mapping *rows* stay here, maintained from the `live_unmatched` queue — plus `dev_fixtures.sql` for local work (never carried over). Apply any of them with `docker exec -i tennis-pg psql -U tennis -d tennis -v ON_ERROR_STOP=1 < db/<file>.sql`. A local Postgres runs in Docker as `tennis-pg` on port **55432** (`postgresql://tennis:tennis@localhost:55432/tennis`); there is no `psql` on the host. The iOS client is [tennis-tracker](https://github.com/StVl/tennis-tracker); what it has to implement for the Live Activity is [`docs/ios-integration.md`](docs/ios-integration.md).

## Architecture

Two layers, no service/repository indirection:

- `api/` — chi router + handlers. `Handler` holds `*pgxpool.Pool` plus `HandlerConfig` — the few config values handlers need per request (`LiveMatchesCap`); it deliberately does **not** take `*config.Config`, so `api` never depends on env parsing. `NewRouter(pool, HandlerConfig)` also gates the `/v1/dev` subtree on `DevEndpoints`, at route registration rather than inside the handler. One file per domain (`players.go`, `matches.go`, `tournaments.go`, `feed.go`, `users.go`, `live.go`, `dev.go`); shared helpers in `respond.go` (`writeError`, `respondQueryError`, `langParam`, `intParam`, `statusList`, `matchIDParam`).
- `internal/storage/` — raw SQL + the response types that get JSON-encoded straight to the client. Functions are **package-level and take the pool as an argument** (`storage.GetPlayer(ctx, pool, lang, slug)`), not methods on a struct. Storage structs carry the `json` tags — there are no separate DTOs, so changing a storage struct changes the wire format.
- `internal/livesource/` — everything vendor-specific about the live-status feed: wire structs, `ParseBoard` / `ParseFixtures`, and the `Source` interface. Swapping vendors should be a new file here, not a rewrite of the ingester. **The wire structs deliberately have no `score` field**, so score data cannot physically leave the parser — that is rule 1 from `docs/live-status-ingest.md` expressed as types, and `TestObservationHasNoScoreFields` enforces it against future additions. `event_status` overrides `status` (the feed really does emit `status=live` with `event_status=Finished`); an unrecognised `event_status` **drops the row entirely** rather than being guessed — going into live takes a single observation, so a new vendor value on a still-live row would otherwise raise a card immediately, and a false LIVE is the one failure a card cannot survive. A row whose `status` is `upcoming` is never an observation, whatever its `event_status`. Counters are split — `RowsDoubles` (expected) vs `RowsUnusable` (a signal) — so "200 rows, all doubles" and "200 rows, none readable" cannot look alike to Phase 7's zero-rows guard. `testdata/` holds real captured boards; `TestKnownEventStatuses` fails the day the vendor adds an enum value, which is the point.
- `cmd/server/main.go` — config → pool → cron scheduler → HTTP, with graceful shutdown.
- `internal/updater/live/` — the two live-status jobs. `schedule.go` (Job A, `live-schedule`) learns when tracked players play; `poll.go` (Job B, `live`) flips `matches.status`. `decide.go` and `derive.go` hold the **pure** decision layer — mode, quota governor, debounce — as functions of plain structs with `now` as a parameter, so they are tested with no DB, no network and no clock. `Ingest` is deliberately separated from the job so `POST /v1/dev/live/ingest` can replay a saved board through the identical path with zero quota spent. The lazy player resolver lives here too: when one side of a match resolves and the other does not, it spends one request to look that player up — capped per cycle, negatively cached with growing backoff, and writing `external_ids` with `confirmed_at = null` because it is a guess awaiting review. Matching is by **name only**: `players.birth_date` and `country_code` are NULL for all 173 rows, so there is no second signal, and the predicate therefore refuses far more often than it accepts.
- `internal/updater/{players,tournaments}` — cron jobs behind the `updater.Updater` interface (`Name()`, `Update(ctx)`). Both are **stubs that just `SELECT 1`**; real data currently arrives via the tennis-data-storage pipeline, not through this service. `internal/scheduler` wraps robfig/cron. Each run gets a context derived from a **root context owned by `main`** — cancelling it aborts in-flight runs, so `cron.Stop()` does not hold shutdown for a full timeout on every deploy. `Job.Timeout` overrides `UPDATE_TIMEOUT` per job (0 = use the shared value); needed because the shared 5m is longer than a frequent job's interval, so a slow call would overlap the next tick. The run body lives in `runOnce`, outside the cron closure, so it is testable — cron has minute granularity, so a test going through the scheduler would wait a minute.

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
