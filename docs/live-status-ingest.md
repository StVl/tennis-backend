# Handoff — live match status ingestion

**For:** an agent working in `tennis-backend`.
**Written by:** the iOS side, after choosing the data source. You are being handed a decided design,
not an open question. Where something is genuinely undecided it says so explicitly.
**Revised:** 2026-08-24, backend side, after probing the API with a real key and inspecting the
database. The original text is preserved; every change is marked **`Revised 2026-08-24`**. Read
*Status* first — several things this document treats as open are now settled, and one thing it
assumes is true turns out not to be.

The consumer is a **Live Activity** on iOS: a lock-screen card that appears when a player the user
follows walks on court. Its full plan lives in the client repo at `tennis-tracker/docs/live-activity.md`.
You do not need to read it to do this task, but it is the authority on anything this file leaves vague.

---

## Status — 2026-08-24

### Blocking everything: there are no matches to flip

`matches` contains 481 rows, **all `completed`**, the newest scheduled `2026-08-03`. No row has ever
carried status `scheduled`. Cincinnati is `ongoing` in `v_tournament_editions` with **0 matches**;
US Open 2026 has 66 `tournament_entries` and **0 matches**.

The ingester's whole job is flipping an existing `scheduled` row to `live`, and rule 3 forbids it
from creating one. As things stand it would resolve both players correctly, look for the match they
are playing, find nothing, and write a `live_unmatched` row — every poll, forever, producing zero
cards.

This is not a pipeline that is merely behind. Per `README.md`, data arrives as *LLM edits JSON shards
→ `migrate_data.py` mirrors to Postgres* — human-driven and batch, which structurally cannot produce
a match row before the match is played.

**Loading draws as `scheduled` matches is the prerequisite for this entire feature, and it lives in
tennis-data-storage, not here.** See *New open questions* below for the three ways out.

### Settled by the human

| Question (was *Questions for the human*) | Answer |
|---|---|
| Free or Basic tier for production | **Free** — 100/day. Poll interval and card latency follow from this |
| Rain delays if the API cannot express them | **Deferred** pending a real enum — since resolved, the API *can*; see below |
| Doubles in or out | **Out.** Excluded at parse time |
| Outbox or `pg_notify` | **Outbox table** — survives Railway's redeploy-per-push, replayable, at-least-once |
| Debounce into live *(new question, forced by the free tier)* | **A single `on_court` observation.** Out of live is unchanged: explicit finish, or three absences |
| `Match.Live` / `matches.live_state` *(new)* | **Left alone.** Nothing writes it, so it stays null |

### Verified against the live API

Everything the document marks unverified, resolved with a real key on 2026-08-22/23:

- **Base URL** `https://api.livetennisapi.com/api/public/v1`
- **Auth** `Authorization: Bearer <key>` (or `X-API-Key`)
- **Spec** `https://docs.livetennisapi.com/openapi.yaml` — OpenAPI 3.1, v1.7.1, ~3700 lines. The only
  machine-readable doc; the docs site renders via JS and scrapes to nothing.
- **`GET /usage`** reports remaining quota and is available on the free tier.
- **Match status enum** `upcoming | live | completed | cancelled`. The spec gives it bare — no
  transition semantics, no latency claim.
- **`event_status`** `Retired | Cancelled | Walk Over | Postponed | Interrupted | null`.
  **`Interrupted` is an in-play suspension** — so yes, the API can express rain. Caveat from the
  spec: the value is *cleared* when a suspended match resumes, so no record of the pause survives
  upstream. Our `live_observations` becomes the only history.
- **The spec already drifts.** A live board returned `event_status: "Finished"` — not in the
  documented enum — on a match whose `status` was still `live`. Treat the enum as open, log
  unrecognised values, and let **`event_status` win over `status`**. Stale live rows are real.
- **`round_code`** uses `F | SF | QF | R16 | R32 | R64 | R128 | RR | BR | Q | Q1…Q4 | ER`, matching
  our own `matches.round_code` vocabulary.
- **Pagination** `limit` maxes at 200, response carries `meta.has_more`. A sampled live board held 19
  matches; the upcoming board held 309 across two pages. One page will not always be enough.

### The finding that changes the design

**Every match payload carries the source's player ids** — `players.p1.id` and `players.p2.id` are
full `Player` objects, not names. Identity resolution is therefore a primary-key lookup on
`external_ids`, not surname matching, and it never touches the `players` table (2 buffer reads).
This supersedes §3 and the `Observation` type in §1. See both.

### The second finding: the source publishes our players' schedule

**`Revised 2026-08-25.`** `GET /matches?status=upcoming` accepts a **repeatable `player=` filter,
up to 50 ids per call**, and returns `scheduled_time` in UTC. Our 101 tracked players therefore cost
**3 calls** for a complete fixture list. Verified — one call for 50 of them returned:

```
19:30Z        Winston-Salem   Collignon v Hijikata
19:30Z        Winston-Salem   Kovacevic v van Assche
19:30Z        Winston-Salem   Cerundolo v Suresh
21:00Z        Winston-Salem   Duckworth v Navone
15:00Z (+1d)  US Open         Pavlovic v Fearnley
```

There is also `GET /fixtures` (free) — lighter payload, `start_time` in UTC and `player1_id` /
`player2_id` resolved, but **no `player=` filter**, so it needs local filtering. Either works;
`?status=upcoming&player=` is the more targeted of the two.

Two consequences, both large:

1. **The poll window should be derived from this, not from our calendar.** See §5 and §6 — it cuts
   both the request count and the card latency at the same time.
