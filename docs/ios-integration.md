# What the iOS side has to do

The backend half of the Live Activity is finished: it detects when a followed player walks on court,
flips `matches.status`, and pushes to APNs. Nothing reaches a lock screen until the seven steps below
are done, and four of them are things only the client can supply.

Counterpart to [`live-activity-handoff.md`](live-activity-handoff.md), which is the same conversation
in the other direction.

| # | Step | Blocks | Backend side |
|---|---|---|---|
| 0 | Decide whether we may create match rows from the vendor's fixtures | **everything** | built, shipped off |
| 1 | Create the APNs key and give us four values | any push at all | waiting |
| 2 | Turn on Live Activities in the app target | any push at all | — |
| 3 | Define `ActivityAttributes` and tell us its type name | the card rendering at all | payload built, name is a config value |
| 4 | Send us the push-to-start token | any push at all | endpoint live |
| 5 | Send us each activity's own token | ending a card by push | endpoint live |
| 6 | Reconcile on launch | stale cards after an outage | endpoint live |
| 7 | Confirm two wire details | nothing, but cheap to get wrong | — |

Everything under `/v1/users/me/*` needs `Authorization: Bearer tt_…`, minted once by
`POST /v1/users` and kept in the Keychain. That already works and the app already uses it.

---

## 0. The decision that gates everything

Our `matches` table has never contained a row with status `scheduled` — the content pipeline is batch
and human-driven, so it structurally cannot publish a match *before* it is played. With nothing
scheduled, there is nothing to flip to `live`, and no card can ever appear.

The fix exists and is switched off: `LIVE_CREATE_MATCHES` lets the backend create `scheduled` rows
from the vendor's fixture feed. That relaxes rule 3 of your handoff ("never auto-create rows"), which
is why it is your call and not ours.

The guard rails, if you say yes: both players must resolve to players we already have, the tournament
must resolve to an edition we already have, `round_code` must exist in our `rounds` table, singles
only, a start time must be known, no existing match for that pair in that edition, and the row is
stamped `import_key = 'livetennisapi:<id>'` so it is always distinguishable from the pipeline's. It
only ever inserts — the one exception being the start time of its *own* rows, because the vendor
reschedules fixtures by up to a full day.

To size it: on the US Open fixture list, **23 of 59** upcoming matches had both players in our
tracked set. That is how many real cards this fortnight depends on.

---

## 1. The APNs key

In the Apple Developer account, create a **Key** with APNs enabled and download the `.p8` (it can only
be downloaded once). Then send us four things:

| We set | What it is |
|---|---|
| `APNS_KEY_PATH` | the `.p8` file itself — never in git, never in a chat log |
| `APNS_KEY_ID` | the key's ID, 10 characters |
| `APNS_TEAM_ID` | the team ID, 10 characters |
| `APNS_BUNDLE_ID` | the app's bundle identifier |

We derive the APNs topic as `<bundle-id>.push-type.liveactivity`, so the bundle id has to be exact.

`APNS_HOST` must match the environment of the tokens you send us: `api.sandbox.push.apple.com` for
development builds, `api.push.apple.com` for TestFlight and the App Store. A sandbox token sent to the
production host fails with `BadDeviceToken`, and that message explains nothing — which is why step 4
requires you to tell us which one each token is.

## 2. App setup

- `NSSupportsLiveActivities` = `true` in `Info.plist`.
- Push Notifications capability on the app target.
- Push-to-start requires **iOS 17.2+**. On anything older the card can only be started by the app
  while it is running, which defeats the point — the whole design assumes the app is closed when the
  match starts.
- Check `ActivityAuthorizationInfo().areActivitiesEnabled` before promising the user a card; the user
  can switch Live Activities off per app.

## 3. `ActivityAttributes`, and its name

This is the one step that is genuinely blocking, because iOS builds the activity from the payload we
send, and it can only decode into a type you define. Here is exactly what we send on start:

```json
{
  "aps": {
    "timestamp": 1756300000,
    "event": "start",
    "attributes-type": "MatchActivityAttributes",
    "attributes": {
      "match_id": 482,
      "edition": "us_open_2026",
      "tournament_name": "US Open",
      "round": "R128",
      "players": [
        {"side": 1, "slug": "sinner",  "name": "Jannik Sinner"},
        {"side": 2, "slug": "alcaraz", "name": "Carlos Alcaraz"}
      ]
    },
    "content-state": {"phase": "on_court"},
    "alert": {"title": "", "body": ""}
  }
}
```

So the Swift side needs to decode that shape — for example:

