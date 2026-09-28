package worker

import (
	"Max-hack/internal/maxapi"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// TestPostgresStoreAndMigrations is opt-in because it creates and drops an
// isolated schema. Point TEST_DATABASE_URL only at a disposable test database.
func TestPostgresStoreAndMigrations(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "worker_test_" + randomHex(t, 8)
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Errorf("drop test schema: %v", err)
		}
	}()

	migrationDB := openSchemaDB(t, databaseURL, schema)
	defer func() {
		if err := migrationDB.Close(); err != nil {
			t.Errorf("close migration database: %v", err)
		}
	}()
	goose.SetBaseFS(os.DirFS(filepath.Join("..", "..", "cmd", "migrator")))
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	const baseMigrationVersion int64 = 20260821000001
	if err := goose.UpToContext(ctx, migrationDB, "migrations", baseMigrationVersion); err != nil {
		t.Fatalf("apply base migration: %v", err)
	}
	const preUpgradeInsert = `
		INSERT INTO processed_events (chat_id, timestamp, update_type)
		VALUES (41, 123455, 'message_created')
	`
	if _, err := migrationDB.ExecContext(ctx, preUpgradeInsert); err != nil {
		t.Fatalf("insert event before durable migration: %v", err)
	}
	if err := goose.UpContext(ctx, migrationDB, "migrations"); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newPostgresStore(db)
	assertDurableReadiness(t, ctx, db, store)
	assertMigratedLegacyEventDeduplicates(t, ctx, db, store)
	assertPreviousVersionCanInsert(t, ctx, db, store)

	update := maxapi.Update{
		UpdateType: maxapi.UpdateMessageCreated,
		Timestamp:  100,
		ChatID:     99,
		MessageID:  "integration-message",
	}
	key, _ := EventKey(update)
	payload, _ := json.Marshal(update)
	ref, shouldQueue, err := store.save(ctx, key, update, payload)
	if err != nil || !shouldQueue {
		t.Fatalf("save: shouldQueue=%v err=%v", shouldQueue, err)
	}
	assertPreviousVersionDeduplicatesNewEvent(t, ctx, db, update)
	var storedTimestamp, eventTimestamp int64
	if err := db.QueryRow(ctx, `SELECT timestamp, event_timestamp FROM processed_events WHERE event_key = $1`, key).Scan(&storedTimestamp, &eventTimestamp); err != nil {
		t.Fatal(err)
	}
	if storedTimestamp != update.Timestamp || eventTimestamp != update.Timestamp {
		t.Fatalf("timestamps = legacy:%d event:%d, want %d for both", storedTimestamp, eventTimestamp, update.Timestamp)
	}
	if err := store.withChatLock(ctx, ref.ChatID, func(session eventSession) error {
		event, claimed, err := session.claim(ctx, ref, time.Minute)
		if err != nil {
			return err
		}
		if !claimed || event.Attempts != 1 || event.Update.MessageID != update.MessageID {
			t.Fatalf("unexpected claim: claimed=%v event=%+v", claimed, event)
		}
		return session.fail(ctx, ref.Key, "temporary integration failure", time.Now().Add(-time.Second), false)
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.withChatLock(ctx, ref.ChatID, func(session eventSession) error {
		event, claimed, err := session.claim(ctx, ref, time.Minute)
		if err != nil {
			return err
		}
		if !claimed || event.Attempts != 2 || event.Update.MessageID != update.MessageID {
			t.Fatalf("unexpected retry claim: claimed=%v event=%+v", claimed, event)
		}
		return session.complete(ctx, ref.Key)
	}); err != nil {
		t.Fatal(err)
	}
	var completedPayload []byte
	var completedError *string
	if err := db.QueryRow(ctx, `SELECT payload, last_error FROM processed_events WHERE event_key = $1`, key).Scan(&completedPayload, &completedError); err != nil {
		t.Fatal(err)
	}
	if string(completedPayload) != "{}" || completedError != nil {
		t.Fatalf("completed event retained data: payload=%q last_error=%v", completedPayload, completedError)
	}

	_, shouldQueue, err = store.save(ctx, key, update, payload)
	if err != nil {
		t.Fatal(err)
	}
	if shouldQueue {
		t.Fatal("completed duplicate was queued again")
	}
	assertInterruptedAttemptCanRetry(t, ctx, store, db)
	assertTerminalFailureDoesNotBlockChat(t, ctx, store, db)
	assertChatOrder(t, ctx, store)
	assertDistinctStableEventsCanShareLegacyTuple(t, ctx, store, db)

	assertCrossInstanceChatLock(t, ctx, store)

	if _, err := db.Exec(ctx, `UPDATE processed_events SET completed_at = NOW() - INTERVAL '31 days' WHERE event_key = $1`, key); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.cleanup(ctx, time.Now().Add(-30*24*time.Hour), time.Now().Add(-90*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("deleted rows = %d, want 1", deleted)
	}
	assertRollbackRejectsPendingThenPreservesLegacyDedupe(t, ctx, migrationDB, store, baseMigrationVersion)
}

func assertInterruptedAttemptCanRetry(t *testing.T, ctx context.Context, store *postgresStore, db *pgxpool.Pool) {
	t.Helper()
	interrupted := maxapi.Update{UpdateType: maxapi.UpdateMessageCreated, Timestamp: 125, ChatID: 444, MessageID: "interrupted"}
	ref := saveIntegrationEvent(t, ctx, store, interrupted)
	if err := store.withChatLock(ctx, interrupted.ChatID, func(session eventSession) error {
		event, claimed, err := session.claim(ctx, ref, time.Minute)
		if err != nil {
			return err
		}
		if !claimed || event.Attempts != 1 {
			t.Fatalf("unexpected interrupted claim: claimed=%v event=%+v", claimed, event)
		}
		return session.interrupt(ctx, ref.Key, "worker shutdown", time.Now().Add(-time.Second))
	}); err != nil {
		t.Fatal(err)
	}

	var status string
	var attempts int
	if err := db.QueryRow(ctx, `
		SELECT status, attempts FROM processed_events WHERE event_key = $1
	`, ref.Key).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || attempts != 0 {
		t.Fatalf("interrupted event = status:%q attempts:%d, want failed/0", status, attempts)
	}

	if err := store.withChatLock(ctx, interrupted.ChatID, func(session eventSession) error {
		event, claimed, err := session.claim(ctx, ref, time.Minute)
		if err != nil {
			return err
		}
		if !claimed || event.Attempts != 1 {
			t.Fatalf("unexpected retry after interruption: claimed=%v event=%+v", claimed, event)
		}
		return session.complete(ctx, ref.Key)
	}); err != nil {
		t.Fatal(err)
	}
}

func assertTerminalFailureDoesNotBlockChat(t *testing.T, ctx context.Context, store *postgresStore, db *pgxpool.Pool) {
	t.Helper()
	failed := maxapi.Update{UpdateType: maxapi.UpdateMessageCreated, Timestamp: 150, ChatID: 555, MessageID: "terminal-failure"}
	failedRef := saveIntegrationEvent(t, ctx, store, failed)
	if err := store.withChatLock(ctx, failed.ChatID, func(session eventSession) error {
		event, claimed, err := session.claim(ctx, failedRef, time.Minute)
		if err != nil {
			return err
		}
		if !claimed || event.Attempts != 1 {
			t.Fatalf("unexpected terminal claim: claimed=%v event=%+v", claimed, event)
		}
		return session.fail(ctx, failedRef.Key, "terminal integration failure", time.Now(), true)
	}); err != nil {
		t.Fatal(err)
	}

	var status string
	var nextAttempt *time.Time
	if err := db.QueryRow(ctx, `
		SELECT status, next_attempt_at
		FROM processed_events
		WHERE event_key = $1
	`, failedRef.Key).Scan(&status, &nextAttempt); err != nil {
		t.Fatal(err)
	}
	if status != "dead" || nextAttempt != nil {
		t.Fatalf("terminal event status=%q next_attempt_at=%v, want dead/NULL", status, nextAttempt)
	}

	newer := maxapi.Update{UpdateType: maxapi.UpdateMessageCreated, Timestamp: 151, ChatID: failed.ChatID, MessageID: "after-terminal"}
	newerRef := saveIntegrationEvent(t, ctx, store, newer)
	if err := store.withChatLock(ctx, failed.ChatID, func(session eventSession) error {
		if _, claimed, err := session.claim(ctx, newerRef, time.Minute); err != nil {
			return err
		} else if !claimed {
			t.Fatal("dead-letter event blocked the next chat event")
		}
		return session.complete(ctx, newerRef.Key)
	}); err != nil {
		t.Fatal(err)
	}
}

func assertMigratedLegacyEventDeduplicates(t *testing.T, ctx context.Context, db *pgxpool.Pool, store *postgresStore) {
	t.Helper()
	update := maxapi.Update{
		UpdateType: maxapi.UpdateMessageCreated,
		Timestamp:  123455,
		ChatID:     41,
		MessageID:  "pre-upgrade-message",
	}
	key, err := EventKey(update)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(update)
	if err != nil {
		t.Fatal(err)
	}
	ref, shouldQueue, err := store.save(ctx, key, update, payload)
	if err != nil {
		t.Fatal(err)
	}
	if shouldQueue || ref.Key == key {
		t.Fatalf("migrated legacy retry = ref:%+v queue:%v, want existing completed tuple", ref, shouldQueue)
	}
	var count int
	if err := db.QueryRow(ctx, `
		SELECT COUNT(*) FROM processed_events
		WHERE chat_id = $1 AND timestamp = $2 AND update_type = $3
	`, update.ChatID, update.Timestamp, update.UpdateType).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("migrated legacy tuple rows = %d, want 1", count)
	}
}

func assertPreviousVersionCanInsert(t *testing.T, ctx context.Context, db *pgxpool.Pool, store *postgresStore) {
	t.Helper()
	const legacyInsert = `
		INSERT INTO processed_events (chat_id, timestamp, update_type)
		VALUES (42, 123456, 'message_created')
		ON CONFLICT (chat_id, timestamp, update_type) DO NOTHING
	`
	first, err := db.Exec(ctx, legacyInsert)
	if err != nil {
		t.Fatalf("previous application insert failed: %v", err)
	}
	second, err := db.Exec(ctx, legacyInsert)
	if err != nil {
		t.Fatalf("previous application duplicate insert failed: %v", err)
	}
	if first.RowsAffected() != 1 || second.RowsAffected() != 0 {
		t.Fatalf("legacy insert rows = %d then %d, want 1 then 0", first.RowsAffected(), second.RowsAffected())
	}
	var key, status string
	var payload []byte
	if err := db.QueryRow(ctx, `
		SELECT event_key, status, payload
		FROM processed_events
		WHERE chat_id = 42 AND timestamp = 123456 AND update_type = 'message_created'
	`).Scan(&key, &status, &payload); err != nil {
		t.Fatal(err)
	}
	if key == "" || status != "completed" || string(payload) != "{}" {
		t.Fatalf("legacy defaults = key:%q status:%q payload:%q", key, status, payload)
	}

	update := maxapi.Update{
		UpdateType: maxapi.UpdateMessageCreated,
		Timestamp:  123456,
		ChatID:     42,
		MessageID:  "rolling-deploy-message",
	}
	stableKey, err := EventKey(update)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(update)
	if err != nil {
		t.Fatal(err)
	}
	ref, shouldQueue, err := store.save(ctx, stableKey, update, encoded)
	if err != nil {
		t.Fatal(err)
	}
	if shouldQueue || ref.Key != key {
		t.Fatalf("legacy rolling-deploy retry = ref:%+v queue:%v, want completed key %q", ref, shouldQueue, key)
	}
}

func assertPreviousVersionDeduplicatesNewEvent(t *testing.T, ctx context.Context, db *pgxpool.Pool, update maxapi.Update) {
	t.Helper()
	tag, err := db.Exec(ctx, `
		INSERT INTO processed_events (chat_id, timestamp, update_type)
		VALUES ($1, $2, $3)
		ON CONFLICT (chat_id, timestamp, update_type) DO NOTHING
	`, update.ResolvedChatID(), update.Timestamp, update.UpdateType)
	if err != nil {
		t.Fatalf("previous application retry failed: %v", err)
	}
	if tag.RowsAffected() != 0 {
		t.Fatalf("previous application inserted a duplicate of a new event: rows=%d", tag.RowsAffected())
	}
}

func assertDistinctStableEventsCanShareLegacyTuple(
	t *testing.T,
	ctx context.Context,
	store *postgresStore,
	db *pgxpool.Pool,
) {
	t.Helper()
	first := maxapi.Update{
		UpdateType: maxapi.UpdateMessageCreated,
		Timestamp:  555000,
		ChatID:     555000,
		MessageID:  "same-timestamp-first",
	}
	second := first
	second.MessageID = "same-timestamp-second"
	firstRef := saveIntegrationEvent(t, ctx, store, first)
	secondRef := saveIntegrationEvent(t, ctx, store, second)
	if firstRef.Key == secondRef.Key {
		t.Fatalf("distinct stable events resolved to one record: %+v", firstRef)
	}

	rows, err := db.Query(ctx, `
		SELECT event_key, timestamp
		FROM processed_events
		WHERE chat_id = $1 AND event_timestamp = $2 AND update_type = $3
		ORDER BY id
	`, first.ChatID, first.Timestamp, first.UpdateType)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	stored := make(map[string]int64, 2)
	for rows.Next() {
		var key string
		var compatibilityValue int64
		if err := rows.Scan(&key, &compatibilityValue); err != nil {
			t.Fatal(err)
		}
		stored[key] = compatibilityValue
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 || stored[firstRef.Key] != first.Timestamp || stored[secondRef.Key] == first.Timestamp {
		t.Fatalf("shared-timestamp rows = %+v, want original and alternate compatibility tuples", stored)
	}

	if err := store.withChatLock(ctx, first.ChatID, func(session eventSession) error {
		for _, ref := range []eventRef{firstRef, secondRef} {
			if _, claimed, err := session.claim(ctx, ref, time.Minute); err != nil {
				return err
			} else if !claimed {
				t.Fatalf("distinct shared-timestamp event was not claimable: %+v", ref)
			}
			if err := session.complete(ctx, ref.Key); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	secondKey, err := EventKey(second)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	duplicateRef, shouldQueue, err := store.save(ctx, secondKey, second, payload)
	if err != nil {
		t.Fatal(err)
	}
	if duplicateRef != secondRef || shouldQueue {
		t.Fatalf("stable retry = ref:%+v queue:%v, want %+v/false", duplicateRef, shouldQueue, secondRef)
	}
}

func assertRollbackRejectsPendingThenPreservesLegacyDedupe(
	t *testing.T,
	ctx context.Context,
	migrationDB *sql.DB,
	store *postgresStore,
	baseMigrationVersion int64,
) {
	t.Helper()
	update := maxapi.Update{
		UpdateType: maxapi.UpdateMessageCreated,
		Timestamp:  987654,
		ChatID:     987,
		MessageID:  "before-rollback",
	}
	ref := saveIntegrationEvent(t, ctx, store, update)
	if err := goose.DownToContext(ctx, migrationDB, "migrations", baseMigrationVersion); err == nil {
		t.Fatal("durable migration rollback accepted an unprocessed webhook event")
	}
	if err := store.ready(ctx); err != nil {
		t.Fatalf("failed rollback did not preserve the durable schema: %v", err)
	}
	var sessionsPreserved bool
	if err := store.db.QueryRow(ctx, `SELECT to_regclass('bot_sessions') IS NOT NULL`).Scan(&sessionsPreserved); err != nil {
		t.Fatal(err)
	}
	if !sessionsPreserved {
		t.Fatal("failed rollback removed bot sessions before the webhook guard")
	}
	if err := store.withChatLock(ctx, ref.ChatID, func(session eventSession) error {
		if _, claimed, err := session.claim(ctx, ref, time.Minute); err != nil {
			return err
		} else if !claimed {
			t.Fatal("pending event was not claimable after rejected rollback")
		}
		return session.complete(ctx, ref.Key)
	}); err != nil {
		t.Fatal(err)
	}
	if err := goose.DownToContext(ctx, migrationDB, "migrations", baseMigrationVersion); err != nil {
		t.Fatalf("roll back durable migrations: %v", err)
	}
	var sessionsRemoved bool
	if err := migrationDB.QueryRowContext(ctx, `SELECT to_regclass('bot_sessions') IS NULL`).Scan(&sessionsRemoved); err != nil {
		t.Fatal(err)
	}
	if !sessionsRemoved {
		t.Fatal("successful full rollback retained bot_sessions unexpectedly")
	}
	result, err := migrationDB.ExecContext(ctx, `
		INSERT INTO processed_events (chat_id, timestamp, update_type)
		VALUES ($1, $2, $3)
		ON CONFLICT (chat_id, timestamp, update_type) DO NOTHING
	`, update.ChatID, update.Timestamp, update.UpdateType)
	if err != nil {
		t.Fatalf("previous application insert after rollback: %v", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("rollback lost legacy dedupe tuple: inserted rows=%d", rows)
	}
}

func assertChatOrder(t *testing.T, ctx context.Context, store *postgresStore) {
	t.Helper()
	older := maxapi.Update{UpdateType: maxapi.UpdateMessageCreated, Timestamp: 200, ChatID: 777, MessageID: "older"}
	newer := maxapi.Update{UpdateType: maxapi.UpdateMessageCreated, Timestamp: 201, ChatID: 777, MessageID: "newer"}
	// Persist out of order to prove that the MAX source timestamp, rather than
	// database insertion order, defines the per-chat processing sequence.
	newerRef := saveIntegrationEvent(t, ctx, store, newer)
	olderRef := saveIntegrationEvent(t, ctx, store, older)

	if err := store.withChatLock(ctx, older.ChatID, func(session eventSession) error {
		if _, claimed, err := session.claim(ctx, newerRef, time.Minute); err != nil {
			return err
		} else if claimed {
			t.Fatal("newer event was claimed before the older pending event")
		}
		if _, claimed, err := session.claim(ctx, olderRef, time.Minute); err != nil {
			return err
		} else if !claimed {
			t.Fatal("older event was not claimed")
		}
		return session.fail(ctx, olderRef.Key, "temporary failure", time.Now().Add(time.Minute), false)
	}); err != nil {
		t.Fatal(err)
	}

	if ref, found, err := store.nextDue(ctx, older.ChatID, time.Minute); err != nil {
		t.Fatal(err)
	} else if found {
		t.Fatalf("next due event = %+v, want none while oldest event is in backoff", ref)
	}
	refs, err := store.due(ctx, 100, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		if ref.ChatID == older.ChatID {
			t.Fatalf("due returned %+v while oldest chat event is in backoff", ref)
		}
	}
	if _, err := store.db.Exec(ctx, `
		UPDATE processed_events SET next_attempt_at = NOW() - INTERVAL '1 second'
		WHERE event_key = $1
	`, olderRef.Key); err != nil {
		t.Fatal(err)
	}

	if err := store.withChatLock(ctx, older.ChatID, func(session eventSession) error {
		if _, claimed, err := session.claim(ctx, olderRef, time.Minute); err != nil {
			return err
		} else if !claimed {
			t.Fatal("older event was not claimable after backoff")
		}
		if err := session.complete(ctx, olderRef.Key); err != nil {
			return err
		}
		if _, claimed, err := session.claim(ctx, newerRef, time.Minute); err != nil {
			return err
		} else if !claimed {
			t.Fatal("newer event was not claimable after older event completed")
		}
		return session.complete(ctx, newerRef.Key)
	}); err != nil {
		t.Fatal(err)
	}
}

func assertDurableReadiness(t *testing.T, ctx context.Context, db *pgxpool.Pool, store *postgresStore) {
	t.Helper()
	if err := store.ready(ctx); err != nil {
		t.Fatalf("durable store is not ready after migrations: %v", err)
	}

	if _, err := db.Exec(ctx, `ALTER TABLE processed_events DROP CONSTRAINT processed_events_pkey`); err != nil {
		t.Fatal(err)
	}
	primaryKeyMissing := true
	defer func() {
		if primaryKeyMissing {
			_, _ = db.Exec(context.Background(), `
				ALTER TABLE processed_events
				ADD PRIMARY KEY (chat_id, timestamp, update_type)
			`)
		}
	}()
	if err := store.ready(ctx); err == nil {
		t.Fatal("readiness accepted a store without the legacy compatibility primary key")
	}
	if _, err := db.Exec(ctx, `
		ALTER TABLE processed_events
		ADD PRIMARY KEY (chat_id, timestamp, update_type)
	`); err != nil {
		t.Fatal(err)
	}
	primaryKeyMissing = false

	if _, err := db.Exec(ctx, `ALTER TABLE processed_events DROP CONSTRAINT processed_events_attempts_check`); err != nil {
		t.Fatal(err)
	}
	constraintMissing := true
	defer func() {
		if constraintMissing {
			_, _ = db.Exec(context.Background(), `
				ALTER TABLE processed_events ADD CONSTRAINT processed_events_attempts_check CHECK (attempts >= 0)
			`)
		}
	}()
	if err := store.ready(ctx); err == nil {
		t.Fatal("readiness accepted a store without a required durable constraint")
	}
	if _, err := db.Exec(ctx, `
		ALTER TABLE processed_events ADD CONSTRAINT processed_events_attempts_check CHECK (attempts >= 0)
	`); err != nil {
		t.Fatal(err)
	}
	constraintMissing = false

	if _, err := db.Exec(ctx, `ALTER TABLE processed_events DROP CONSTRAINT processed_events_status_check`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		ALTER TABLE processed_events ADD CONSTRAINT processed_events_status_check
			CHECK (status IN ('received', 'processing', 'completed', 'failed'))
	`); err != nil {
		t.Fatal(err)
	}
	statusConstraintOutdated := true
	defer func() {
		if statusConstraintOutdated {
			_, _ = db.Exec(context.Background(), `ALTER TABLE processed_events DROP CONSTRAINT IF EXISTS processed_events_status_check`)
			_, _ = db.Exec(context.Background(), `
				ALTER TABLE processed_events ADD CONSTRAINT processed_events_status_check
					CHECK (status IN ('received', 'processing', 'completed', 'failed', 'dead'))
			`)
		}
	}()
	if err := store.ready(ctx); err == nil {
		t.Fatal("readiness accepted the legacy status constraint without dead-letter support")
	}
	if _, err := db.Exec(ctx, `ALTER TABLE processed_events DROP CONSTRAINT processed_events_status_check`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		ALTER TABLE processed_events ADD CONSTRAINT processed_events_status_check
			CHECK (status IN ('received', 'processing', 'completed', 'failed', 'dead'))
	`); err != nil {
		t.Fatal(err)
	}
	statusConstraintOutdated = false

	if _, err := db.Exec(ctx, `ALTER TABLE processed_events RENAME COLUMN last_error TO missing_last_error`); err != nil {
		t.Fatal(err)
	}
	columnMissing := true
	defer func() {
		if columnMissing {
			_, _ = db.Exec(context.Background(), `ALTER TABLE processed_events RENAME COLUMN missing_last_error TO last_error`)
		}
	}()
	if err := store.ready(ctx); err == nil {
		t.Fatal("readiness accepted a store without a required durable column")
	}
	if _, err := db.Exec(ctx, `ALTER TABLE processed_events RENAME COLUMN missing_last_error TO last_error`); err != nil {
		t.Fatal(err)
	}
	columnMissing = false

	if err := store.ready(ctx); err != nil {
		t.Fatalf("durable store did not become ready after schema restoration: %v", err)
	}
}

func saveIntegrationEvent(t *testing.T, ctx context.Context, store *postgresStore, update maxapi.Update) eventRef {
	t.Helper()
	key, err := EventKey(update)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(update)
	if err != nil {
		t.Fatal(err)
	}
	ref, _, err := store.save(ctx, key, update, payload)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func openSchemaDB(t *testing.T, databaseURL, schema string) *sql.DB {
	t.Helper()
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.RuntimeParams["search_path"] = schema
	return stdlib.OpenDB(*config)
}

func assertCrossInstanceChatLock(t *testing.T, ctx context.Context, store *postgresStore) {
	t.Helper()
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- store.withChatLock(ctx, 1234, func(eventSession) error {
			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first chat lock was not acquired")
	}

	err := store.withChatLock(ctx, 1234, func(eventSession) error {
		t.Fatal("second worker entered a locked chat")
		return nil
	})
	if !errors.Is(err, errChatBusy) {
		t.Fatalf("second lock error = %v, want errChatBusy", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func randomHex(t *testing.T, size int) string {
	t.Helper()
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(data)
}
