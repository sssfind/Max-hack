package worker

import (
	"Max-hack/internal/maxapi"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type eventRef struct {
	Key    string
	ChatID int64
}

type storedEvent struct {
	Ref      eventRef
	Update   maxapi.Update
	Attempts int
}

// eventSession uses the same database connection that owns the chat advisory
// lock. This prevents a pool deadlock when every worker is processing a chat.
type eventSession interface {
	claim(context.Context, eventRef, time.Duration) (storedEvent, bool, error)
	complete(context.Context, string) error
	fail(context.Context, string, string, time.Time, bool) error
	interrupt(context.Context, string, string, time.Time) error
}

type eventStore interface {
	ready(context.Context) error
	save(context.Context, string, maxapi.Update, []byte) (eventRef, bool, error)
	withChatLock(context.Context, int64, func(eventSession) error) error
	nextDue(context.Context, int64, time.Duration) (eventRef, bool, error)
	due(context.Context, int, time.Duration) ([]eventRef, error)
	cleanup(context.Context, time.Time, time.Time) (int64, error)
}

var errChatBusy = errors.New("chat is being processed by another worker instance")

type postgresStore struct {
	db *pgxpool.Pool
}

func newPostgresStore(db *pgxpool.Pool) *postgresStore {
	return &postgresStore{db: db}
}

func (s *postgresStore) ready(ctx context.Context) error {
	var schemaReady bool
	err := s.db.QueryRow(ctx, `
		WITH target AS (
			SELECT to_regclass('processed_events') AS oid
		), required_columns(name) AS (
			VALUES
				('id'), ('event_timestamp'), ('event_key'), ('payload'), ('status'),
				('attempts'), ('received_at'), ('updated_at'), ('processing_started_at'),
				('completed_at'), ('failed_at'), ('next_attempt_at'), ('last_error')
		), required_constraints(name, required_fragment) AS (
			VALUES
				('processed_events_status_check', 'dead'),
				('processed_events_attempts_check', 'attempts')
		)
		SELECT target.oid IS NOT NULL
			AND NOT EXISTS (
				SELECT 1
				FROM required_columns required
				WHERE NOT EXISTS (
					SELECT 1
					FROM pg_attribute attribute
					WHERE attribute.attrelid = target.oid
					  AND attribute.attname = required.name
					  AND attribute.attnum > 0
					  AND NOT attribute.attisdropped
				)
			)
			AND NOT EXISTS (
				SELECT 1
				FROM required_constraints required
				WHERE NOT EXISTS (
					SELECT 1
					FROM pg_constraint constraint_record
					WHERE constraint_record.conrelid = target.oid
					  AND constraint_record.conname = required.name
					  AND constraint_record.contype = 'c'
					  AND constraint_record.convalidated
					  AND POSITION(required.required_fragment IN pg_get_constraintdef(constraint_record.oid)) > 0
				)
			)
			AND EXISTS (
				SELECT 1
				FROM pg_index index_record
				WHERE index_record.indexrelid = to_regclass('processed_events_event_key_idx')
				  AND index_record.indrelid = target.oid
				  AND index_record.indisunique
				  AND index_record.indisvalid
				  AND index_record.indisready
			)
			AND EXISTS (
				SELECT 1
				FROM pg_constraint constraint_record
				WHERE constraint_record.conrelid = target.oid
				  AND constraint_record.contype = 'p'
				  AND regexp_replace(
					pg_get_constraintdef(constraint_record.oid),
					'["[:space:]]', '', 'g'
				  ) = 'PRIMARYKEY(chat_id,timestamp,update_type)'
			)
		FROM target
	`).Scan(&schemaReady)
	if err != nil {
		return fmt.Errorf("processed events storage is not ready: %w", err)
	}
	if !schemaReady {
		return errors.New("processed events storage is not ready: durable schema migration is incomplete")
	}
	return nil
}

func (s *postgresStore) save(ctx context.Context, key string, update maxapi.Update, payload []byte) (eventRef, bool, error) {
	ref := eventRef{Key: key, ChatID: update.ResolvedChatID()}
	var shouldQueue bool
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return eventRef{}, false, fmt.Errorf("begin webhook event persistence: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	err = tx.QueryRow(ctx, `
		INSERT INTO processed_events (
			event_key, chat_id, timestamp, event_timestamp, update_type, payload, status,
			received_at, updated_at, next_attempt_at
		)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, 'received', NOW(), NOW(), NOW())
		ON CONFLICT DO NOTHING
		RETURNING event_key, chat_id,
			status = 'received'
			OR (status = 'failed' AND COALESCE(next_attempt_at, NOW()) <= NOW())
	`, key, ref.ChatID, update.Timestamp, update.Timestamp, update.UpdateType, string(payload)).Scan(
		&ref.Key, &ref.ChatID, &shouldQueue,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		// A stable key wins over the compatibility tuple. This is essential when
		// two distinct MAX events share the same source timestamp and type.
		err = tx.QueryRow(ctx, `
			SELECT event_key, chat_id,
				status = 'received'
				OR (status = 'failed' AND COALESCE(next_attempt_at, NOW()) <= NOW())
			FROM processed_events
			WHERE event_key = $1
		`, key).Scan(
			&ref.Key, &ref.ChatID, &shouldQueue,
		)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		var tupleRef eventRef
		var tupleShouldQueue bool
		err = tx.QueryRow(ctx, `
			SELECT event_key, chat_id,
				status = 'received'
				OR (status = 'failed' AND COALESCE(next_attempt_at, NOW()) <= NOW())
			FROM processed_events
			WHERE chat_id = $1 AND timestamp = $2 AND update_type = $3
		`, ref.ChatID, update.Timestamp, update.UpdateType).Scan(
			&tupleRef.Key, &tupleRef.ChatID, &tupleShouldQueue,
		)
		if err == nil && strings.HasPrefix(tupleRef.Key, "legacy:") {
			// Rows written by the old binary do not contain a platform identifier,
			// so their original tuple is the only safe cross-version identity.
			ref = tupleRef
			shouldQueue = tupleShouldQueue
		} else if err == nil {
			// A different stable key owns the legacy tuple. Persist this event with
			// a deterministic alternate tuple while event_timestamp retains the
			// real MAX timestamp used for per-chat ordering.
			err = pgx.ErrNoRows
			for probe := uint32(0); probe < 4 && errors.Is(err, pgx.ErrNoRows); probe++ {
				err = tx.QueryRow(ctx, `
					INSERT INTO processed_events (
						event_key, chat_id, timestamp, event_timestamp, update_type, payload, status,
						received_at, updated_at, next_attempt_at
					)
					VALUES ($1, $2, $3, $4, $5, $6::jsonb, 'received', NOW(), NOW(), NOW())
					ON CONFLICT DO NOTHING
					RETURNING event_key, chat_id,
						status = 'received'
						OR (status = 'failed' AND COALESCE(next_attempt_at, NOW()) <= NOW())
				`, key, ref.ChatID, compatibilityTimestamp(key, probe), update.Timestamp, update.UpdateType, string(payload)).Scan(
					&ref.Key, &ref.ChatID, &shouldQueue,
				)
				if errors.Is(err, pgx.ErrNoRows) {
					err = tx.QueryRow(ctx, `
						SELECT event_key, chat_id,
							status = 'received'
							OR (status = 'failed' AND COALESCE(next_attempt_at, NOW()) <= NOW())
						FROM processed_events
						WHERE event_key = $1
					`, key).Scan(&ref.Key, &ref.ChatID, &shouldQueue)
				}
			}
		}
	}
	if err != nil {
		return eventRef{}, false, fmt.Errorf("persist webhook event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return eventRef{}, false, fmt.Errorf("commit webhook event persistence: %w", err)
	}

	return ref, shouldQueue, nil
}

func (s *postgresStore) withChatLock(ctx context.Context, chatID int64, fn func(eventSession) error) error {
	conn, err := s.db.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire database connection for chat lock: %w", err)
	}
	releaseNormally := true
	defer func() {
		if releaseNormally {
			conn.Release()
			return
		}
		raw := conn.Hijack()
		closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = raw.Close(closeCtx)
	}()

	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, chatID).Scan(&locked); err != nil {
		return fmt.Errorf("lock chat: %w", err)
	}
	if !locked {
		return errChatBusy
	}

	operationErr := fn(postgresEventSession{conn: conn})

	unlockCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var unlocked bool
	unlockErr := conn.QueryRow(unlockCtx, `SELECT pg_advisory_unlock($1)`, chatID).Scan(&unlocked)
	if unlockErr != nil || !unlocked {
		// A session-level advisory lock must never leak back into the pool.
		releaseNormally = false
		if unlockErr == nil {
			unlockErr = errors.New("database reported that the chat lock was not held")
		}
		if operationErr != nil {
			return errors.Join(operationErr, fmt.Errorf("unlock chat: %w", unlockErr))
		}
		return fmt.Errorf("unlock chat: %w", unlockErr)
	}
	return operationErr
}

func (s *postgresStore) due(ctx context.Context, limit int, staleAfter time.Duration) ([]eventRef, error) {
	rows, err := s.db.Query(ctx, `
		SELECT event_key, chat_id
		FROM (
			SELECT DISTINCT ON (chat_id)
				event_key, chat_id, event_timestamp, id, status, next_attempt_at, processing_started_at,
				updated_at, received_at
			FROM processed_events
			WHERE status IN ('received', 'processing', 'failed')
			ORDER BY chat_id, event_timestamp, id
		) earliest_per_chat
		WHERE
			(status = 'received' AND COALESCE(next_attempt_at, NOW()) <= NOW())
			OR (status = 'failed' AND COALESCE(next_attempt_at, NOW()) <= NOW())
			OR (status = 'processing' AND COALESCE(processing_started_at, updated_at, received_at) < NOW() - $1::interval)
		ORDER BY event_timestamp, id
		LIMIT $2
	`, intervalString(staleAfter), limit)
	if err != nil {
		return nil, fmt.Errorf("list recoverable webhook events: %w", err)
	}
	defer rows.Close()

	refs := make([]eventRef, 0, limit)
	for rows.Next() {
		var ref eventRef
		if err := rows.Scan(&ref.Key, &ref.ChatID); err != nil {
			return nil, fmt.Errorf("scan recoverable webhook event: %w", err)
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recoverable webhook events: %w", err)
	}
	return refs, nil
}

func (s *postgresStore) nextDue(ctx context.Context, chatID int64, staleAfter time.Duration) (eventRef, bool, error) {
	var ref eventRef
	err := s.db.QueryRow(ctx, `
		SELECT event_key, chat_id
		FROM (
			SELECT event_key, chat_id, event_timestamp, id, status, next_attempt_at, processing_started_at,
				updated_at, received_at
			FROM processed_events
			WHERE chat_id = $1 AND status IN ('received', 'processing', 'failed')
			ORDER BY event_timestamp, id
			LIMIT 1
		) earliest
		WHERE (
			(status = 'received' AND COALESCE(next_attempt_at, NOW()) <= NOW())
			OR (status = 'failed' AND COALESCE(next_attempt_at, NOW()) <= NOW())
			OR (status = 'processing' AND COALESCE(processing_started_at, updated_at, received_at) < NOW() - $2::interval)
		)
	`, chatID, intervalString(staleAfter)).Scan(&ref.Key, &ref.ChatID)
	if errors.Is(err, pgx.ErrNoRows) {
		return eventRef{}, false, nil
	}
	if err != nil {
		return eventRef{}, false, fmt.Errorf("find next webhook event for chat: %w", err)
	}
	return ref, true, nil
}

func (s *postgresStore) cleanup(ctx context.Context, completedBefore, failedBefore time.Time) (int64, error) {
	tag, err := s.db.Exec(ctx, `
		DELETE FROM processed_events
		WHERE (status = 'completed' AND COALESCE(completed_at, updated_at) < $1)
		   OR (status IN ('failed', 'dead') AND COALESCE(failed_at, updated_at) < $2)
	`, completedBefore, failedBefore)
	if err != nil {
		return 0, fmt.Errorf("cleanup processed webhook events: %w", err)
	}
	return tag.RowsAffected(), nil
}

type postgresEventSession struct {
	conn *pgxpool.Conn
}

func (s postgresEventSession) claim(ctx context.Context, ref eventRef, staleAfter time.Duration) (storedEvent, bool, error) {
	var payload []byte
	var attempts int
	err := s.conn.QueryRow(ctx, `
		UPDATE processed_events AS pe
		SET status = 'processing',
			attempts = attempts + 1,
			processing_started_at = NOW(),
			updated_at = NOW(),
			last_error = NULL
		WHERE event_key = $1
		  AND (
			(pe.status IN ('received', 'failed') AND COALESCE(pe.next_attempt_at, NOW()) <= NOW())
			OR (pe.status = 'processing' AND COALESCE(pe.processing_started_at, pe.updated_at, pe.received_at) < NOW() - $2::interval)
		  )
		  AND NOT EXISTS (
			SELECT 1
			FROM processed_events older
			WHERE older.chat_id = pe.chat_id
			  AND (older.event_timestamp, older.id) < (pe.event_timestamp, pe.id)
			  AND older.status IN ('received', 'processing', 'failed')
		  )
		RETURNING payload, attempts
	`, ref.Key, intervalString(staleAfter)).Scan(&payload, &attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return storedEvent{}, false, nil
	}
	if err != nil {
		return storedEvent{}, false, fmt.Errorf("claim webhook event: %w", err)
	}

	var update maxapi.Update
	if err := json.Unmarshal(payload, &update); err != nil {
		return storedEvent{Ref: ref, Attempts: attempts}, true, fmt.Errorf("decode stored webhook event: %w", err)
	}
	return storedEvent{Ref: ref, Update: update, Attempts: attempts}, true, nil
}

func (s postgresEventSession) complete(ctx context.Context, key string) error {
	tag, err := s.conn.Exec(ctx, `
		UPDATE processed_events
		SET status = 'completed', completed_at = NOW(), updated_at = NOW(),
			processing_started_at = NULL, next_attempt_at = NULL,
			payload = '{}'::jsonb, last_error = NULL
		WHERE event_key = $1 AND status = 'processing'
	`, key)
	if err != nil {
		return fmt.Errorf("complete webhook event: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("complete webhook event: event is not processing")
	}
	return nil
}

func (s postgresEventSession) fail(ctx context.Context, key, message string, nextAttempt time.Time, terminal bool) error {
	message = truncateRunes(message, 2000)
	tag, err := s.conn.Exec(ctx, `
		UPDATE processed_events
		SET status = CASE WHEN $4 THEN 'dead' ELSE 'failed' END,
			failed_at = COALESCE(failed_at, NOW()), updated_at = NOW(),
			processing_started_at = NULL,
			next_attempt_at = CASE WHEN $4 THEN NULL ELSE $2::timestamptz END,
			last_error = $3
		WHERE event_key = $1 AND status = 'processing'
	`, key, nextAttempt, message, terminal)
	if err != nil {
		return fmt.Errorf("fail webhook event: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("fail webhook event: event is not processing")
	}
	return nil
}

func (s postgresEventSession) interrupt(ctx context.Context, key, message string, nextAttempt time.Time) error {
	message = truncateRunes(message, 2000)
	tag, err := s.conn.Exec(ctx, `
		UPDATE processed_events
		SET status = 'failed',
			attempts = GREATEST(attempts - 1, 0),
			failed_at = COALESCE(failed_at, NOW()), updated_at = NOW(),
			processing_started_at = NULL,
			next_attempt_at = $2,
			last_error = $3
		WHERE event_key = $1 AND status = 'processing'
	`, key, nextAttempt, message)
	if err != nil {
		return fmt.Errorf("interrupt webhook event: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("interrupt webhook event: event is not processing")
	}
	return nil
}

func truncateRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func intervalString(value time.Duration) string {
	return fmt.Sprintf("%f seconds", value.Seconds())
}