```swift
struct MatchActivityAttributes: ActivityAttributes {
    struct ContentState: Codable, Hashable {
        let phase: String          // "on_court" | "ended"
    }
    struct Player: Codable, Hashable {
        let side: Int
        let slug: String
        let name: String
    }
    let matchId: Int              // CodingKeys → "match_id"
    let edition: String
    let tournamentName: String    // → "tournament_name"
    let round: String
    let players: [Player]
}
```

**Tell us the type name.** `attributes-type` has to equal it exactly, and it is a config value on our
side (`APNS_ATTRIBUTES_TYPE`, currently `MatchActivityAttributes`) precisely so renaming the Swift type
is a one-line change rather than a deploy. If they ever drift apart, every push-to-start is silently
undecodable on the device — no error reaches us.

If you need a field we don't send, ask: adding one is cheap. Two things we will never send are a score
and a winner, by design — the card states that someone is on court, nothing more.

## 4. Send us the push-to-start token

Get it from `Activity<MatchActivityAttributes>.pushToStartTokenUpdates`, hex-encode the `Data`, and:

```http
PUT /v1/users/me/push-token
Authorization: Bearer tt_…
{"token": "a1b2c3…", "env": "sandbox"}
```

`env` is `"sandbox"` or `"production"` and is **required** — we refuse to guess it, because a
development token is indistinguishable from a production one by inspection. `204` means stored.

The stream re-issues the token; send it again whenever it changes. It is per install, not per match.

## 5. Send us each activity's own token

A push-to-start token can only *start* activities. To **end** one, APNs needs the token that the
started activity issues for itself — from `activity.pushTokenUpdates`. Send it keyed by the match id
you got in `attributes`:

```http
PUT /v1/users/me/live-activities/482
Authorization: Bearer tt_…
{"token": "d4e5f6…"}
```

`204` means stored; `404 no_session` means we have no open session for that match — the start push
never went out, or the card is already finished.

**If you skip this step, cards never end by push.** We keep the session, log that there was nothing to
send, and iOS retires the activity on its own hours later. That is the single most visible failure mode
of the whole feature, and it is invisible from our side.

## 6. Reconcile on launch

A Live Activity outlives the app. If the end push is lost — APNs, offline device, our outage — the card
survives the match, and nothing else will take it down.

On launch and on foreground:

```http
GET /v1/users/me/live-matches
→ {"items": [ {...} ], "total": 1, "truncated": false}
```

Dismiss any activity whose match is **not** in `items`. Each item carries `id`, `edition`,
`tournament_name`, `round`, `scheduled_at`, `court`, `status`, `surface`, `sides[].players[]` and
`started_at` — the last being when we marked it live, not the first point played. There are no `sets`,
`score_text` or `live` keys: they are absent, not null.

**Check `truncated` before dismissing anything.** It means the list was cut by a server-side fuse
(`LIVE_MATCHES_LIMIT`, default 50), so absence from `items` does *not* mean the match ended. `total` is
the real count.

## 7. Two things to confirm

- **`/v1/matches` returns `"live": {}` for a live match, not `null`.** The column is `NOT NULL`
  defaulted to `{}`, so the moment a match is live the key appears as an empty object. Nothing writes
  into it. Worth knowing before anything renders it.
- **How many concurrent cards do you want to show?** `LIVE_MATCHES_LIMIT` is a safety fuse, not a
  product cap; the client decides how many activities to hold. During a slam, a user following twenty
  players can easily have more than five matches live at once.

---

## Testing without waiting for real tennis

With `DEV_ENDPOINTS_ENABLED=true` the backend exposes hand triggers, so the full lifecycle can be
driven on demand:

| | |
|---|---|
| `POST /v1/dev/matches/{id}/live` | mark a match live — fires the same `→ live` event the poller does |
| `POST /v1/dev/matches/{id}/finish` | end it — fires `→ finished` |
| `GET /v1/dev/matches/{id}/live-state` | what we think the match's state is |
| `POST /v1/dev/live/ingest` | replay a saved vendor board through the real ingest path, zero API quota |

Both mutations are idempotent, and `409 not_flippable` means the match isn't a `scheduled` row without
a winner — use the fixtures in `db/dev_fixtures.sql`. This is how the client can be built and debugged
before step 0 is decided and before the vendor integration is switched on.

## What the backend does not do yet

- **Rain delays do nothing.** The ingester records a `suspended` state and emits an event, but the
  pusher takes no action on it, because nobody has decided what the card should say. Also unverified:
  whether the vendor keeps an interrupted match on its live board at all.
- **No score updates, ever.** There is no update push in this design — only start and end.
