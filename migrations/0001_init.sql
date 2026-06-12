-- +goose Up
CREATE TABLE buyers (
    id BIGSERIAL PRIMARY KEY,
    crm_external_id TEXT UNIQUE,
    display_name TEXT NOT NULL,
    email TEXT,
    telegram_username TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE app_users (
    id BIGSERIAL PRIMARY KEY,
    email TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('admin', 'buyer')),
    buyer_id BIGINT REFERENCES buyers(id) ON DELETE SET NULL,
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (role <> 'buyer' OR buyer_id IS NOT NULL)
);

CREATE INDEX app_users_buyer_id_idx ON app_users(buyer_id);

CREATE TABLE refresh_tokens (
    jti TEXT PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX refresh_tokens_user_idx ON refresh_tokens(user_id);

CREATE TABLE fb_profiles (
    id BIGSERIAL PRIMARY KEY,
    fb_user_id TEXT NOT NULL UNIQUE,
    fb_name TEXT NOT NULL,
    access_token_ciphertext TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'checkpoint', 'expired', 'disabled')),
    status_message TEXT,
    token_expires_at TIMESTAMPTZ,
    token_expiry_alert_sent_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE ad_accounts (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    account_status INT,
    currency TEXT,
    timezone_name TEXT,
    is_tracked BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- An ad account can be visible from several connected FB profiles; exactly one
-- of them is the primary used for hourly sync, so stats are never fetched twice.
CREATE TABLE fb_profile_ad_accounts (
    fb_profile_id BIGINT NOT NULL REFERENCES fb_profiles(id) ON DELETE CASCADE,
    ad_account_id TEXT NOT NULL REFERENCES ad_accounts(id) ON DELETE CASCADE,
    is_primary_for_sync BOOLEAN NOT NULL DEFAULT FALSE,
    access_status TEXT NOT NULL DEFAULT 'active',
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (fb_profile_id, ad_account_id)
);

CREATE UNIQUE INDEX fb_profile_ad_accounts_one_primary
    ON fb_profile_ad_accounts(ad_account_id)
    WHERE is_primary_for_sync;

-- Who owned the ad account and when; buyers may only read stats captured
-- inside their own [assigned_at, unassigned_at) intervals.
CREATE TABLE buyer_account_history (
    id BIGSERIAL PRIMARY KEY,
    ad_account_id TEXT NOT NULL REFERENCES ad_accounts(id) ON DELETE CASCADE,
    buyer_id BIGINT NOT NULL REFERENCES buyers(id) ON DELETE RESTRICT,
    assigned_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    unassigned_at TIMESTAMPTZ,
    CHECK (unassigned_at IS NULL OR unassigned_at > assigned_at)
);

CREATE UNIQUE INDEX buyer_account_history_one_current
    ON buyer_account_history(ad_account_id)
    WHERE unassigned_at IS NULL;

CREATE INDEX buyer_account_history_lookup
    ON buyer_account_history(ad_account_id, assigned_at, unassigned_at);

CREATE INDEX buyer_account_history_buyer_idx
    ON buyer_account_history(buyer_id, assigned_at);

-- Cumulative "today so far" metrics at the exact moment captured_at; deltas
-- between consecutive snapshots are computed by the frontend, not stored.
CREATE TABLE account_stat_snapshots (
    id BIGSERIAL PRIMARY KEY,
    ad_account_id TEXT NOT NULL REFERENCES ad_accounts(id) ON DELETE CASCADE,
    captured_at TIMESTAMPTZ NOT NULL,
    meta_date DATE NOT NULL,
    spend NUMERIC(14, 2) NOT NULL DEFAULT 0,
    impressions BIGINT NOT NULL DEFAULT 0,
    clicks BIGINT NOT NULL DEFAULT 0,
    reach BIGINT NOT NULL DEFAULT 0,
    frequency NUMERIC(10, 4) NOT NULL DEFAULT 0,
    cpc NUMERIC(14, 6) NOT NULL DEFAULT 0,
    cpm NUMERIC(14, 6) NOT NULL DEFAULT 0,
    ctr NUMERIC(10, 6) NOT NULL DEFAULT 0,
    raw JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (ad_account_id, captured_at)
);

CREATE INDEX account_stat_snapshots_day
    ON account_stat_snapshots(ad_account_id, meta_date, captured_at DESC);

CREATE TABLE snapshot_actions (
    id BIGSERIAL PRIMARY KEY,
    snapshot_id BIGINT NOT NULL REFERENCES account_stat_snapshots(id) ON DELETE CASCADE,
    action_type TEXT NOT NULL,
    count NUMERIC(14, 2) NOT NULL DEFAULT 0,
    value NUMERIC(14, 2) NOT NULL DEFAULT 0,
    UNIQUE (snapshot_id, action_type)
);

CREATE TABLE alerts (
    id BIGSERIAL PRIMARY KEY,
    type TEXT NOT NULL,
    fb_profile_id BIGINT REFERENCES fb_profiles(id) ON DELETE SET NULL,
    ad_account_id TEXT REFERENCES ad_accounts(id) ON DELETE SET NULL,
    buyer_id BIGINT REFERENCES buyers(id) ON DELETE SET NULL,
    message TEXT NOT NULL,
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    sent_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS alerts;
DROP TABLE IF EXISTS snapshot_actions;
DROP TABLE IF EXISTS account_stat_snapshots;
DROP TABLE IF EXISTS buyer_account_history;
DROP TABLE IF EXISTS fb_profile_ad_accounts;
DROP TABLE IF EXISTS ad_accounts;
DROP TABLE IF EXISTS fb_profiles;
DROP TABLE IF EXISTS refresh_tokens;
DROP TABLE IF EXISTS app_users;
DROP TABLE IF EXISTS buyers;
