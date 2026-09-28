-- +goose Up
-- +goose StatementBegin
-- Compatibility repair for databases that applied an early revision of
-- 20260927000001 before the terminal dead-letter state was added.
ALTER TABLE processed_events DROP CONSTRAINT IF EXISTS processed_events_status_check;
ALTER TABLE processed_events ADD CONSTRAINT processed_events_status_check
    CHECK (status IN ('received', 'processing', 'completed', 'failed', 'dead'));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- This migration is the first step of a normal DownTo rollback. Refuse before
-- any later Down migration can discard payloads that were accepted but have
-- not reached a terminal state yet. The durable migration repeats the guard
-- so a database whose current version is 20260927000001 is protected too.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM processed_events
        WHERE status IN ('received', 'processing', 'failed')
    ) THEN
        RAISE EXCEPTION
            'cannot roll back durable webhook storage while retryable events exist';
    END IF;
END
$$;
-- +goose StatementEnd
