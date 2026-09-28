-- +goose Up
CREATE TABLE IF NOT EXISTS bot_sessions (
    chat_id BIGINT PRIMARY KEY,
    state JSONB NOT NULL,
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMP WITH TIME ZONE NOT NULL
);

CREATE INDEX IF NOT EXISTS bot_sessions_expires_at_idx ON bot_sessions (expires_at);

-- +goose Down
-- Keep session data while stepping down through migration versions. The
-- durable processed_events migration removes this table in the same guarded
-- transaction that removes the new inbox columns; this avoids a partial
-- rollback if retryable webhook events still exist.
SELECT 1;
