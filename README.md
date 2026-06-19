# Meta Tracking Backend

Go backend for tracking Meta ad account spend by buyer assignment history.
Admin-connected FB profiles feed a pool of ad accounts that is snapshotted in
round-robin batches; each snapshot stores the cumulative "today so far" metrics
at an exact timestamp, and the frontend derives deltas.

## Stack

- Go 1.26+
- PostgreSQL
- Redis / Asynq
- Chi HTTP router
- JWT roles (`admin`, `buyer`)

## Quick Start

```bash
cp .env.example .env
docker compose up -d postgres redis
go mod tidy
go test ./...
go run ./cmd/api
```

Run worker:

```bash
go run ./cmd/worker
```

## Migrations

Migrations are in `migrations/` and use Goose-compatible SQL comments.

```bash
go install github.com/pressly/goose/v3/cmd/goose@latest
goose -dir migrations postgres "$DATABASE_URL" up
```

## Auth Flow

1. The first admin is inserted manually into the database.
2. Admin creates buyers via `POST /api/buyers` (buyer record + email/password
   login in one call) and extra admins via `POST /api/users`.
3. `POST /api/auth/login` returns an access token (15 min) and a refresh token
   (30 days). Refresh tokens are single-use: `POST /api/auth/refresh` rotates
   them and revokes the old one; a disabled user cannot refresh.

## Main Endpoints

- `GET /healthz`
- `POST /api/auth/login`, `POST /api/auth/refresh`
- `POST /api/users` — create account (admin)
- `POST /api/buyers` — create buyer **with CRM login** (`display_name`, `email`, `password`) (admin)
- `GET /api/buyers` (admin)
- `GET /api/fb-profiles/oauth/start` — returns the Facebook Login URL (admin)
- `GET /oauth/facebook/callback` — Facebook redirect target (no auth header; validated by signed state)
- `GET /api/fb-profiles` (admin)
- `DELETE /api/fb-profiles/{id}` — remove the connected FB profile; ad accounts and historical snapshots are preserved (admin)
- `GET /api/ad-accounts?activity_filter=all|active|inactive` — with current buyer, `last_update_at`, and `next_update_at`; buyers see only their own; default filter is `all`
- `GET /api/ad-accounts/{id}/snapshots?from=&to=&activity_filter=` — snapshots of one account; buyers only get their ownership intervals
- `GET /api/ad-accounts/{id}/assignments` — ownership history (admin)
- `POST /api/ad-accounts/{id}/assign`, `POST /api/ad-accounts/{id}/unassign` (admin)
- `PATCH /api/ad-accounts/activity-status` — bulk set `{"ad_account_ids": ["act_..."], "activity_status": "active|inactive"}` (admin)
- `GET /api/snapshots?from=&to=&buyer_id=&ad_account_id=&activity_filter=` — snapshots across accounts; buyers are always limited to their own ownership intervals
- `GET /api/alerts?limit=` (admin)

All `/api/*` endpoints require `Authorization: Bearer <jwt>`.
Swagger UI: `GET /swagger`.

## Analytics Endpoints

Raw snapshots remain available for debugging, but frontends should prefer the
analytics read-model endpoints below. They convert cumulative Meta snapshots
into interval deltas, apply buyer ownership filtering server-side, and return
derived KPIs such as CPL, CPA, ROAS, CTR, CPC, and CPM.

Common query parameters:

- `from`, `to` — RFC3339 interval bounds, required unless noted otherwise
- `timezone` — IANA timezone for bucket labels and period pacing, default `UTC`
- `granularity` — `hour` or `day`, default `hour`
- `buyer_id`, `ad_account_id` — admin filters; buyers are always forced to their own `buyer_id`
- `activity_filter` — `all`, `active`, or `inactive`; default `all`; filters account rows and KPI aggregation without affecting background sync


Update metadata contract:

- Account objects include `activity_status`, `last_update_at`, and `next_update_at`.
- Aggregate/stat JSON responses include root-level `activity_filter`, `last_update_at`, and `next_update_at`.
- `last_update_at` is the latest snapshot timestamp in the selected result set.
- `next_update_at` is an estimate based on the current round-robin cursor, `SYNC_BATCH_SIZE`, and `SYNC_BATCH_DELAY`.
- `inactive` accounts remain synced in the background; the status only affects API filtering and aggregation.

