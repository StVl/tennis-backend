# Live Activity — implementation handoff

For an agent implementing the iOS side in [tennis-tracker](https://github.com/StVl/tennis-tracker).

Every field name, JSON key, status code and default below is copied from the backend source on
`main`, not from other documents. **Do not cross-check this against `README.md` or
`docs/live-activity-handoff.md`** — each of those contains at least one statement that is now wrong,
and they are noted at the end. `docs/live-flow.md` is accurate but describes backend internals.

The backend side is finished and has run in production: 30 cards raised, 30 retired, none stuck.

---

## What you are building

When a player the user follows walks on court, the backend sends an APNs **push-to-start**. iOS
creates a Live Activity from it with no app code running. When the match ends, the backend sends an
**end push** and the card goes away.

The card shows **presence only — never a score.** That is a hard design rule enforced by types and
tests on the backend; no payload will ever carry a score, and the end push does not even carry the
winner. Do not design a UI that needs either.

---

## 1. The one coupling that silently breaks everything

The push carries `"attributes-type": "MatchActivityAttributes"` as a **string**. iOS uses it to pick
the Swift type to construct the activity from. If your type name differs, every push-to-start is
silently undecodable — **no error surfaces on either side.**

So the type must be named exactly `MatchActivityAttributes`, and it must be resolvable under that
runtime name. If you need a different name, the backend's `APNS_ATTRIBUTES_TYPE` must change to match.

## 2. The Swift models

Payload keys are `snake_case`, so `CodingKeys` are required. This compiles:

```swift
import ActivityKit

struct MatchActivityAttributes: ActivityAttributes {
    public struct ContentState: Codable, Hashable {
        /// Only ever "on_court" or "ended". Nothing else is sent, ever.
        var phase: String
    }

    let matchId: Int
    let edition: String
    let tournamentName: String
    let round: String
    let players: [Player]

    enum CodingKeys: String, CodingKey {
        case matchId = "match_id"
        case edition
        case tournamentName = "tournament_name"
        case round
        case players
    }

    struct Player: Codable, Hashable {
        let side: Int
        let slug: String
        let name: String
    }
}
```

`ContentState` must decode **both** `{"phase": "on_court"}` and `{"phase": "ended"}` — the same type
handles start and end, so no field may be non-optional unless both pushes send it. `phase` is the
only key either one sends.

Every field in `attributes` is backed by a `NOT NULL` column: nothing is optional, nothing is `null`.
`players` is an array (never `null`), always 2 in practice because doubles are dropped upstream —
but decode it as `[Player]` and render defensively rather than assuming a pair.

## 3. The two payloads, verbatim

**Start:**

```json
{"aps": {
  "timestamp": 1788609600,
  "event": "start",
  "attributes-type": "MatchActivityAttributes",
  "attributes": {
    "match_id": 1,
    "edition": "barcelona_2026",
    "tournament_name": "Barcelona Open Banc Sabadell",
    "round": "F",
    "players": [
      {"side": 1, "slug": "fils",   "name": "Arthur Fils"},
      {"side": 2, "slug": "rublev", "name": "Andrey Rublev"}
    ]
  },
  "content-state": {"phase": "on_court"},
  "alert": {"title": "", "body": ""}
}}
```

**End:**

```json
{"aps": {
  "timestamp": 1788613200,
  "event": "end",
  "dismissal-date": 1788613230,
  "content-state": {"phase": "ended"}
}}
```

`timestamp` and `dismissal-date` are Unix epoch **seconds**. The end push carries no `attributes`,
no winner, no score — `phase: "ended"` is its entire information content.

`dismissal-date` is `now + PUSH_DISMISS_AFTER`, **30 seconds** by default. So the ended state is a
half-minute window before iOS removes the card — worth knowing before you invest much design or
localisation in it. During testing, a card vanishing about 30 s after it says "finished" is correct.

Note also that `phase: "ended"` does **not** mean the match is over. `sweepStale` force-ends sessions
past `PUSH_MAX_SESSION_AGE` (6h), so a long five-setter gets the same end push while still on court.
Treat it as *"stop showing this"*, not *"the match finished"* — and prefer neutral copy.

The empty `alert` is deliberate and required; it is what makes iOS treat this as a push-to-start
rather than dropping it. `README.md` shows this payload without it — `README.md` is wrong.

**There is no update push.** `suspended` and `resumed` events are consumed by the backend with no
action, so during a rain delay the card keeps saying `on_court` and receives nothing. Do not build a
`phase: "suspended"` branch and wait for it.

Headers, for reference: `apns-push-type: liveactivity`, `apns-topic: <bundle-id>.push-type.liveactivity`,
`apns-priority: 10`, and `apns-expiration` one hour on start only. Priority is always 10 with no
low-priority path — **a device that is off for two hours loses the start push permanently**, which is
why reconciliation (§6) is not optional.

---

## 4. Endpoints

Base: the deployed backend. Everything under `/v1/users/me` requires `Authorization: Bearer tt_…`.

### Register (once, first launch)

```
POST /v1/users
{"device_id": "optional-string"}        → 201 {"user_id": "...", "token": "tt_..."}
```

The token is returned **once** — only its sha256 is stored. Keychain it.

### Follows — required for any push to arrive

```
GET    /v1/users/me/follows              → 200 {"items": [...]}
PUT    /v1/users/me/follows              → 204   (replace whole list, order preserved)
PUT    /v1/users/me/follows/{slug}       → 204   (idempotent)
DELETE /v1/users/me/follows/{slug}       → 204   (idempotent)
```

A push reaches a user only if a `follows` row links them to a player **in that match**. A registered
token with no follows receives nothing.

### Push-to-start token

```
PUT /v1/users/me/push-token
{"token": "<hex>", "env": "sandbox"}     → 204
```

From `Activity<MatchActivityAttributes>.pushToStartTokenUpdates`, hex-encoded. `env` must be
`sandbox` or `production` — anything else is `400 bad_env`. It must match the server's `APNS_HOST`:
`sandbox` for Xcode and Simulator builds, `production` for TestFlight and the App Store. A mismatch
gives `BadDeviceToken`, whose message explains nothing.

**Map it, do not pass it through.** If you resolve this from the provisioning profile,
`aps-environment` reads **`development`**, not `sandbox`. Sending it verbatim is `400 bad_env` on
every launch, no token is ever stored, and no card is ever raised — and the message says *"env must
be sandbox or production"*, which points you at the value rather than at the mapping. So:
`development` → `"sandbox"`, `production` → `"production"`.

The JSON key is **`env`**, not `environment`. A Swift property named `environment` needs a
`CodingKeys` entry to emit `env`, or the server decodes an empty string and rejects it the same way.

Errors: `400 bad_body`, `400 bad_token` (empty), `400 bad_env`.

### The activity's own token — this is what ends the card

```
PUT /v1/users/me/live-activities/{match_id}
{"token": "<hex>"}                       → 204 | 400 bad_id | 400 bad_body | 404 no_session
```

From `activity.pushTokenUpdates`. **Send it on every emission, not just the first** — it is an
unconditional update of the open session row, so re-sending is safe and overwrites.

`404 no_session` means no open session exists: either the start push has not been sent yet, or the
card already ended, or the app started the activity itself (see §7). The session row is created
immediately *before* the start push leaves the server, so the PUT can only succeed after the push
has arrived — send it from your `pushTokenUpdates` observer, not eagerly.

### Launch reconciliation

```
GET /v1/users/me/live-matches            → 200
```

```json
{"items": [{
  "id": 482,
  "edition": "us_open_2026",
  "tournament_name": "US Open",
  "round": "R128",
  "scheduled_at": "2026-08-26T16:11:03Z",
  "court": "Arthur Ashe Stadium",
  "status": "live",
  "surface": "hard",
  "sides": [{"side": 1, "players": [
      {"slug": "sinner", "name": "Jannik Sinner",
       "last_name": "Sinner", "photo_url": "https://…", "rank": 1}]}],
  "started_at": "2026-08-26T16:14:00Z"
}], "total": 1, "truncated": false}
```

**Nullable:** `scheduled_at`, `court`, `started_at`, and inside players `last_name`, `photo_url`,
`rank`. Everything else is always present. `started_at` is when *we* marked it live — not the first
point played — and is `null` for matches the content pipeline marked live independently.

`total` is the count before the limit; `truncated` says items were cut. **Absence from `items` must
never be read as "the match ended"** when `truncated` is true. An empty `items` with `total: 0` is
the normal, healthy response.

---

## 5. The sequence

1. `POST /v1/users`, store the token.
2. `PUT /v1/users/me/follows/{slug}` for at least one player.
3. **Immediately at first launch**, register the push-to-start token. Not lazily on a later screen —
   see §7.
4. A push-to-start arrives → iOS creates the activity → your `pushTokenUpdates` observer fires →
   `PUT /v1/users/me/live-activities/{match_id}` using `attributes.match_id`.
5. End push arrives → iOS dismisses at `dismissal-date`.
6. On every cold start, `GET /v1/users/me/live-matches` and end any activity whose `match_id` is not
   in `items`.

## 6. Why step 6 is mandatory

There is no delivery guarantee anywhere in this chain:

- A `finished` event with no open session is **silently consumed** — no end push is sent.
- If the update token never arrived, the backend closes the session and sends nothing. The card
  outlives the match.
- Sessions past `PUSH_MAX_SESSION_AGE` (6h) are force-closed server-side. A five-set match plus a
  rain delay can exceed that, and the real `finished` event afterwards then finds no session.
- Start pushes expire after an hour and are priority 10 only.

Treat an end push as *"stop showing this"*, not as *"the match is over"*. Set a `staleDate` on the
activity as a backstop — the backend explicitly relies on iOS eventually retiring orphans.

## 7. What will bite you

**One card per user per match.** Following both players still yields one card. Key your local
registry on `attributes.match_id` and never create a second activity for a `match_id` you already
hold — a start push can legitimately arrive twice.

**Only one device per user gets the card.** The session slot is a unique index on
`(user_id, match_id)`, claimed before sending, so with two registered tokens exactly one receives it —
the **most recently updated**. Register one token per install and re-`PUT` it whenever it changes;
that is what keeps yours the freshest.

**Re-registering a changed token adds a row, it does not replace.** The upsert key is
`(user_id, token)` and there is no unregister endpoint; dead tokens are removed only when Apple
returns 410.

**A card fired before your token is registered is lost forever.** With no matching token the backend
consumes the event without sending — it is not retried, and there is no endpoint to ask for a resend.
A match already live when the token arrives will never produce a card; it appears only in
`/v1/users/me/live-matches`.

**A match can be live with no card.** The content pipeline also sets `status='live'` independently,
and the backend only ever raises cards for matches *it* flipped. So "live in the app" and "card on
the lock screen" are independent facts — build the UI accordingly.

**A user who dismisses the card manually will not get it back** for that match.

**`round` is a raw code** (`F`, `QF`, `R128`, `R1`, `RR`, `Q1`…) and the vocabulary is **not
consistent between editions** — the same draw may use `R128/R64` in one and `R1/R2` in another. Map
defensively; never `switch` exhaustively.

**`tournament_name` is not localized.** It is plain text in one language; there is no `?lang=`
equivalent for the push.

**Card latency is not instant.** The poller runs on a quota budget of 100 vendor requests/day and the
pusher every minute, so expect minutes between walk-on and card. Do not imply real-time.

## 8. Testing without waiting for real tennis

Run the backend locally with `DEV_ENDPOINTS_ENABLED=true` and point the app at it. These routes have
**no authentication**, which is why they are off in production:

| | |
|---|---|
| `POST /v1/dev/matches/{id}/live` | fires the same `→ live` event the poller does |
| `POST /v1/dev/matches/{id}/finish` | fires `→ finished` |
| `GET /v1/dev/matches/{id}/live-state` | what the backend thinks the state is |

Use the fixtures in `db/dev_fixtures.sql`; `409 not_flippable` means the row isn't a `scheduled` match
without a winner.

**The Simulator works.** Recent Xcode/iOS mint a real ActivityKit push-to-start token in the
Simulator and a real APNs server can target it — verified end to end against this backend on
2026-09-06: card raised, activity token returned, card retired by push. You do not need a physical
device for the push loop. The Simulator's token is always **sandbox**, which matches the backend's
default `APNS_HOST`, so no environment juggling.

To isolate a rendering problem from a delivery problem, push the payload locally with no server
involved: `xcrun simctl push booted <bundle-id> payload.json` using the verbatim JSON from §3. If that
renders and a real push does not, the fault is delivery; if neither renders, it is the type name or
the widget registration.

Three things that will make you think it's broken when it isn't: a dev flip produces **no push at all**
unless `PUSH_ENABLED=true` and the four `APNS_*` variables are set (otherwise a stub sender returns
"push delivery is disabled"); the pusher runs on a cron so a flip takes **up to 60 seconds**; and a
failed push then retries every `PUSH_RETRY_AFTER` (2 min) up to `PUSH_MAX_ATTEMPTS`, after which the
event is dead and the fixture needs `/finish` then `/live` to produce a fresh one.

`APNS_HOST` needs the scheme: `https://api.sandbox.push.apple.com`.

## 9. Other documents

- `docs/live-flow.md` — accurate, backend internals. Useful context, not required reading.
- `README.md` — its start-payload example **omits `alert`**, and its claim about the `live` field is
  contradicted elsewhere. Prefer this document.
- `docs/ios-integration.md` — the human-facing predecessor. Mostly correct, but its `APNS_HOST`
  values are missing `https://` and its Swift sketch is illustrative rather than compiling.
- `docs/live-activity-handoff.md` — **superseded**, contradicts current code in three places. Ignore.
