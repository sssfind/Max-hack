-- +goose Up
-- +goose StatementBegin
ALTER TABLE processed_events
    ADD COLUMN id BIGSERIAL,
    ADD COLUMN event_timestamp BIGINT,
    ADD COLUMN event_key TEXT,
    ADD COLUMN payload JSONB,
    ADD COLUMN status VARCHAR(20),
    ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN received_at TIMESTAMP WITH TIME ZONE,
    ADD COLUMN updated_at TIMESTAMP WITH TIME ZONE,
    ADD COLUMN processing_started_at TIMESTAMP WITH TIME ZONE,
    ADD COLUMN completed_at TIMESTAMP WITH TIME ZONE,
    ADD COLUMN failed_at TIMESTAMP WITH TIME ZONE,
    ADD COLUMN next_attempt_at TIMESTAMP WITH TIME ZONE,
    ADD COLUMN last_error TEXT;

-- Existing rows came from the old "insert means processed" implementation.
-- Keep them as completed deduplication records; they cannot be replayed because
-- their original payload was not stored.
UPDATE processed_events
SET event_timestamp = timestamp,
    event_key = 'legacy:' || md5(chat_id::text || ':' || timestamp::text || ':' || update_type),
    payload = '{}'::jsonb,
    status = 'completed',
    received_at = COALESCE(created_at, NOW()),
    updated_at = COALESCE(created_at, NOW()),
    completed_at = COALESCE(created_at, NOW());

ALTER TABLE processed_events
    ALTER COLUMN id SET NOT NULL,
    ALTER COLUMN event_timestamp SET DEFAULT 0,
    ALTER COLUMN event_timestamp SET NOT NULL,
    -- Defaults keep the previous application version operational during a
    -- deployment rollback. New code always supplies its stable event key and
    -- payload explicitly.
    ALTER COLUMN event_key SET DEFAULT ('legacy:' || md5(random()::text || clock_timestamp()::text || pg_backend_pid()::text)),
    ALTER COLUMN event_key SET NOT NULL,
    ALTER COLUMN payload SET DEFAULT '{}'::jsonb,
    ALTER COLUMN payload SET NOT NULL,
    ALTER COLUMN status SET DEFAULT 'completed',
    ALTER COLUMN status SET NOT NULL,
    ALTER COLUMN received_at SET DEFAULT NOW(),
    ALTER COLUMN received_at SET NOT NULL,
    ALTER COLUMN updated_at SET DEFAULT NOW(),
    ALTER COLUMN updated_at SET NOT NULL;

ALTER TABLE processed_events ADD CONSTRAINT processed_events_status_check
    CHECK (status IN ('received', 'processing', 'completed', 'failed', 'dead'));
ALTER TABLE processed_events ADD CONSTRAINT processed_events_attempts_check
    CHECK (attempts >= 0);

-- Keep the original (chat_id, timestamp, update_type) primary key deliberately:
-- the previous application version names it in ON CONFLICT during rollback.
-- New code uses the independent stable event_key uniqueness below, but also
-- stores the original MAX timestamp in the first event's legacy tuple. If two
-- distinct stable events share that tuple, later events use deterministic
-- alternate values while event_timestamp retains the real source time.
CREATE UNIQUE INDEX processed_events_id_idx ON processed_events (id);
CREATE UNIQUE INDEX processed_events_event_key_idx ON processed_events (event_key);
CREATE INDEX processed_events_due_idx
    ON processed_events (status, next_attempt_at, event_timestamp, id)
    WHERE status IN ('received', 'processing', 'failed');
CREATE INDEX processed_events_chat_pending_idx
    ON processed_events (chat_id, event_timestamp, id)
    WHERE status IN ('received', 'processing', 'failed');
CREATE INDEX processed_events_completed_at_idx
    ON processed_events ((COALESCE(completed_at, updated_at)))
    WHERE status = 'completed';
CREATE INDEX processed_events_failed_at_idx
    ON processed_events (failed_at)
    WHERE status IN ('failed', 'dead');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Serialize the final guard with webhook persistence. Without this lock, a
-- new received row could commit after the EXISTS snapshot and before ALTER
-- TABLE obtains its own exclusive lock, losing an already-acknowledged event.
LOCK TABLE processed_events IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM processed_events
        WHERE status IN ('received', 'processing', 'failed')
    ) THEN
        RAISE EXCEPTION
            'cannot remove durable processed_events columns while retryable webhook events exist';
    END IF;
END
$$;

DROP TABLE IF EXISTS bot_sessions;

DROP INDEX IF EXISTS processed_events_failed_at_idx;
DROP INDEX IF EXISTS processed_events_completed_at_idx;
DROP INDEX IF EXISTS processed_events_chat_pending_idx;
DROP INDEX IF EXISTS processed_events_due_idx;
DROP INDEX IF EXISTS processed_events_event_key_idx;
DROP INDEX IF EXISTS processed_events_id_idx;

ALTER TABLE processed_events DROP CONSTRAINT IF EXISTS processed_events_attempts_check;
ALTER TABLE processed_events DROP CONSTRAINT IF EXISTS processed_events_status_check;

ALTER TABLE processed_events
    DROP COLUMN last_error,
    DROP COLUMN next_attempt_at,
    DROP COLUMN failed_at,
    DROP COLUMN completed_at,
    DROP COLUMN processing_started_at,
    DROP COLUMN updated_at,
    DROP COLUMN received_at,
    DROP COLUMN attempts,
    DROP COLUMN status,
    DROP COLUMN payload,
    DROP COLUMN event_key,
    DROP COLUMN event_timestamp,
    DROP COLUMN id;
-- +goose StatementEnd
