-- +goose Up
CREATE TABLE sync_profile_cursors (
    fb_profile_id BIGINT PRIMARY KEY REFERENCES fb_profiles(id) ON DELETE CASCADE,
    next_offset INT NOT NULL DEFAULT 0 CHECK (next_offset >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS sync_profile_cursors;
