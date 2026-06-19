-- +goose Up
ALTER TABLE ad_accounts
    ADD COLUMN activity_status TEXT NOT NULL DEFAULT 'active'
        CHECK (activity_status IN ('active', 'inactive'));

CREATE INDEX ad_accounts_activity_status_idx ON ad_accounts(activity_status);

ALTER TABLE fb_profiles
    ADD COLUMN last_account_resync_at TIMESTAMPTZ,
    ADD COLUMN last_account_resync_date DATE,
    ADD COLUMN last_account_resync_error TEXT;

-- +goose Down
ALTER TABLE fb_profiles
    DROP COLUMN IF EXISTS last_account_resync_error,
    DROP COLUMN IF EXISTS last_account_resync_date,
    DROP COLUMN IF EXISTS last_account_resync_at;

DROP INDEX IF EXISTS ad_accounts_activity_status_idx;

ALTER TABLE ad_accounts
    DROP COLUMN IF EXISTS activity_status;