Admin examples:

```bash
curl -X PATCH "$API/api/ad-accounts/activity-status" \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"ad_account_ids":["act_1000360982703034"],"activity_status":"inactive"}'

curl -X DELETE "$API/api/fb-profiles/1" \
  -H "Authorization: Bearer $ADMIN_TOKEN"
```

Endpoints:

- `GET /api/analytics/summary` — KPI cards, account counts, freshness status
- `GET /api/analytics/timeseries` — bucketed KPI series for charts
- `GET /api/analytics/ad-accounts` — account performance table (`sort=spend_desc|roas_desc|cpl_asc`, `limit=`)
- `GET /api/analytics/ad-accounts/{id}` — one-account KPI summary, actions, time series, and admin assignment history
- `GET /api/analytics/buyers` — buyer performance leaderboard (admin only)
- `GET /api/analytics/actions` — action_type breakdown with count, value, cost per action, and share
- `GET /api/analytics/compare?compare_from=&compare_to=` — current vs previous KPI deltas
- `GET /api/analytics/pacing?budget=&period=today|week|month` — budget pacing; also accepts explicit `from`/`to`
- `GET /api/analytics/freshness` — account sync freshness and admin profile statuses
- `GET /api/analytics/issues` — actionable sync/data quality issues
- `GET /api/analytics/export.csv?group_by=ad_account|hour|day` — CSV export

## Snapshots

Every active FB profile refreshes its ad-account list automatically once per
UTC day. OAuth connect imports the initial account list immediately; afterwards
the scheduler performs the daily account-list resync at the first tick of each
new UTC date and then continues round-robin snapshot chunks until the next day.
By default the worker processes `SYNC_BATCH_SIZE=60` tracked ad accounts every
`SYNC_BATCH_DELAY=10m`; the full-cycle time shifts with the number of accounts.
The cursor is persisted in PostgreSQL, so restarts continue from the next chunk
rather than starting over.

A sync captures, per tracked ad account, the cumulative daily metrics at that
exact moment (`captured_at`): spend, impressions, clicks, reach, frequency,
cpc, cpm, ctr as typed columns in `account_stat_snapshots`, plus conversions
normalized into `snapshot_actions` (one row per `action_type` with `count` and
monetary `value`). No deltas are stored — the frontend computes them from
consecutive snapshots. `meta_date` is "today" in the ad account's own timezone.

An ad account visible from several connected profiles exists once in the pool;
exactly one profile is its primary for sync, so stats are never fetched twice.

## Buyer attribution

Admins assign/reassign ad accounts to buyers; every ownership interval is kept
in `buyer_account_history` (`assigned_at` / `unassigned_at`). Stats endpoints
for buyers only return snapshots captured inside their own intervals.

## Connecting a Meta profile (Facebook Login / dev-mode OAuth)

Profiles are connected by an **admin** through the official Facebook Login
flow, server-side: the CRM button calls `GET /api/fb-profiles/oauth/start` and
redirects the admin to the returned `auth_url`.

The Meta app runs in **Development mode**, which means no App Review or business
verification is required, but only people added to the app as
**Testers/Developers** can complete login:

1. Add the person to the Meta app (App Dashboard → App Roles → Roles → Testers)
   and have them accept the invite.
2. Admin calls `GET /api/fb-profiles/oauth/start` and follows `auth_url`.
3. They log into Facebook (an account with a role on the ad accounts) and grant
   `ads_read` (the only permission the backend needs — it reads ad accounts and
   insights, it does not manage business assets).
4. Facebook redirects to `GET /oauth/facebook/callback`, which exchanges the
   code for a **long-lived (~60 day)** token, stores it encrypted with its
   `token_expires_at`, and imports the profile's ad accounts into the tracking
   pool (already-known accounts are not duplicated).

Required env: `META_APP_ID`, `META_APP_SECRET`, `META_OAUTH_REDIRECT_URI` (must
exactly match a Valid OAuth Redirect URI in the Meta app), and optionally
`META_OAUTH_SCOPES`.

Long-lived tokens last ~60 days. The worker emits a `TOKEN_EXPIRING_SOON` alert
(and Telegram message) once, 7 days before expiry; reconnect by repeating the
OAuth flow.