2. **This is scheduled-match data, published before the matches are played** — precisely what our
   database has never had. See open question 1.

### Done

- **Player mapping built and seeded.** `db/live_external_ids.sql` — `external_ids` DDL plus 134 seed
  rows, keyed on `players.slug` rather than `players.id` so it is safe to run in any environment
  (`id` is `generated always as identity` and is not stable across databases). Applied to the local
  dev DB and verified.
- **101 of 102 tracked players mapped**, all confirmed. The 30-row review queue
  (`confirmed_at is null`) is entirely untracked surname-stub rows, so no Live Activity depends on a
  guess.
- **Holger Rune is unmappable** — absent from the source's index entirely; `/players?search=rune`
  finds only substring noise. He resolves the first time he walks on court, but only if the lazy
  resolver in §3 exists.

### New open questions

1. **How do `scheduled` matches get into the database?** The blocker above. Three ways:
   (a) pre-load draws through the existing pipeline when they publish, ~127 rows per slam, keeps
   rule 3 intact; (b) let the ingester create matches, narrowly — ongoing tracked edition, both
   players resolved, known `round_code` — which breaks rule 3 as written; (c) accept coverage only
   for hand-loaded tournaments. **This is the iOS side's call, because rule 3 is theirs.**

   > **Revised 2026-08-25 — a fourth option, and the best of them.** Create match rows from the
   > **fixture feed** (`?status=upcoming&player=` or `/fixtures`) rather than from live observations.
   > It is still creating rows from API payloads, so rule 3 still needs relaxing — but in the
   > narrowest possible direction. A fixture carries an explicit UTC start time, a `round_code`
   > already in our vocabulary, a stable `tournament_id`, and both player ids resolved. That is draw
   > data arriving *before* the match is played — which is the thing rule 3 exists to protect — as
   > opposed to (b), which infers a match must exist because we saw it already in progress.
   >
   > The ingester fetches this feed anyway for the poll window (§6), so the data is in hand either
   > way. The only question is whether it may write it.
   >
   > Guard rails if chosen: only when both players resolve, only inside an edition we already have,
   > only with a non-null `round_code`, stamp provenance on the row, and **insert only — never update
   > a row the pipeline owns.**

2. **What status do we write when a match ends?** Not `completed`-with-a-score — that belongs to the
   pipeline. Proposal: this job owns only the *live* flag and restores the status the row held before
   the flip, recorded at flip time.
3. **The slam window does not fit the free tier.** A 16-hour US Open day at `*/10` costs 96 of 100
   requests, leaving less than one backoff sequence. Either narrow the window to the hours our
   tracked players actually play, or take Basic for that fortnight. Decide before 31 August.

   > **Revised 2026-08-25 — largely resolved by §6.** Fixture-driven polling replaces the blind
   > window with the hours our players actually play, and the quota governor makes the interval
   > self-tuning, so a heavy day stretches itself rather than overrunning. Free tier now fits without
   > a tier change. Worth re-checking against a real US Open day rather than assuming.
4. **Should `players.birth_date` / `country_code` be backfilled?** They are `NULL` for all 173 rows,
   which is why offline matching had only names to work with. `players.json` already carries birthday
   for 129 of the 134 mapped players and country for 131 — zero API calls. It belongs in
   tennis-data-storage. It does not fully solve §3's hard cases (see the Báez note there).

---

## Read this first — the five rules that will silently break things

1. **No score data leaves this service.** The card shows presence, never a scoreline. The chosen API
   returns scores; parse them, drop them, never persist them. See *The ingest boundary*.
2. **Never write results.** This ingester writes `matches.status` only. Scores, winners and the
   transition to `completed`-with-a-score stay with the tennis-data-storage pipeline that owns them.
3. **Never auto-create rows.** No inventing players, matches, editions from API payloads. Unmatched
   observations go to a review table. Inventing rows corrupts the draw the pipeline owns.
4. **Fail loudly, never fail empty.** A parse that yields zero rows during a window when matches
   should exist is an *error*, not a result. Failing empty silently ends every user's Live Activity
   and looks like a quiet afternoon.
5. **Debounce state lives in Postgres, not memory.** Railway restarts on every deploy; in-memory
   counters would re-flap the entire board each time.

> **Revised 2026-08-24.** Rules 1, 2, 4 and 5 stand unchanged and are reinforced by what we found —
> in particular rule 4, since the source emits stale `status: "live"` rows that only `event_status`
> contradicts.
>
> **Rule 3 is now the crux of the whole task.** It is correct as written *and* it is what makes the
> feature undeliverable today: no `scheduled` matches exist to flip, so forbidding row creation
> forbids the feature. Nothing here proposes overriding it — that is the iOS side's decision, framed
> as open question 1 in *Status*. But it can no longer be read as a free constraint.

---

## Scope

**Task 1 — build now.** Live status ingestion: poll the API, resolve identities, derive
`matches.status`, expose a dev trigger.

**Task 2 — design for, do not build.** APNs push infrastructure (device tokens, push-to-start, end
push, sweeper). It is listed at the bottom because Task 1's event mechanism has to be usable by it.
Do not implement it in this pass.

**Out of scope entirely:** the iOS client, anything touching scores, and the tennis-data-storage
pipeline.

