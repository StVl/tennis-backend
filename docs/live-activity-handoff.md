# Handoff — Live Activity, Task 1 done and Task 2 open

**For:** whoever picks up the push side, and the iOS side who owns two of the decisions below.
**Written by:** the backend, after implementing Task 1 of `docs/live-status-ingest.md`.
**Counterpart to:** `docs/live-status-ingest.md`, which is still the authority on *why*. This file
says what exists, what does not, and what will bite you.

---

## Read this first

**Task 1 is complete and Task 2 is untouched.** The ingester decides `matches.status` and writes a
row to the `live_events` outbox on every transition. **Nothing reads that outbox.** There is no APNs
code, no device token, no session table. A card has never been sent.

**Two things stand between this and a real card, and neither is code:**

1. `LIVE_CREATE_MATCHES` is off, so nothing gets flipped. Every match in the database is `completed`;
   no row has ever been `scheduled`. The ingester resolves both players correctly, looks for the
   match they are playing, and finds nothing. Turning the flag on lets Job A create rows from the
   vendor's fixture feed — which relaxes rule 3, and rule 3 belongs to the iOS side.
2. Nine of thirteen edition mappings are unconfirmed, and the US Open main-draw id is unknown. See
   *Decisions and other repos*.

**You do not have to wait for either to build Task 2.** See *Start today* below.

---

## What exists

### The pipeline

Two cron jobs, because the free tier allows 100 requests a day and a cron tick is free while a
request is not.

| | |
|---|---|
| `live-schedule` (Job A) | `0 */8 * * *`, 3 requests. Learns when tracked players play — 101 players in 3 requests via the vendor's repeatable `player=` filter — into `live_schedule`. Also prunes journals, reconciles quota against `GET /usage`, and (behind the flag) creates match rows. |
| `live` (Job B) | ticks `*/5`, spends rarely. Decides **in Postgres, with no network call**, whether the tick deserves a request. Most ticks spend nothing. |

Identity resolution is a primary-key hit on `external_ids` — the payload carries the vendor's player
ids, so no name matching in the hot path. 101 of 102 tracked players are seeded.

Entering live takes one `on_court` observation. Leaving takes an explicit finish, three consecutive
misses, or max-live-age. The asymmetry is the whole design: a lingering card costs nothing, a false
LIVE is a push that cannot be recalled.

### Tables (`db/live_ingest.sql`)

`live_ingest_runs` · `live_observations` · `live_flags` · `live_schedule` · `live_unmatched` ·
`live_events` · `live_resolve_attempts`

`live_flags` is the one to understand: one row per match **we** hold live, carrying what to restore
on exit and how many polls since we last saw it. `select * from live_flags` answers "which cards are
up?".

### Endpoints

| | |
|---|---|
| `GET /v1/users/me/live-matches` | the client's launch-time reconciliation. Neutral match shape with `sets`, `score_text` and `live` **omitted**, plus `total`/`truncated` so an absent match can never be read as "ended". |
| `POST /v1/dev/matches/{id}/live` · `/finish` | flip by hand. Same storage functions the poller uses. |
| `GET /v1/dev/matches/{id}/live-state` | status + our flag, `flag: null` when we do not hold it. |
| `POST /v1/dev/live/ingest` | replay a saved board through the **identical** ingest path, zero quota. |
| `GET /v1/dev/live/unmatched` | the review queue by reason, with samples and a hint. |

`/v1/dev/*` exists only under `DEV_ENDPOINTS_ENABLED`.

---

## What is left — Task 2

The handoff's own list, unchanged, plus one thing it omits.

**Token registration** — an endpoint under `requireUser` for the client to hand over its
push-to-start token, and `device_push_tokens(user_id, push_to_start_token, env, updated_at)`. The
handoff does not mention the endpoint but you obviously need one. `env` matters because a
development APNs token is indistinguishable from a production one by inspection, and the wrong host
answers `BadDeviceToken`.

**`live_activity_sessions(id, user_id, match_id, update_token, phase, started_at, ended_at)`** — so a
second push-to-start is not sent to someone who already has the card.

**APNs client** — HTTP/2, ES256 JWT from a `.p8`, rotated hourly. Direct to Apple, **not via FCM**:
the app has Firebase Analytics and Crashlytics but not Messaging, and FCM would add a
token-mapping table and a second failure domain for nothing.

**The outbox consumer** — claim rows from `live_events`. On `→ live`, join `follows` →
`match_participants` for the audience, skipping anyone who already holds a session. On `→ finished`,
an end push with a `dismissal-date` and **no result in the payload**.

**A decision on `suspended` / `resumed`.** Both events are already emitted, exactly once per
transition rather than per cycle. Whether the card reacts is a product call nobody has made. Note
that whether a suspended match even stays on the vendor's live board is **unverified** — if it drops
off, a rain delay is indistinguishable from a finish and the branch may be dead code.

**A sweeper** force-ending sessions open after N hours, so a feed outage mid-match does not leave a
card up until iOS retires it hours later.

**Drop tokens on APNs `410 Unregistered`.**

---

## Start today

The pusher does not need the vendor API, loaded draws, or the rule-3 decision. `POST
/v1/dev/matches/{id}/live` writes a real `live_events` row through the same path the poller uses:

```bash
DEV_ENDPOINTS_ENABLED=true go run ./cmd/server
curl -X POST localhost:8080/v1/dev/matches/482/live     # -> live_events: live
curl -X POST localhost:8080/v1/dev/matches/482/finish   # -> live_events: finished
```

`db/dev_fixtures.sql` creates the flippable matches — the database has none otherwise. Re-running it
refreshes their start times; it deliberately does **not** reset status, because that would strand a
`live_flags` row.

For the full cycle including debounce, `POST /v1/dev/live/ingest` replays a captured board. Three
boards without a match drive the three-miss exit. Costs nothing.

---

## Things that will bite

Each of these cost real debugging time. They are in the code with comments, but they are worth
knowing before you read it.

**`matches.live_state` is `NOT NULL` defaulted to `'{}'`.** So `/v1/matches` returns `"live": {}` —
not `null` — for anything live. Confirm with iOS before rendering it.

**`event_status` overrides `status`.** The feed genuinely emits `status: "live"` with
`event_status: "Finished"`. An unrecognised `event_status` drops the row rather than being guessed
into `on_court`, because entering live takes a single observation.

**The vendor's ids are not one-per-thing.** Players appear under several ids (Tabur `175` and `8824`);
tournaments have catalogue ids that differ from the ids in match payloads, and one tournament id can
carry both main draw and qualifying. Every mapping is N:1. Do not make any of those indexes unique.

**Advisory locks are session-scoped.** Taken through the pool, lock and unlock land on different
sessions, the lock leaks, and the job goes quiet for up to an hour looking perfectly healthy. The
code holds a dedicated connection and unlocks on a detached context; if unlock fails it destroys the
connection rather than returning it.

**A test that passes may be testing nothing.** The first advisory-lock test was vacuous — advisory
locks are re-entrant per session and the pool hands the same connection back, so a leaked lock was
invisible from one pool. It now verifies from a second. Mutation-check anything that guards a silent
failure.

**`go test ./...` is green and skips every DB-backed test** unless `TEST_DATABASE_URL` is set — **27
tests skip**, including the four covering the advisory-lock hazard. Green without it does not mean the
suite ran.

**The collapse guard cannot protect a single card.** Holding one card, `1 → 0` looks identical
whether the match ended or the vendor stopped listing it. Below three prior in-scope rows the
three-miss debounce is the protection; the guard covers many matches vanishing at once.

---

## Decisions and other repos

**Rule 3 — iOS side.** Enabling `LIVE_CREATE_MATCHES` lets this service add rows to `matches`, which
tennis-data-storage owns. Guard rails are in place (confirmed edition only, singles only, round code
must exist in `rounds`, dedupe on the player pair, insert-only, provenance stamped, retired after
48h). The decision is still not ours.

**Nine edition ids need confirming.** `db/live_edition_ids.sql` marks `confirmed_at` only on the four
ids actually observed in match payloads. The rest are catalogue-derived: the catalogue names the
tournament correctly but not each id's role (main / qualifying / doubles), and the role is what
decides which draw a match lands in. `ResolveEditions` ignores unconfirmed rows, so **the writer
currently creates almost nothing even with the flag on.** That is the intended cost of refusing to
guess.

**The US Open main-draw singles id is unknown.** Across every capture the only US Open ids seen are
qualifying and doubles. It will appear in `GET /v1/dev/live/unmatched` as `edition_unmapped` on the
first schedule refresh after the draw publishes; add one line to the seed.

**`players.birth_date` and `country_code` are NULL for all 173 rows** — tennis-data-storage. The lazy
resolver therefore matches on names alone, which is its weakest point. `players.json` already carries
birthdays for 129 of the 134 mapped players, so this costs no API calls and is the single change that
would most improve resolution.

**Scheduled matches** — tennis-data-storage, or `LIVE_CREATE_MATCHES`. Without one of them there is
nothing to flip.

**Flip latency is unmeasured** — unknown #3 from the original handoff, never closed. How long the
vendor takes to mark a match live sits underneath whatever interval we choose and cannot be derived
from `scheduled_time`, because a match starts when the previous one on that court ends.

---

## Where things live

| | |
|---|---|
| `internal/livesource/` | everything vendor-specific: wire structs, parser, HTTP client, `Source`. Swapping vendors should be a new file here. The wire structs have **no `score` field**, so score data cannot leave the parser. |
| `internal/updater/live/` | `schedule.go` (Job A), `poll.go` (Job B + `Ingest`), `decide.go` and `derive.go` — the pure decision layer, tested with no DB, network or clock. |
| `internal/storage/live*.go` | all SQL. `live.go` flips, `live_poll.go` resolves, `live_create.go` writes matches, `live_resolve.go` is the lazy resolver. |
| `db/live_*.sql` | schema and seeds. **Working copies** — they need carrying into `tennis-data-storage/db/schema.sql`. `dev_fixtures.sql` is local-only. |

Local Postgres is Docker `tennis-pg` on `:55432`; there is no `psql` on the host, so `docker exec -i`.
`README.md` documents every endpoint and all 27 env vars.
