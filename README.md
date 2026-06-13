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

1. `POST /api/auth/register` works **only on an empty database** and creates the
   first admin (role is forced to `admin`). After that it returns 403.
2. Admin creates buyers via `POST /api/buyers` (buyer record + email/password
   login in one call) and extra admins via `POST /api/users`.
3. `POST /api/auth/login` returns an access token (15 min) and a refresh token
   (30 days). Refresh tokens are single-use: `POST /api/auth/refresh` rotates
   them and revokes the old one; a disabled user cannot refresh.

## Main Endpoints

- `GET /healthz`
- `POST /api/auth/register` — bootstrap first admin only
- `POST /api/auth/login`, `POST /api/auth/refresh`
- `POST /api/users` — create account (admin)
- `POST /api/buyers` — create buyer **with CRM login** (`display_name`, `email`, `password`) (admin)
- `GET /api/buyers` (admin)
- `GET /api/fb-profiles/oauth/start` — returns the Facebook Login URL (admin)
- `GET /oauth/facebook/callback` — Facebook redirect target (no auth header; validated by signed state)
- `GET /api/fb-profiles` (admin)
- `POST /api/fb-profiles/{id}/resync` — re-pull the profile's ad accounts with the stored token and queue a stats sync (admin)
- `GET /api/ad-accounts` — with current buyer; buyers see only their own
- `GET /api/ad-accounts/{id}/snapshots?from=&to=` — snapshots of one account; buyers only get their ownership intervals
- `GET /api/ad-accounts/{id}/assignments` — ownership history (admin)
- `POST /api/ad-accounts/{id}/assign`, `POST /api/ad-accounts/{id}/unassign` (admin)
- `PATCH /api/ad-accounts/{id}` — `{"is_tracked": false}` (admin)
- `GET /api/snapshots?from=&to=&buyer_id=&ad_account_id=` — snapshots across accounts; buyers are always limited to their own ownership intervals
- `GET /api/alerts?limit=` (admin)

All `/api/*` endpoints require `Authorization: Bearer <jwt>`.
Swagger UI: `GET /swagger`.

## Snapshots

Every active FB profile is synced in round-robin chunks to stay under Meta
Marketing API development-tier limits. By default the worker processes
`SYNC_BATCH_SIZE=60` tracked ad accounts every `SYNC_BATCH_DELAY=10m`; with 543
accounts that is roughly a 100 minute full cycle. The cursor is persisted in
PostgreSQL, so restarts continue from the next chunk rather than starting over.

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
