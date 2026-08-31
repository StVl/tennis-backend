# The database: who owns what, and what to run in what order

Two repositories touch one Postgres database, and neither one alone can build it. That is the whole
reason this file exists.

## Who owns what

| | [tennis-data-storage](https://github.com/StVl/tennis-data-storage) | tennis-backend (here) |
|---|---|---|
| Schema (tables, types, views) | **owns it** — `db/schema.sql` | copies under `db/`, carried over by PR |
| Content data (players, tournaments, matches, results) | **owns it** — `scripts/migrate_data.py` from `data/*.json` | never writes it |
| Vendor id mappings (`external_ids`) | holds the table | **owns the rows** — `db/live_*_ids.sql` |
| Live status (`matches.status`) | never writes it | **owns it**, one column, always guarded |
| Ingest bookkeeping (`live_*` tables) | holds the tables | **owns the rows** |

The short version: **they own the schema and the content, we own the live status and the vendor
mappings.** Everything this service writes to a table the pipeline owns is a single column,
`matches.status`, and every such write is conditional on the current value — see
`FlipLive` / `FlipOut` in `internal/storage/live.go`.

## Building a working database from nothing

Three steps, and the order is not optional.

```bash
# 1. Schema — creates every table, type and view. No data.
#    In the tennis-data-storage checkout:
psql "$DATABASE_URL" -f db/schema.sql

# 2. Content — players, tournaments, editions, matches, results.
#    Also in tennis-data-storage:
DATABASE_URL="$DATABASE_URL" python3 scripts/migrate_data.py     # --dry-run to rehearse

# 3. Live-ingest tables and vendor id mappings — this repo.
#    Normally you do NOT run this: the service applies it on every boot.
#    By hand, if you want it before the deploy:
psql "$DATABASE_URL" -f db/live_ingest.sql
psql "$DATABASE_URL" -f db/live_push.sql
psql "$DATABASE_URL" -f db/live_external_ids.sql
psql "$DATABASE_URL" -f db/live_edition_ids.sql
```

**Step 3 applies itself.** The four files are embedded in the binary (`db/embed.go`) and run at
startup under an advisory lock, so a deploy is enough and nobody needs psql against production.
`LIVE_SCHEMA_AUTO_APPLY=false` turns that off. A failure is logged, not fatal: it breaks the live
feature only, and taking the whole API down over it would be worse. Two instances booting together
are fine — the second sees the lock held and skips, because the work is idempotent anyway.

`db/dev_fixtures.sql` is deliberately **not** embedded: it creates synthetic matches, and this set
runs on production.

**Why step 3 has to come last.** Those two files map the vendor's ids onto ours by *slug*:

```sql
join players p on p.slug = v.slug                         -- live_external_ids.sql
join tournament_editions te on te.slug = v.edition_slug   -- live_edition_ids.sql
```

On an empty database there is nothing to join against, so the inserts match zero rows and complete
without error. That is exactly what happens if you run them before step 2, and nothing complains at
the time. It is also why these files are **not** part of `schema.sql`: that file is data-free by
design (except `tours` and `rounds`, which nothing depends on), so slug-joined seeds cannot live
there.

Steps 1 and 3 are idempotent — verified by applying each twice. Step 2 has a `--dry-run`
flag; whether a second real run is safe is `migrate_data.py`'s business, not documented here.

### If you skip step 3

The ingester will resolve no players at all, and it says so on the first schedule run rather than
failing quietly:

```
WARN live-schedule: no tracked players are mapped to the source; seed db/live_external_ids.sql
```

That warning (`internal/updater/live/schedule.go:76`) is the intended detector for this mistake.

## What each file in `db/` is

| File | Carried to tennis-data-storage? | What it is |
|---|---|---|
| `live_ingest.sql` | **yes** — schema | The seven ingest tables: runs, observations, flags, schedule, unmatched, events, resolve attempts |
| `live_push.sql` | **yes** — schema | `live_activity_sessions` (+ `device_push_tokens`, see below) |
| `live_external_ids.sql` | table yes, **rows no** | `external_ids` DDL + 134 player mappings |
| `live_edition_ids.sql` | **rows no** | 5 tournament mappings, ATP singles only |
| `dev_fixtures.sql` | **never** | Two synthetic `scheduled` matches for local testing. Marked "не переносить" |

The schema half of the first four went over in
[tennis-data-storage#1](https://github.com/StVl/tennis-data-storage/pull/1). The mapping *rows* stay
here on purpose: they are maintained from a queue that lives here. An unrecognised vendor id lands in
`live_unmatched` with `reason='edition_unmapped'`, a human adds a line to the file, and it is
re-applied. That loop belongs next to the service that produces the queue.

`device_push_tokens` is the one exception in the table above: it was **not** carried over, because
tennis-data-storage already has `push_tokens` with `push_token_kind_t` containing
`'apns_live_activity'`. This service should switch to that table and drop its own.

## Local development

Postgres runs in Docker as `tennis-pg` on port **55432**, and there is no `psql` on the host, so
every command goes through the container:

```bash
docker exec -i tennis-pg psql -U tennis -d tennis -v ON_ERROR_STOP=1 < db/<file>.sql
```

Connection string: `postgresql://tennis:tennis@localhost:55432/tennis`.

The local database matches the layout above, plus `db/dev_fixtures.sql`, which creates two
`scheduled` matches in `us_open_2026`. Those fixtures exist because **this local database has no
`scheduled` rows at all** — 481 matches, every one `completed` — so without them there is nothing for
the live ingester to flip and no way to test it. That is a property of the local snapshot, not of
production: production carries scheduled matches as a product feature. Re-running the
fixtures file refreshes their `scheduled_at` relative to now; cleanup is
`delete from matches where import_key like 'devfix\_%'`.

`TEST_DATABASE_URL` points the DB-backed tests at this database. Without it they **skip** and the
suite is still green — see the warning in `CLAUDE.md`.

## Things that will bite

- **A schema change here is a cross-repo PR.** There are no migrations in this repo. Editing
  `db/*.sql` changes nothing anywhere until it is applied locally *and* carried over.
- **`create table if not exists` hides edits.** Changing a table definition in a file that has
  already been applied does nothing to an existing database. Either write the `alter`, or drop and
  rebuild.
- **`external_ids` is N:1 and its `entity_id` has no foreign key**, both deliberately. One entity can
  have several vendor keys — the vendor carries split identities for the same player, and different
  tours and draws of one tournament are different ids (`US Open`: 1217 atp, 1218 wta, 1221 doubles).
  A dangling mapping row is recoverable from the slug; a foreign key that blocks the pipeline's
  import is not.
- **Every foreign key from a `live_*` table into the content core is `on delete cascade.`** Without
  it our bookkeeping blocks *their* deletes: an edition delete cascades into `matches` and would fail
  on our rows. One of these was missing once and would have broken the pipeline's import.