> **Revised 2026-08-24.** Task 1 splits cleanly by what the blocker touches:
>
> - **Unblocked, buildable now** — the dev endpoints, config, the source client and parser, the
>   fixture tests, `external_ids` and the resolver. The dev endpoints alone let iOS exercise the full
>   Live Activity lifecycle against hand-flipped matches, with no live API and no loaded draws.
> - **Blocked** — the part that finds *our* match row, and therefore every real status flip. Not
>   blocked on code; blocked on tennis-data-storage loading draws as `scheduled` matches.
>
> The last bullet above is worth re-reading against that: the pipeline is out of scope for this task,
> yet this task cannot deliver a single card until something changes there.

---

## The data source

**[Live Tennis API](https://livetennisapi.com/)** — chosen over tennis-api.com and MatchStat because
it is the only one with a single documented endpoint returning every live match in one request, which
makes quota independent of how many tournaments are running.

| | |
|---|---|
| Live endpoint | `GET /matches?status=live` — all live matches, one call, available on the free tier |
| Other useful endpoints | `GET /matches/{id}/score`, `GET /players/{id}` (free tier) |
| Free tier | 30 req/min, 100 req/day |
| Basic ($9.99/mo) | 60 req/min, **1 000 req/day** — the tier the production poll interval assumes |
| Coverage | ATP, WTA, Challenger, ITF, singles and doubles, on all tiers including free |
| Reference | `docs.livetennisapi.com`, plus an OpenAPI spec on GitHub |

**Base URL and auth scheme are unverified** — take them from `docs.livetennisapi.com` or the OpenAPI
spec. Do not guess them from this file.

> **Revised 2026-08-24.** Both verified — see *Status*. Base URL
> `https://api.livetennisapi.com/api/public/v1`, auth `Authorization: Bearer <key>`. The spec is at
> `https://docs.livetennisapi.com/openapi.yaml`; the docs *site* is a JS app and yields nothing to a
> scrape, so go straight to the YAML.

### Resolve these three unknowns before writing the pipeline

1. **The status enumeration.** Public docs do not list the possible values. You need to know whether
   it can express `suspended` (rain) and `retired`, or only live/finished. Everything downstream
   bends around the answer, and it is the one criterion on which the runner-up API was better.
2. **The response shape**, from a real call. Save it to `testdata/` — see *Testing*.
3. **How fast the live flag actually flips.** Watch one match start and time it. That number sets the
   poll interval; no documentation will tell you honestly.

> **Revised 2026-08-24.**
> **(1) Resolved.** Both enums are in *Status*. It can express suspension (`Interrupted`) and
> retirement. It also emits at least one value the spec does not document.
> **(2) Partly done.** A real live board and a real upcoming board are captured. They still need to
> move into `testdata/` — a quiet Saturday board is 19 matches and thin on ATP; recapture during the
> US Open for a fixture with real variety.
> **(3) Still unknown, and harder than this makes it sound.** There is no baseline to time against:
> `scheduled_time` is not a start time, because a tennis match begins when the previous one on that
> court ends. On a sampled board, three matches sat `upcoming` 4h44m, 4h29m and 2h49m past their
> nominal slot. Measuring it means polling both boards across a real transition and comparing against
> an independent source for when the first point was played. Note also that this number does **not**
> set the poll interval — the quota does. It sets how much unavoidable latency sits *underneath*
> whatever interval we choose.

### Poll interval arithmetic

One request per poll, only during a play window (do not poll an empty overnight court).

| Tier | Window | Interval |
|---|---|---|
| Free, 100/day | 12 h | ~7 min (~8.5 min once 15% is held back for retries) |
| Free, 100/day | 18 h (two continents at once) | ~11 min |
| Basic, 1 000/day | 12 h | **60 s**, with ~280 requests spare |

Make the interval configurable, not a constant. Start on the free tier for development; production
assumes Basic.

> **Revised 2026-08-24.** Production is **Free**, so the middle rows are the real ones. Recommended
> shape: a `LIVE_CRON` env var defaulting to `*/10 * * * *`, rather than the `LIVE_POLL_INTERVAL`
> duration named below — cron matches the two existing jobs, divides the hour evenly, and expresses
> both tiers (Basic would be `* * * * *`). Only sub-minute polling would need `cron.WithSeconds()` or
> a ticker, and we never need sub-minute.
>
> | Tier | Cron | Window | Calls/day | Spare |
> |---|---|---|---|---|
> | Free | `*/10 * * * *` | 12 h | 72 | 28 |
> | Free | `*/10 * * * *` | 16 h (slam) | 96 | **4** |
> | Basic | `* * * * *` | 12 h | 720 | 280 |
>
> The slam row is the problem — four spare requests is less than one backoff sequence. See open
> question 3 in *Status*.

---

## Repo conventions you must follow

From this repo's own `CLAUDE.md` — restated because breaking them is invisible until review:

- **Two layers, no service/repository indirection.** `api/` holds chi handlers; `Handler` carries only
  `*pgxpool.Pool`. `internal/storage/` holds raw SQL plus the json-tagged types that get encoded
  straight to the client. Storage functions are **package-level and take the pool as an argument** —
  `storage.GetPlayer(ctx, pool, lang, slug)` — never methods on a struct.
- **Storage structs are the wire format.** There are no separate DTOs; changing a struct changes the
  API response.
- **Adding an endpoint** = storage function + json-tagged type in `internal/storage/<domain>.go` →
  handler method in `api/<domain>.go` → route in `api/router.go` → **document it in `README.md`**. The
  README is the API reference and is kept complete.
- **Code comments and README prose are in Russian.** Match that in files you touch. (This handoff is
  in English; what you write in the repo is not.)
- `gofmt -l .` already reports drift in `internal/scheduler/scheduler.go` and
  `internal/storage/{feed,matches,players,tournaments}.go`. Format files you touch; do not reformat
  the rest as drive-by noise.
- Config is **env-only**, `internal/config`, `DATABASE_URL` required with no default.
- Cron jobs implement `updater.Updater` (`Name()`, `Update(ctx)`) and register through
  `internal/scheduler`, which wraps robfig/cron and gives each run an `UPDATE_TIMEOUT` context.
- All user input stays parameterized; only fixed SQL fragments are concatenated (see the `arg()`
  closure in `ListMatches`).
- There are no tests in the repo yet. This task adds the first ones — see *Testing*.

**Deliberate exception to note:** this service is read-mostly today, and tennis-data-storage owns
writes. Live status is the one column this service writes, because it is a continuously running
service concern rather than a batch job. Keep the split explicit in comments so it does not get
"fixed" back.

---

## Schema

Canonical schema lives in [tennis-data-storage](https://github.com/StVl/tennis-data-storage)
(`db/schema.sql`). Coordinate additions there; do not silently create tables only this service knows
about.

```sql
-- Maps the source's own ids onto ours. Resolve once, look up forever.
create table external_ids (
  source       text   not null,          -- 'livetennisapi'
  entity_type  text   not null,          -- 'player' | 'edition' | 'match'
  external_key text   not null,          -- the source's id
  entity_id    bigint not null,          -- ours
  confirmed_at timestamptz,              -- null = machine-guessed, awaiting review
  primary key (source, entity_type, external_key)
);

-- Append-only observations. Status is derived from these, never written directly from a poll.
create table live_observations (
  id          bigserial primary key,
  match_id    bigint      not null references matches(id),
  source      text        not null,
  state       text        not null,      -- 'on_court' | 'finished' | 'scheduled' | 'suspended'
  observed_at timestamptz not null default now()
);
create index on live_observations (match_id, observed_at desc);

-- Rows the resolver could not map. A review queue, not an error log.
create table live_unmatched (
  id          bigserial primary key,
  source      text        not null,
  payload     jsonb       not null,      -- scores stripped, see The ingest boundary
  reason      text,
  observed_at timestamptz not null default now()
);

-- One row per poll cycle. The only visibility into why a card did or did not appear.
create table live_ingest_runs (
  id           bigserial primary key,
  source       text        not null,
  started_at   timestamptz not null,
  finished_at  timestamptz,
  rows_parsed  int,
  rows_matched int,
  error        text
);
```

`matches.status` already has a `live` value in its enum — `/v1/matches?status=live` filters on it
today and returns an empty set. No enum migration needed.

> **Revised 2026-08-24.** Confirmed: the enum is `scheduled | live | completed | cancelled`. Verified
> end to end against the dev DB — setting one match to `live` immediately makes it appear in
> `/v1/matches?status=live` **and** turns `is_live` true for both participants in the widget/home
> path, because `nextMatchPerPlayer` already selects `status in ('scheduled','live')`. So the card
> and the app screen read the same column and cannot disagree; the only real divergence is a card
> outliving the truth, which is what `/v1/users/me/live-matches` and the Task 2 sweeper exist for.
>
> Three additions to the DDL above:
> - `external_ids` needs an index on `(source, entity_type, entity_id)` for the reverse direction.
>   The PK only serves external → ours. **Do not make it unique** — the mapping is N:1 (see §3).
> - `entity_id` deliberately carries **no foreign key**, matching the DDL as written. A dangling
>   mapping row is recoverable and re-derivable from slug; an FK that blocks a pipeline import is not.
> - The outbox table for §7 is not in this schema block and needs adding.
>
> `db/live_external_ids.sql` in this repo holds the `external_ids` DDL plus the 134-row seed, ready
> to carry into `tennis-data-storage/db/schema.sql`. It joins on `players.slug`, not `players.id` —
> `id` is `generated always as identity` and is not stable across environments.

> **Revised 2026-08-25 — two more additions, for smart polling (§6).**
>
> ```sql
> -- Расписание наших игроков, как его отдаёт источник. Кэш, а не истина:
> -- строки живут до следующего обновления и не влияют на matches.
> create table live_schedule (
>   source       text        not null,
>   external_key text        not null,      -- id матча у источника
>   scheduled_at timestamptz,               -- null = известна только дата
>   event_date   date,
>   player_keys  text[]      not null,      -- id игроков у источника
>   tournament   text,
>   round_code   text,
>   refreshed_at timestamptz not null default now(),
>   primary key (source, external_key)
> );
> create index on live_schedule (scheduled_at);
> create index on live_schedule (event_date);
> ```
>
> And `live_ingest_runs` needs **`requests_made int`** — one cycle can span several pages, so a run
> count is not a request count, and the quota governor in §6 needs the latter. Add
> `skipped_reason text` too, so an asleep tick is distinguishable from a failure at a glance.

---

## The pipeline

### 1 · Source abstraction

Keep the site-specific parsing behind an interface so a source swap is a new file, not a rewrite.
Everything downstream must be source-agnostic.

```go
// internal/livesource
type State string

const (
    StateOnCourt   State = "on_court"
    StateFinished  State = "finished"
    StateScheduled State = "scheduled"
    StateSuspended State = "suspended"
)

type Observation struct {
    ExternalKey string    // the source's match id
    Edition     string    // as the source names it
    Round       string    // as the source names it
    Players     [2]string // as the source names them
    State       State
    ObservedAt  time.Time
}

type Source interface {
    Name() string
    PollLive(ctx context.Context) ([]Observation, error)
}
```

`Observation` deliberately has **no score fields**. That is the type-level half of rule 1.

> **Revised 2026-08-24.** `Players [2]string` is wrong — the payload carries the source's *player
> ids*, so carry those and never a name. This is what removes fuzzy matching from the hot path
> entirely. `Round` should carry `round_code`, which arrives in our own vocabulary.
>
> ```go
> type Observation struct {
>     ExternalKey string    // the source's match id
>     PlayerKeys  [2]string // the source's player ids — resolved via external_ids
>     RoundCode   string    // already F | SF | QF | R16 … in our vocabulary
>     ScheduledAt *time.Time
>     State       State
>     ObservedAt  time.Time
> }
> ```
>
> `Edition` is dropped: with both players resolved, the match is identifiable without it, and no
> tournament-to-edition mapping is needed at all (see §3). Keep the no-score-fields property.

### 2 · The ingest boundary

The API response contains `sets`, `games`, `points`, `server`, win probabilities. Map it into
`Observation` and let the rest go out of scope. Nothing score-shaped may reach `live_observations`,
`live_unmatched.payload`, a log line, or any response body.

Put a comment on the parse function saying so — the next reader will reasonably wonder why the
interesting fields are being discarded, and the answer is a product rule, not an oversight.

### 3 · Identity resolution

The source says `"J. Sinner"` and `"Cincinnati Masters"`; the DB has `players.slug = 'sinner'` and
`tournament_editions.slug = 'cincinnati_2026'`. Bridging that is most of the work.

Order:

1. `external_ids` hit on the source's match key → done, straight to step 4.
2. Miss → guess: normalise both surnames (strip diacritics, casefold), then look for a match in
   `match_participants` within the edition and a date window around now.
3. A confident guess writes `external_ids` with `confirmed_at = null` and proceeds.
4. No confident guess → `live_unmatched`, and the run continues. One unresolvable row must never
   abort the cycle.

There are ~102 tracked players and typically one to three concurrent editions, so this is a finite
one-time cost: once a player is confirmed, that player is solved permanently.

> **Revised 2026-08-24 — this section is superseded.** The source hands us player ids, so names are
> used exactly once, offline, and never at runtime. Revised order:
>
> 1. **Scope.** Resolve `p1.id` and `p2.id` against `external_ids` (PK lookup, 2 buffer reads).
>    **If neither resolves, drop the row silently** — count it, do not persist it. The board is ATP,
>    WTA, Challenger and ITF worldwide; queueing every unresolvable row would put hundreds into the
>    review table per cycle and stop it being a review table. This replaces the edition-based filter
>    the original text implies.
> 2. **Match.** With both players known, find the row where those two are the participants within a
>    date window around now — near-unique, and it needs no edition mapping.
> 3. **Lazy resolver.** An unknown player id opposite a known one → `GET /players/{id}`, attempt a
>    match, write `external_ids` with `confirmed_at = null`, proceed. Once per player, forever.
> 4. **No confident answer** → `live_unmatched`, cycle continues. Unchanged.
>
> **The lazy resolver is mandatory, not an optimisation.** Two reasons, both observed:
>
> - `players.json` is a **rank-sliced snapshot** (ATP ranks 1–146), not a roster. Qualifying and
>   Challenger draws are full of players outside it. Abedallah Shelbayh was live during a sampled
>   poll, is in our `players` table, and is not in the file.
> - **The source carries split identities for the same person.** An upcoming Hurkacz v Báez match
>   referenced Báez as id `32089` ("S. Baez", rank 53, no birthday, `arg`) while our seed has him as
>   `1615` ("Sebastián Báez", rank 65, born 2000-12-28, `arg`). A *tracked, confirmed* player would
>   fail to resolve. Tabur (`175`, `8824`) and Misolic (`13409`, `14985`) are the same pattern —
>   which is why `external_ids` must stay **N:1** and its `entity_id` index must never become unique.
>
> Note the unwelcome corollary for open question 4: the split records carry the least data, so a
> birthday backfill does not rescue exactly the cases that need rescuing. `32089` has no birthday
> either.

### 4 · Derived status, with debounce

Append the observation, then recompute status from history — never write status straight from a poll.

- **Into live:** two of the last three observations for that match are `on_court`.
- **Out of live:** an explicit `finished` observation, **or** three consecutive absences from the live
  feed.

The asymmetry is deliberate. A false LIVE is the one failure the card cannot survive — being on court
is its only claim. A card that lingers thirty seconds too long costs nothing.

> **Revised 2026-08-24.** The asymmetry stands; the into-live threshold does not. Two-of-three
> assumed 60s polls, where it cost ~2 minutes. At `*/10` it costs ~10 minutes on a ~100-minute match,
> so **a single `on_court` observation flips to live**. Out of live is unchanged — explicit
> `finished`, or three consecutive absences (~30 min).
>
> Accepted cost: correctness now rests entirely on the source's live flag, and once the pusher exists
> a false LIVE is a push that cannot be recalled. That raises the stakes on the fixture-backed parse
> tests, and on letting `event_status` override `status`.
>
> Two mechanics the original leaves implicit:
> - **Absence only counts when the poll succeeded.** After a good cycle, every match currently `live`
>   that did not appear records an absence. After a failed cycle, nothing does.
> - **What we write on exit** is open question 2 in *Status*. Not `completed`-with-a-score.

### 5 · Guards

- **Zero-rows guard.** If a poll parses zero rows while the schedule says matches should be running,
  record `live_ingest_runs.error`, write no observations, sweep nothing, and log at error level. Same
  for a sudden collapse in row count against the previous run.
- **Advisory lock.** Take a Postgres advisory lock around each cycle so two instances never poll
  concurrently.
- **Poll window.** Derive it from `v_tournament_editions` (`status = 'ongoing'`) plus today's
  scheduled match times. No ongoing edition → do not poll at all.
- **Backoff.** Exponential on 429/5xx, with jitter. Respect the tier's per-minute limit.
- **Raw payload retention.** Persist the raw response body (score fields stripped) per run, or at
  least on error, so a parse bug can be replayed without waiting for another live match.

> **Revised 2026-08-24.**
> - **Zero-rows guard needs a second, sharper condition.** The feed is worldwide, so literal
>   `rows_parsed == 0` only ever means the API broke. The condition that actually protects a card is
>   **`rows_in_scope == 0`** while an ongoing edition says otherwise. Record both on the run.
> - **Advisory lock** must be `pg_try_advisory_lock`, never the blocking variant — a second instance
>   should exit, not queue behind the first and then poll immediately after it.
> - **Poll window matters more on the free tier than this implies.** At 100 requests/day, polling an
>   empty overnight court is quota we cannot spare, so the window check must run *before* the HTTP
>   call, not around it.
>
> **Revised 2026-08-25 — do not derive the window from `v_tournament_editions`.** The original text
> says to, and it is the wrong source. Our calendar holds **25 editions, April to November**, where
> the real ATP tour runs January to November across 60+ events. Gating on it would take the poller
> dark for entire months while tracked players were on court, and it would fail *silently* — the
> worst possible failure for this job, and a direct violation of rule 4 in spirit. (A query over our
> calendar calls 53% of 2026 "dark"; almost all of that is missing data, not absent tennis.)
>
> Derive the window from the **source's fixture feed** instead — see §6. Our editions table can stay
> as a secondary sanity signal, but it must never be the gate.
> - **Pagination.** `limit` caps at 200 and `meta.has_more` is real; a slam day can exceed one page,
>   and each extra page is another request against the daily budget.

### 6 · Scheduling

`internal/updater/live`, implementing `updater.Updater`, registered in `cmd/server/main.go` beside
`tournamentsUpdater` and `playersUpdater`.

**`scheduler.New` calls `cron.New()` without `cron.WithSeconds()`** — minute granularity only. A
60-second interval is fine with the existing wrapper; anything sub-minute needs `WithSeconds()` or a
plain ticker goroutine. Note that `UPDATE_TIMEOUT` defaults to 5m, which is longer than the poll
interval; give this job its own shorter timeout or a slow API call will overlap the next run.

> **Revised 2026-08-25 — smart polling. This replaces "one cron at a fixed interval".**
>
> The principle: **a cron tick is cheap, an API request is not.** So tick often on a fixed schedule
> and decide *in Postgres, with no network call*, whether this particular tick should spend a
> request. Most ticks spend nothing. No dynamic rescheduling is needed and the existing
> `internal/scheduler` wrapper is enough — which is why this shape was chosen over a self-adjusting
> cron.
>
> Two registered jobs, not one.
>
> ---
>
> #### Job A — `live-schedule`, the cheap one
>
> Cron `0 */8 * * *` (3×/day, ~9 requests). Answers "when do our players play?"
>
> 1. Guard on `LIVE_POLL_ENABLED`; take a **separate** advisory lock from Job B.
> 2. Read the tracked players' external keys from `external_ids` (101 today).
> 3. Batch into groups of **50** — the `player=` filter's documented maximum — and call
>    `GET /matches?status=upcoming&player=…&limit=200` per batch. **3 requests.** Follow
>    `meta.has_more` if set.
> 4. Drop doubles. Resolve both player ids; keep any fixture with **at least one** tracked player.
> 5. Upsert into `live_schedule` keyed on `(source, external_key)`, and delete rows whose
>    `event_date` is more than a day past.
> 6. Record the run, `requests_made` included.
>
> A fixture with `scheduled_time = null` is a **real state**, not a gap — the order of play has not
> been published yet, usually until the evening before. Store it with `event_date` only; step 3 of
> Job B treats it as "watch that whole day".
>
> ---
>
> #### Job B — `live`, the adaptive one
>
> Cron `*/5 * * * *` — the finest interval we would ever want. Every tick:
>
> 1. **Enabled?** `LIVE_POLL_ENABLED` false → return. No request.
> 2. **Lock.** `pg_try_advisory_lock`; not acquired → return. No request.
> 3. **Decide the mode — DB only, still no request.** First match wins:
>
>    | Mode | Condition | Then |
>    |---|---|---|
>    | **ACTIVE** | any match we flipped is currently `live`, or is counting absences toward its exit | poll |
>    | **WATCHING** | a `live_schedule` row with `scheduled_at` in `[now − 6h, now + 30m]`, or a date-only row for today | poll |
>    | **STALE-SAFE** | last successful Job A run is older than 12h | poll at the floor interval — **fail open** |
>    | **ASLEEP** | none of the above | **return without calling the API** |
>
>    The **−6h tail** is not padding: a scheduled time is when a match may *begin*, and it begins
>    when the previous match on that court ends. Sampled boards showed fixtures still `upcoming`
>    4h44m past their nominal slot. ACTIVE also holds the poller open independently of the clock, so
>    a five-setter cannot be abandoned mid-match.
>
> 4. **Quota governor — should *this* tick spend a request?**
>
>    ```
>    spent      = sum(requests_made) from live_ingest_runs today
>    reserve    = Job A's remaining calls + 15% retry headroom
>    left       = LIVE_DAILY_QUOTA − spent − reserve
>    watch_mins = minutes still to be watched today (live_schedule + anything ACTIVE)
>    need       = clamp(watch_mins / max(left, 1), FLOOR=5m, CEIL=20m)
>    poll only if (now − last poll) >= need
>    ```
>
>    This is what makes the interval self-tuning. A quiet day with two matches polls every 5 minutes;
>    a US Open day with ten of our players across a twelve-hour double session stretches itself
>    toward 10–12 minutes automatically. **The quota cannot be overrun**, and no manual tier switch
>    is needed. If `left` hits zero, log loudly — that is a real incident, not a quiet degradation.
>
> 5. **Poll.** From here the cycle is exactly §§2–7 as written: fetch, parse, scope filter, resolve,
>    append, derive, emit.
> 6. **Close the run** with `requests_made`, and on a skip write `skipped_reason` so an asleep tick
>    reads differently from a failure.
>
> ---
>
> #### What it buys
>
> | | Requests/day | Card appears |
> |---|---|---|
> | Blind 16h window at `*/10` | 96 of 100 | 0–10 min late |
> | Fixture-driven, quiet day | ~54 | **0–5 min late** |
> | Fixture-driven, slam day | ~78 | 0–10 min late |
>
> Fewer requests *and* lower latency, because the quota stops being spent on empty hours and gets
> spent on the hours a card is actually at stake. The free tier stops being the binding constraint.
>
> #### Failure modes to build against
>
> - **Never fail closed.** If Job A fails repeatedly the poller must not sleep forever — hence
>   STALE-SAFE. A silent permanent sleep is the same class of bug as rule 4's silent empty parse.
> - **Schedules slip constantly.** Generous head start, very generous tail, and ACTIVE overriding
>   the clock. Never bound the window by a computed "expected end".
> - **A fixture for an unmapped player** still counts for the window — one side resolving is enough
>   to be worth watching, and the lazy resolver (§3) may map the other side once it appears live.
> - **Everything is UTC.** `scheduled_time` and `start_time` are UTC, and `tournament_editions` has
>   no timezone column, so no local-time arithmetic is needed anywhere. Do not introduce any.

### 7 · Emit transition events

The pusher (Task 2) reacts to state changes, not to poll cycles. Emit an event on
`→ live`, `→ finished`, `→ suspended`: an outbox table or `pg_notify`, your call — pick one and say
why in a comment. Do **not** make the pusher poll the poller.

> **Revised 2026-08-24 — decided: outbox table.** Railway redeploys on every push, so `LISTEN`
> connections die in that gap and anything fired inside it is gone with no record it existed — the
> exact failure that leaves a user with no card and no way to find out why. A table survives the
> restart, gives at-least-once delivery with an explicit claim, and is replayable. Cost is one table
> and a sweep of consumed rows.

---

## Endpoints

### Build now

`POST /v1/dev/matches/{id}/live` and `POST /v1/dev/matches/{id}/finish` — env-gated
(`DEV_ENDPOINTS_ENABLED`, default off), flip a match by hand.

This is the highest-value item in the whole task and should land **first**. It unblocks all iOS work
before the API integration is finished, and it stays the only way to force a match live on demand for
testing afterwards.

### Design the shape, build in Task 2

`GET /v1/users/me/live-matches` — the client's launch-time reconciliation. Under the existing
`requireUser` subtree, so `userID(r)` applies.

Returns the **neutral match shape** (`sides`, as `/v1/matches` and `weekly_highlights` already do)
for live matches among the caller's follows — **with `sets`, `score_text` and `live` omitted.** A
field that exists is a field that leaks the day someone renders it by accident.

Server-owned rules for this response, matching what the client expects:

- A user following **both** players in a match gets **one** entry, not two.
- Cap the number of entries per user (the client caps concurrent activities).
- Doubles: `sides[].players` is an array and the draw contains doubles. Either handle two surnames a
  side or exclude doubles — but decide, rather than meeting it as an index panic.

Remember to document whatever you add in `README.md`.

---

## Configuration

New env vars, `internal/config`, following the existing `envOrDefault` pattern:

| Var | Purpose |
|---|---|
| `LIVE_API_BASE_URL` | from the source's docs |
| `LIVE_API_KEY` | secret; Railway env var only — `.env` is gitignored, keep it that way |
| `LIVE_POLL_INTERVAL` | duration, e.g. `60s`; do not hard-code |
| `LIVE_POLL_ENABLED` | kill switch that does not need a redeploy |
| `DEV_ENDPOINTS_ENABLED` | gates the dev triggers, default off |

> **Revised 2026-08-24.** Concrete values, and one substitution:
>
> | Var | Default | Note |
> |---|---|---|
> | `LIVE_API_BASE_URL` | `https://api.livetennisapi.com/api/public/v1` | verified |
> | `LIVE_API_KEY` | — | free tier; Railway env only, never `.env` |
> | `LIVE_CRON` | `*/10 * * * *` | **replaces `LIVE_POLL_INTERVAL`** — consistent with `TOURNAMENTS_CRON` / `PLAYERS_CRON`, expresses both tiers |
>
> **Revised 2026-08-25 — smart polling (§6) changes this again.** `LIVE_CRON` becomes the *tick*
> rate rather than the poll rate, since most ticks spend nothing:
>
> | Var | Default | Note |
> |---|---|---|
> | `LIVE_CRON` | `*/5 * * * *` | Job B tick. Actual request rate is set by the governor |
> | `LIVE_SCHEDULE_CRON` | `0 */8 * * *` | Job A — refresh our players' fixtures, 3 requests/run |
> | `LIVE_DAILY_QUOTA` | `100` | the governor's budget; raise to `1000` on Basic and it retunes itself |
> | `LIVE_MIN_INTERVAL` | `5m` | floor — never poll faster |
> | `LIVE_MAX_INTERVAL` | `20m` | ceiling — never stretch further, even if the budget says it could |
> | `LIVE_WATCH_LEAD` | `30m` | how early a fixture opens the window |
> | `LIVE_WATCH_TAIL` | `6h` | how long after a scheduled time a fixture keeps it open |
> | `LIVE_POLL_ENABLED` | `false` | checked per run, not at registration |
> | `LIVE_UPDATE_TIMEOUT` | shorter than the cron period | the shared `UPDATE_TIMEOUT` is 5m, longer than the interval, so a slow call would overlap the next run |
> | `DEV_ENDPOINTS_ENABLED` | `false` | unchanged |
>
> Note `LIVE_POLL_ENABLED` is not quite the redeploy-free kill switch the table claims — changing a
> Railway env var triggers a redeploy. It is still worth having as the cheapest possible stop.

---

## Testing

This task adds the repo's first tests. That is deliberate: a source's payload shape *will* change, and
a fixture-backed parse test is the only thing that turns that from an outage into a failing test.

- Save a real `GET /matches?status=live` response into `testdata/` and write parse tests against it.
  Capture it during a live session — the US Open fortnight starting next week is the best window,
  with many concurrent matches and long days.
- Test the debounce derivation directly: on/off/on must stay live; three absences must end it.
- Test the zero-rows guard: an empty parse during an active window must produce an error, not a sweep.
- Test resolution against deliberately messy names (`"J. Sinner"`, `"Sinner J."`, diacritics).

`go test ./internal/livesource -run TestName` per the repo's stated convention.

---

## Definition of done

```bash
go build ./...
go vet ./...
gofmt -l <files you touched>          # must be empty
go test ./...                         # the new tests pass
```

Plus, manually:

- `DEV_ENDPOINTS_ENABLED=true` + `POST /v1/dev/matches/{id}/live` flips a match, and
  `GET /v1/matches?status=live` then returns it.
- One real poll cycle against the live API writes a `live_ingest_runs` row with a non-zero
  `rows_parsed`, and any unresolved rows land in `live_unmatched` rather than aborting the run.
- `README.md` documents every endpoint added.

> **Revised 2026-08-24.** The local dev DB the manual checks need is
> `postgresql://tennis:tennis@localhost:55432/tennis` — Docker container `tennis-pg`. There is no
> `psql` on the host; use `docker exec tennis-pg psql -U tennis -d tennis`. This contradicts
> `CLAUDE.md`, which says there is no local DB setup; the repo statement is still true, the database
> is just set up outside it.
>
> Two amendments:
> - The first manual check passes today — verified against 481 completed matches.
> - The second **cannot pass** until draws are loaded. A real cycle will show a healthy
>   `rows_parsed` and `rows_matched = 0`. Add a third check to replace it once draws exist: a
>   tracked player's match transitions `scheduled → live → prior status` across three cycles, with an
>   outbox row per transition.
>
> One more, worth adding to the list: **`players.json` is currently untracked in the repo root.**
> Move it to `testdata/` or gitignore it before it is committed by accident.

---

## Questions for the human, not for you to guess

Flag these rather than picking a default:

1. Free tier or the $9.99 Basic tier for production? It decides the poll interval (~8 min vs 60 s).
2. If the API cannot express `suspended`, do rain delays get a real state or does the card just go
   stale?
3. Doubles in or out of the trigger?
4. Outbox table or `pg_notify` for transition events?

> **Revised 2026-08-24 — all four answered**, see *Status*: Free tier; the API *can* express
> suspension (`Interrupted`), so rain gets a real state; doubles out; outbox table.
>
> **Four new ones replace them**, also in *Status* and repeated here because they are the ones that
> block a plan:
>
> 1. **How do `scheduled` matches reach the database?** Nothing else matters until this is answered —
>    the ingester has nothing to flip. Rule 3 is the iOS side's, so relaxing it is the iOS side's call.
> 2. **What status is written when a match ends?** Not `completed`-with-a-score.
> 3. **Free tier or Basic for the US Open fortnight?** `*/10` over a 16-hour slam day leaves 4 spare
>    requests. Decide before 31 August.
> 4. **Backfill `players.birth_date` / `country_code`?** Free from `players.json`, belongs in
>    tennis-data-storage, and improves — but does not fix — the lazy resolver.

---

## Task 2, for context only — do not build

Design Task 1's event mechanism so this drops on top without rework:

- `device_push_tokens(user_id, push_to_start_token, env, updated_at)` — `env` matters because a
  development APNs token is indistinguishable from a production one by inspection, and the wrong host
  answers `BadDeviceToken`.
- `live_activity_sessions(id, user_id, match_id, update_token, phase, started_at, ended_at)`.
- APNs over HTTP/2 with an ES256 JWT from a `.p8` (hourly rotation). **Direct to APNs, not via FCM** —
  the app has Firebase Analytics and Crashlytics but not Messaging, and routing through FCM would add
  a registration-token mapping table and a second failure domain for no gain.
- On `→ live`: join `follows` → `match_participants`, skip users who already hold a session for that
  match, send push-to-start.
- On `→ finished`: `event: "end"` with a `dismissal-date` and **no result in the payload**.
- A **sweeper** that force-ends any session still open after N hours. Without it, a feed outage
  mid-match leaves a LIVE card up until iOS retires it eight hours later.
- Drop tokens on APNs `410 Unregistered`.
