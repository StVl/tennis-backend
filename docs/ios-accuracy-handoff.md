# Data accuracy fixes — iOS handoff

For whoever works on [tennis-tracker](https://github.com/StVl/tennis-tracker) next.

**No iOS change is required.** Six backend fixes change *which* data the home screen, the widget
and two player endpoints return, never the *shape*: no key added, removed or renamed, no new enum
value. Every shipped build decodes the new responses exactly as before. This document is so the
app side knows what users will now see differently, how to see it locally, and what one optional
follow-up would look like.

Client references below were checked against `tennis-tracker` at `4df4968` (2026-10-26).

---

## What changed, and what users see

| # | Surface | Before | Now |
|---|---|---|---|
| 1 | `your_season[].next_match`, widget rows, `tournaments[].next_match`, `GET /v1/players/{slug}?include=next_match` | A match that just ended stayed the player's *next* match, `status: "scheduled"`, for up to 12 h | Gone from all of them as soon as the live ingester sees it end |
| 1 | `featured_tournament.stage_label` (`"Round of 32 · Day 2"`) | Could stick on an early round for the whole tournament | Earliest round still to play, same cut-offs as `next_match` |
| 2 | `GET /v1/players/{a}/h2h/{b}` | Walkovers counted as wins/losses | Walkovers stay in `matches` but not in `wins`/`losses`, as ATP/WTA count them. `wins + losses` can now be less than `matches.count` |
| 3 | `featured_tournament` | "Day N of M", "Starts tomorrow" and which tournament counts as ongoing were by UTC date | By the date in `?tz=`, the same date `tournaments[].day` already used. The two can no longer disagree in one response |
| 4 | `featured_tournament.section_meta`, `lang=ru`, WTA | `"Swiatek победил"` | `"Swiatek победила"` |
| 5 | `featured_tournament` | With several ongoing tournaments (most weeks), an arbitrary one, possibly different per request | Singles only. Among equals: higher category, then name, the same order as the `tournaments` carousel, so it matches the carousel's first card |
| 6 | Widget `today_column`, `split` state | Cancelled matches counted as "today" and as "a followed player plays today" | Ignored |

### Why #1 happens at all

When the vendor says a match is over, the backend takes it out of `live` and puts it **back to
`scheduled`**. That's deliberate: only the content pipeline may write a result, and it can't be
invented. But the pipeline delivers results with a lag, sometimes days. Until now that window showed
a finished match as upcoming. The backend now remembers that it saw the end and hides the match.
There's still no result to show, so the row shows nothing about the match until the pipeline catches up.

### The one change users will notice — "Your week"

For the window between the match ending and the result arriving, the row now has
`next_match: null` **and** `last_match: null`. `HomeWeek.swift` falls through to `nextTournament`, then
`.idle`:

| | Before | Now |
|---|---|---|
| Match ended, result not loaded | "Plays today · 13:30" (wrong) | next upcoming tournament, or idle |
| Result loaded | won/lost with score | unchanged |

The widget does the same: the match row becomes a tournament row, or the player has no slot.

The new row is correct, but it says less than it could. If that's a problem, see
[the optional follow-up](#optional-follow-up--finished-result-coming) below.

### Things the app already does right — keep doing them

- **Render `day` / `total_days` and the featured strings as sent** (`HomeCopy.swift:177`). Fix #3
  only reaches users because the app doesn't recompute them.
- **Always send `?tz=TimeZone.current.identifier`** (`APIEndpoints.swift:161`, `:217`). Without it the
  server falls back to UTC and #3 does nothing.
- **Don't infer "finished" from `scheduled_at < now`.** Rain delays and matches with only a known date
  (00:00Z, played in the evening) make that wrong in both directions. The server decides.

---

## Seeing it locally

You need the backend running against a local database. See
[`database.md`](database.md#local-development) for building one, including `db/dev_fixtures.sql`,
which creates `sinner` vs `alcaraz` two hours from now. Start with dev routes on:

```bash
DATABASE_URL="postgresql://tennis:tennis@localhost:55432/tennis" DEV_ENDPOINTS_ENABLED=true go run ./cmd/server
```

Follow `sinner` and watch the home card and widget while driving the fixture through a match:

| Step | Request | `your_season[0].next_match` | Widget row |
|---|---|---|---|
| fresh fixture | — | the fixture, `scheduled` | match, `is_today: true` |
| on court | `POST /v1/dev/matches/{id}/live` | the fixture, `live` | match, `is_live: true` |
| match over | `POST /v1/dev/matches/{id}/finish` | **`null`**, `next_tournament` filled | **tournament** |
| reset | re-apply `db/dev_fixtures.sql` | the fixture again | match |

This sequence was run against the backend on 2026-10-27 and produced exactly these rows. Get `{id}`
from `GET /v1/matches?player=sinner&status=scheduled`. The reset also clears the fixture's `finished`
event. Without that, it would stay hidden for 7 days.

To check #3, request `/v1/home?tz=Australia/Sydney` and `?tz=UTC` during Sydney's morning. The two
differ by one day in both `featured_tournament.section_meta` and `tournaments[].day`, and inside each
response the two always agree.

---

## Optional follow-up — "Finished, result coming"

**Not built. A proposal, for when the empty row in the window above becomes a problem.**

Server: keep the finished match in `your_season[].next_match` and widget rows instead of dropping
it, with one new key:

```json
"next_match": { "match_id": 1142, "status": "scheduled", "result_pending": true, "...": "..." }
```

`result_pending` would be `true` only on a match the backend saw end, with no result yet. On every
other match it would be absent or `false`. Old builds ignore an unknown key, but they would then show
the match as upcoming again. So the server would send it **only to clients that declare support**, for
example by a query parameter, the same way `?tz=` marks the new home today. That's why it's a two-sided
change and not a backend tweak.

iOS side, if you take it:

1. `let resultPending: Bool?` on `PlayerMatchDTO` and `WidgetRowDTO` (optional, like `isLive`, for
   the same rollout reason).
2. A `WeekStatus` case for it, plus EN/RU copy ("Finished · result soon" / «Завершён · результат скоро»
   or similar).
3. **Decide what it means with hidden scores.** "Finished" with no winner leaks nothing by itself.
   But a match shown as finished next to a draw whose next round is already filled does. The
   hidden-result rules in `HomeWeek.swift` probably need to treat it like a hidden `last_match`.
4. Send the opt-in parameter.

Tell the backend which wording and opt-in you want, and the server half is small: the backend
already knows these matches, from the same check that now hides them.

---

## Where this lives in the backend

- Hiding finished matches: `notFinishedByIngest` in `internal/storage/feed.go`, used by
  `nextMatchPerPlayer` (same file), `nextMatchPerEdition` (`home_tournaments.go`) and
  `currentRoundLabel` (`featured_query.go`).
- Featured tournament: `loadFeaturedTournament` / `chooseFeatured` in
  `internal/storage/featured_query.go`, text in `featured.go`.
- H2H: `HeadToHead` in `internal/storage/players.go`.
- Widget: `widgetRows` in `internal/storage/feed.go`.
- API reference: `README.md` (home, widget, h2h sections), in Russian.
