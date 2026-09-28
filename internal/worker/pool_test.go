package worker

import (
	"Max-hack/internal/maxapi"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestEventKeyUsesStablePlatformIdentifiers(t *testing.T) {
	tests := []struct {
		name   string
		update maxapi.Update
		want   string
	}{
		{
			name: "callback id takes precedence",
			update: maxapi.Update{UpdateType: maxapi.UpdateMessageCallback, MessageID: "message-1", Callback: &maxapi.Callback{
				CallbackID: "callback-1",
			}},
			want: "callback:callback-1",
		},
		{
			name:   "top-level message id",
			update: maxapi.Update{UpdateType: maxapi.UpdateMessageCreated, MessageID: "message-1"},
			want:   "message:message_created:message-1",
		},
		{
			name: "nested message id",
			update: maxapi.Update{UpdateType: maxapi.UpdateMessageCreated, Message: &maxapi.Message{
				Body: maxapi.MessageBody{Mid: "message-2"},
			}},
			want: "message:message_created:message-2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := EventKey(tt.update)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("key = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEventKeyFallbackIsDeterministic(t *testing.T) {
	update := maxapi.Update{UpdateType: maxapi.UpdateBotStarted, Timestamp: 123, ChatID: 7}
	first, err := EventKey(update)
	if err != nil {
		t.Fatal(err)
	}
	second, err := EventKey(update)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || len(first) != len("update:")+64 {
		t.Fatalf("fallback keys are not stable SHA-256 values: %q and %q", first, second)
	}
}

func TestCompatibilityTimestampIsDeterministicAndProbeSpecific(t *testing.T) {
	first := compatibilityTimestamp("message:message_created:one", 0)
	if repeat := compatibilityTimestamp("message:message_created:one", 0); repeat != first {
		t.Fatalf("compatibility timestamp changed: %d then %d", first, repeat)
	}
	if second := compatibilityTimestamp("message:message_created:one", 1); second == first {
		t.Fatalf("different probes produced the same compatibility timestamp: %d", first)
	}
	if other := compatibilityTimestamp("message:message_created:two", 0); other == first {
		t.Fatalf("different event keys produced the same compatibility timestamp: %d", first)
	}
}

func TestDefaultFailedEventRetentionIsSevenDays(t *testing.T) {
	if got, want := DefaultConfig().FailedTTL, 7*24*time.Hour; got != want {
		t.Fatalf("failed event TTL = %s, want %s", got, want)
	}
	if got, want := DefaultConfig().MaxAttempts, 8; got != want {
		t.Fatalf("maximum event attempts = %d, want %d", got, want)
	}
}

func TestAcceptKeepsEventsWhenMemoryQueueIsFull(t *testing.T) {
	store := newMemoryStore()
	p := newPoolWithStore(1, 1, store, handlerFunc(func(context.Context, maxapi.Update) {}), DefaultConfig())

	for i := int64(1); i <= 2; i++ {
		update := testUpdate(i, 10)
		if err := p.Accept(context.Background(), update); err != nil {
			t.Fatalf("Accept(%d): %v", i, err)
		}
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.events) != 2 {
		t.Fatalf("persisted events = %d, want 2", len(store.events))
	}
	if got := len(p.jobs); got != 1 {
		t.Fatalf("queued events = %d, want capacity-limited 1", got)
	}
}

func TestPoolReadinessRequiresStart(t *testing.T) {
	store := newMemoryStore()
	p := newPoolWithStore(1, 1, store, handlerFunc(func(context.Context, maxapi.Update) {}), testConfig())
	if err := p.Ready(context.Background()); err == nil {
		t.Fatal("pool reported ready before Start")
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)
	if err := p.Ready(context.Background()); err != nil {
		t.Fatalf("started pool is not ready: %v", err)
	}
	cancel()
	p.Wait()
}

func TestDuplicateWebhookIsHandledOnce(t *testing.T) {
	store := newMemoryStore()
	handled := make(chan struct{}, 2)
	p := newPoolWithStore(2, 4, store, handlerFunc(func(context.Context, maxapi.Update) {
		handled <- struct{}{}
	}), testConfig())
	update := testUpdate(1, 10)
	if err := p.Accept(context.Background(), update); err != nil {
		t.Fatal(err)
	}
	if err := p.Accept(context.Background(), update); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)
	waitSignal(t, handled)
	time.Sleep(50 * time.Millisecond)
	cancel()
	p.Wait()

	if got := len(handled); got != 0 {
		t.Fatalf("duplicate invocation count after first = %d, want 0", got)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, event := range store.events {
		if event.status != "completed" || event.attempts != 1 {
			t.Fatalf("event state = %s, attempts = %d; want completed, 1", event.status, event.attempts)
		}
		if !event.payloadCleared || event.update != (maxapi.Update{}) || event.lastError != "" {
			t.Fatalf("completed event retained payload or error: cleared=%v update=%+v error=%q", event.payloadCleared, event.update, event.lastError)
		}
	}
}

func TestEventsForOneChatAreSerialized(t *testing.T) {
	store := newMemoryStore()
	var concurrent atomic.Int32
	var maximum atomic.Int32
	handled := make(chan struct{}, 4)
	handler := handlerFunc(func(context.Context, maxapi.Update) {
		current := concurrent.Add(1)
		for {
			previous := maximum.Load()
			if current <= previous || maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		concurrent.Add(-1)
		handled <- struct{}{}
	})
	p := newPoolWithStore(4, 8, store, handler, testConfig())
	for i := int64(1); i <= 4; i++ {
		if err := p.Accept(context.Background(), testUpdate(i, 77)); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)
	for range 4 {
		waitSignal(t, handled)
	}
	cancel()
	p.Wait()

	if got := maximum.Load(); got != 1 {
		t.Fatalf("maximum concurrent handlers for one chat = %d, want 1", got)
	}
}

func TestShutdownCancelsInFlightEventAndDoesNotClaimBufferedEvent(t *testing.T) {
	store := newMemoryStore()
	started := make(chan struct{})
	stopped := make(chan struct{})
	handler := errorHandlerFunc(func(ctx context.Context, _ maxapi.Update) error {
		close(started)
		<-ctx.Done()
		close(stopped)
		return ctx.Err()
	})
	config := testConfig()
	config.MaxAttempts = 1
	p := newPoolWithStore(1, 2, store, handler, config)

	first := testUpdate(1, 77)
	second := testUpdate(2, 88)
	if err := p.Accept(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if err := p.Accept(context.Background(), second); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)
	waitSignal(t, started)
	cancel()
	waitSignal(t, stopped)
	p.Wait()

	firstKey, _ := EventKey(first)
	secondKey, _ := EventKey(second)
	store.mu.Lock()
	defer store.mu.Unlock()
	if event := store.events[firstKey]; event.status != "failed" || event.attempts != 0 {
		t.Fatalf("in-flight event after shutdown = status:%s attempts:%d, want failed/0", event.status, event.attempts)
	}
	if event := store.events[secondKey]; event.status != "received" || event.attempts != 0 {
		t.Fatalf("buffered event after shutdown = status:%s attempts:%d, want received/0", event.status, event.attempts)
	}
}

func TestFailedEventCanBeRetried(t *testing.T) {
	store := newMemoryStore()
	var panicFirst atomic.Bool
	panicFirst.Store(true)
	handler := handlerFunc(func(context.Context, maxapi.Update) {
		if panicFirst.CompareAndSwap(true, false) {
			panic("temporary failure")
		}
	})
	p := newPoolWithStore(1, 2, store, handler, testConfig())
	update := testUpdate(1, 10)
	key, _ := EventKey(update)
	payload, _ := json.Marshal(update)
	ref, _, err := store.save(context.Background(), key, update, payload)
	if err != nil {
		t.Fatal(err)
	}

	p.process(context.Background(), ref)
	store.mu.Lock()
	if store.events[key].status != "failed" || store.events[key].attempts != 1 {
		state := *store.events[key]
		store.mu.Unlock()
		t.Fatalf("after failure: status=%s attempts=%d, want failed/1", state.status, state.attempts)
	}
	store.events[key].nextAttempt = time.Now().Add(-time.Second)
	store.mu.Unlock()

	p.process(context.Background(), ref)
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.events[key].status != "completed" || store.events[key].attempts != 2 {
		t.Fatalf("after retry: status=%s attempts=%d, want completed/2", store.events[key].status, store.events[key].attempts)
	}
	if !store.events[key].payloadCleared || store.events[key].lastError != "" {
		t.Fatalf("completed retry retained payload or error: cleared=%v error=%q", store.events[key].payloadCleared, store.events[key].lastError)
	}
}

func TestHandlerErrorKeepsEventForRetry(t *testing.T) {
	store := newMemoryStore()
	handlerErr := errors.New("MAX unavailable")
	p := newPoolWithStore(1, 2, store, errorHandlerFunc(func(context.Context, maxapi.Update) error {
		return handlerErr
	}), testConfig())
	update := testUpdate(1, 10)
	key, _ := EventKey(update)
	payload, _ := json.Marshal(update)
	ref, _, err := store.save(context.Background(), key, update, payload)
	if err != nil {
		t.Fatal(err)
	}

	p.process(context.Background(), ref)
	store.mu.Lock()
	defer store.mu.Unlock()
	event := store.events[key]
	if event.status != "failed" || event.payloadCleared || !strings.Contains(event.lastError, handlerErr.Error()) {
		t.Fatalf("event after handler error = %+v", event)
	}
}

type nonRetryableTestError struct{ error }

func (nonRetryableTestError) NonRetryable() bool { return true }

func TestNonRetryableHandlerErrorMovesEventDirectlyToDeadLetter(t *testing.T) {
	store := newMemoryStore()
	deliveryErr := errors.New("ambiguous outbound POST failure")
	p := newPoolWithStore(1, 2, store, errorHandlerFunc(func(context.Context, maxapi.Update) error {
		return nonRetryableTestError{error: deliveryErr}
	}), testConfig())
	update := testUpdate(1, 10)
	key, _ := EventKey(update)
	payload, _ := json.Marshal(update)
	ref, _, err := store.save(context.Background(), key, update, payload)
	if err != nil {
		t.Fatal(err)
	}

	p.process(context.Background(), ref)
	store.mu.Lock()
	defer store.mu.Unlock()
	event := store.events[key]
	if event.status != "dead" || event.attempts != 1 || !strings.Contains(event.lastError, deliveryErr.Error()) {
		t.Fatalf("event after non-retryable error = %+v", event)
	}
}

func TestRetryableCauseTakesPriorityInJoinedHandlerError(t *testing.T) {
	store := newMemoryStore()
	databaseErr := errors.New("session delete failed")
	deliveryErr := nonRetryableTestError{error: errors.New("failure notice was not delivered")}
	p := newPoolWithStore(1, 2, store, errorHandlerFunc(func(context.Context, maxapi.Update) error {
		return errors.Join(databaseErr, deliveryErr)
	}), testConfig())
	update := testUpdate(1, 10)
	key, _ := EventKey(update)
	payload, _ := json.Marshal(update)
	ref, _, err := store.save(context.Background(), key, update, payload)
	if err != nil {
		t.Fatal(err)
	}

	p.process(context.Background(), ref)
	store.mu.Lock()
	defer store.mu.Unlock()
	if got := store.events[key].status; got != "failed" {
		t.Fatalf("joined retryable/non-retryable error status = %q, want failed", got)
	}
}

func TestOlderBackoffEventBlocksNewerEventInSameChat(t *testing.T) {
	store := newMemoryStore()
	var handled atomic.Int32
	p := newPoolWithStore(1, 2, store, handlerFunc(func(context.Context, maxapi.Update) {
		handled.Add(1)
	}), testConfig())

	first := testUpdate(1, 77)
	firstKey, _ := EventKey(first)
	firstPayload, _ := json.Marshal(first)
	if _, _, err := store.save(context.Background(), firstKey, first, firstPayload); err != nil {
		t.Fatal(err)
	}
	second := testUpdate(2, 77)
	secondKey, _ := EventKey(second)
	secondPayload, _ := json.Marshal(second)
	secondRef, _, err := store.save(context.Background(), secondKey, second, secondPayload)
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.events[firstKey].status = "failed"
	store.events[firstKey].nextAttempt = time.Now().Add(time.Minute)
	store.mu.Unlock()
	if refs, err := store.due(context.Background(), 10, time.Minute); err != nil {
		t.Fatal(err)
	} else if len(refs) != 0 {
		t.Fatalf("due events = %+v, want none while oldest event is in backoff", refs)
	}
	if ref, found, err := store.nextDue(context.Background(), first.ChatID, time.Minute); err != nil {
		t.Fatal(err)
	} else if found {
		t.Fatalf("next due event = %+v, want none while oldest event is in backoff", ref)
	}

	if processed := p.process(context.Background(), secondRef); processed {
		t.Fatal("newer event was claimed while an older event was in backoff")
	}
	if got := handled.Load(); got != 0 {
		t.Fatalf("handled events = %d, want 0", got)
	}
}

func TestEventsForOneChatUseSourceTimestampThenInsertionID(t *testing.T) {
	store := newMemoryStore()
	chatID := int64(77)
	newer := testUpdate(200, chatID)
	older := testUpdate(100, chatID)
	tieFirst := testUpdate(300, chatID)
	tieSecond := testUpdate(300, chatID)
	tieFirst.MessageID = "tie-first"
	tieSecond.MessageID = "tie-second"

	newerRef := saveMemoryEvent(t, store, newer)
	olderRef := saveMemoryEvent(t, store, older)
	tieFirstRef := saveMemoryEvent(t, store, tieFirst)
	tieSecondRef := saveMemoryEvent(t, store, tieSecond)

	if ref, found, err := store.nextDue(context.Background(), chatID, time.Minute); err != nil {
		t.Fatal(err)
	} else if !found || ref != olderRef {
		t.Fatalf("next due event = %+v, found=%v; want source-oldest %+v", ref, found, olderRef)
	}
	if refs, err := store.due(context.Background(), 10, time.Minute); err != nil {
		t.Fatal(err)
	} else if len(refs) != 1 || refs[0] != olderRef {
		t.Fatalf("due events = %+v, want only source-oldest %+v", refs, olderRef)
	}

	assertMemoryClaimBlocked(t, store, newerRef)
	assertMemoryClaimBlocked(t, store, tieSecondRef)
	completeMemoryEvent(t, store, olderRef)
	completeMemoryEvent(t, store, newerRef)

	// Equal source timestamps retain deterministic insertion order via the
	// monotonic database id (represented by created in the memory store).
	assertMemoryClaimBlocked(t, store, tieSecondRef)
	completeMemoryEvent(t, store, tieFirstRef)
	completeMemoryEvent(t, store, tieSecondRef)
}

func saveMemoryEvent(t *testing.T, store *memoryStore, update maxapi.Update) eventRef {
	t.Helper()
	key, err := EventKey(update)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(update)
	if err != nil {
		t.Fatal(err)
	}
	ref, _, err := store.save(context.Background(), key, update, payload)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func assertMemoryClaimBlocked(t *testing.T, store *memoryStore, ref eventRef) {
	t.Helper()
	if err := store.withChatLock(context.Background(), ref.ChatID, func(session eventSession) error {
		_, claimed, err := session.claim(context.Background(), ref, time.Minute)
		if err != nil {
			return err
		}
		if claimed {
			t.Fatalf("event %+v was claimed before its source-order predecessor", ref)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func completeMemoryEvent(t *testing.T, store *memoryStore, ref eventRef) {
	t.Helper()
	if err := store.withChatLock(context.Background(), ref.ChatID, func(session eventSession) error {
		if _, claimed, err := session.claim(context.Background(), ref, time.Minute); err != nil {
			return err
		} else if !claimed {
			t.Fatalf("event %+v was not claimable in source order", ref)
		}
		return session.complete(context.Background(), ref.Key)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDuplicateDoesNotBypassBackoffOrRequeueTerminalEvent(t *testing.T) {
	store := newMemoryStore()
	update := testUpdate(1, 10)
	key, _ := EventKey(update)
	payload, _ := json.Marshal(update)
	if _, _, err := store.save(context.Background(), key, update, payload); err != nil {
		t.Fatal(err)
	}

	future := time.Now().Add(time.Minute)
	store.mu.Lock()
	store.events[key].status = "failed"
	store.events[key].nextAttempt = future
	store.mu.Unlock()
	if _, shouldQueue, err := store.save(context.Background(), key, update, payload); err != nil {
		t.Fatal(err)
	} else if shouldQueue {
		t.Fatal("failed duplicate bypassed its retry backoff")
	}
	store.mu.Lock()
	if got := store.events[key].nextAttempt; !got.Equal(future) {
		store.mu.Unlock()
		t.Fatalf("duplicate changed retry time from %s to %s", future, got)
	}
	store.events[key].status = "dead"
	store.mu.Unlock()
	if _, shouldQueue, err := store.save(context.Background(), key, update, payload); err != nil {
		t.Fatal(err)
	} else if shouldQueue {
		t.Fatal("dead-letter duplicate was queued again")
	}
}

func TestEventMovesToDeadLetterAfterMaximumAttempts(t *testing.T) {
	store := newMemoryStore()
	config := testConfig()
	config.MaxAttempts = 2
	p := newPoolWithStore(1, 2, store, errorHandlerFunc(func(context.Context, maxapi.Update) error {
		return errors.New("permanent failure")
	}), config)
	update := testUpdate(1, 10)
	key, _ := EventKey(update)
	payload, _ := json.Marshal(update)
	ref, _, err := store.save(context.Background(), key, update, payload)
	if err != nil {
		t.Fatal(err)
	}

	p.process(context.Background(), ref)
	store.mu.Lock()
	store.events[key].nextAttempt = time.Now().Add(-time.Second)
	store.mu.Unlock()
	p.process(context.Background(), ref)

	store.mu.Lock()
	defer store.mu.Unlock()
	if got := store.events[key].status; got != "dead" {
		t.Fatalf("event status = %q, want dead", got)
	}
}

func TestMalformedStoredEventMovesToDeadLetter(t *testing.T) {
	store := newMemoryStore()
	config := testConfig()
	config.MaxAttempts = 2
	var handled atomic.Int32
	p := newPoolWithStore(1, 2, store, handlerFunc(func(context.Context, maxapi.Update) {
		handled.Add(1)
	}), config)
	update := testUpdate(1, 10)
	key, _ := EventKey(update)
	payload, _ := json.Marshal(update)
	ref, _, err := store.save(context.Background(), key, update, payload)
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.events[key].claimErr = errors.New("malformed persisted payload")
	store.mu.Unlock()

	p.process(context.Background(), ref)
	store.mu.Lock()
	store.events[key].nextAttempt = time.Now().Add(-time.Second)
	store.mu.Unlock()
	p.process(context.Background(), ref)

	store.mu.Lock()
	defer store.mu.Unlock()
	if got := store.events[key].status; got != "dead" {
		t.Fatalf("event status = %q, want dead", got)
	}
	if got := handled.Load(); got != 0 {
		t.Fatalf("handler calls = %d, want 0 for malformed payload", got)
	}
}

func TestStartupRecoversPersistedEvent(t *testing.T) {
	store := newMemoryStore()
	handled := make(chan struct{}, 1)
	p := newPoolWithStore(1, 2, store, handlerFunc(func(context.Context, maxapi.Update) {
		handled <- struct{}{}
	}), testConfig())
	update := testUpdate(1, 88)
	key, _ := EventKey(update)
	payload, _ := json.Marshal(update)
	if _, _, err := store.save(context.Background(), key, update, payload); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)
	waitSignal(t, handled)
	cancel()
	p.Wait()
}

func TestAcceptReturnsPersistenceError(t *testing.T) {
	store := newMemoryStore()
	store.saveErr = errors.New("database unavailable")
	p := newPoolWithStore(1, 1, store, handlerFunc(func(context.Context, maxapi.Update) {}), DefaultConfig())
	if err := p.Accept(context.Background(), testUpdate(1, 1)); !errors.Is(err, store.saveErr) {
		t.Fatalf("Accept error = %v, want %v", err, store.saveErr)
	}
}

func waitSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for handler")
	}
}

func testUpdate(sequence, chatID int64) maxapi.Update {
	return maxapi.Update{
		UpdateType: maxapi.UpdateMessageCreated,
		Timestamp:  sequence,
		MessageID:  "message-" + time.Unix(sequence, 0).UTC().Format("150405"),
		ChatID:     chatID,
	}
}

func testConfig() Config {
	config := DefaultConfig()
	config.HandlerTimeout = time.Second
	config.ProcessingTimeout = 3 * time.Second
	config.RecoveryInterval = 5 * time.Millisecond
	config.CleanupInterval = time.Hour
	return config
}

type handlerFunc func(context.Context, maxapi.Update)

func (f handlerFunc) Handle(ctx context.Context, update maxapi.Update) error {
	f(ctx, update)
	return nil
}

type errorHandlerFunc func(context.Context, maxapi.Update) error

func (f errorHandlerFunc) Handle(ctx context.Context, update maxapi.Update) error {
	return f(ctx, update)
}

type memoryRecord struct {
	ref            eventRef
	update         maxapi.Update
	status         string
	attempts       int
	nextAttempt    time.Time
	eventTimestamp int64
	created        int64
	payloadCleared bool
	lastError      string
	claimErr       error
}

type memoryStore struct {
	mu       sync.Mutex
	events   map[string]*memoryRecord
	chatLock map[int64]*sync.Mutex
	nextID   int64
	saveErr  error
}

func newMemoryStore() *memoryStore {
	return &memoryStore{events: make(map[string]*memoryRecord), chatLock: make(map[int64]*sync.Mutex)}
}

func (s *memoryStore) save(_ context.Context, key string, update maxapi.Update, _ []byte) (eventRef, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return eventRef{}, false, s.saveErr
	}
	if existing, ok := s.events[key]; ok {
		shouldQueue := existing.status == "received" ||
			(existing.status == "failed" && !existing.nextAttempt.After(time.Now()))
		return existing.ref, shouldQueue, nil
	}
	s.nextID++
	ref := eventRef{Key: key, ChatID: update.ResolvedChatID()}
	s.events[key] = &memoryRecord{
		ref:            ref,
		update:         update,
		status:         "received",
		nextAttempt:    time.Now(),
		eventTimestamp: update.Timestamp,
		created:        s.nextID,
	}
	return ref, true, nil
}

func (s *memoryStore) ready(context.Context) error { return nil }

func (s *memoryStore) withChatLock(_ context.Context, chatID int64, fn func(eventSession) error) error {
	s.mu.Lock()
	lock := s.chatLock[chatID]
	if lock == nil {
		lock = &sync.Mutex{}
		s.chatLock[chatID] = lock
	}
	s.mu.Unlock()
	lock.Lock()
	defer lock.Unlock()
	return fn(memorySession{store: s})
}

func (s *memoryStore) due(_ context.Context, limit int, _ time.Duration) ([]eventRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	earliest := make(map[int64]*memoryRecord)
	for _, event := range s.events {
		if event.status != "received" && event.status != "processing" && event.status != "failed" {
			continue
		}
		current := earliest[event.ref.ChatID]
		if current == nil || memoryRecordBefore(event, current) {
			earliest[event.ref.ChatID] = event
		}
	}
	ordered := make([]*memoryRecord, 0, len(earliest))
	for _, event := range earliest {
		ordered = append(ordered, event)
	}
	sort.Slice(ordered, func(i, j int) bool { return memoryRecordBefore(ordered[i], ordered[j]) })
	refs := make([]eventRef, 0, min(limit, len(ordered)))
	now := time.Now()
	for _, event := range ordered {
		if len(refs) == limit {
			break
		}
		if (event.status == "received" || event.status == "failed") && !event.nextAttempt.After(now) {
			refs = append(refs, event.ref)
		}
	}
	return refs, nil
}

func (s *memoryStore) nextDue(_ context.Context, chatID int64, _ time.Duration) (eventRef, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	var selected *memoryRecord
	for _, event := range s.events {
		if event.ref.ChatID != chatID || (event.status != "received" && event.status != "processing" && event.status != "failed") {
			continue
		}
		if selected == nil || memoryRecordBefore(event, selected) {
			selected = event
		}
	}
	if selected == nil {
		return eventRef{}, false, nil
	}
	if selected.status == "processing" || selected.nextAttempt.After(now) {
		return eventRef{}, false, nil
	}
	return selected.ref, true, nil
}

func (s *memoryStore) cleanup(context.Context, time.Time, time.Time) (int64, error) {
	return 0, nil
}

type memorySession struct{ store *memoryStore }

func (s memorySession) claim(_ context.Context, ref eventRef, _ time.Duration) (storedEvent, bool, error) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	event := s.store.events[ref.Key]
	if event == nil || (event.status != "received" && event.status != "failed") || event.nextAttempt.After(time.Now()) {
		return storedEvent{}, false, nil
	}
	for _, older := range s.store.events {
		if older.ref.ChatID == event.ref.ChatID && memoryRecordBefore(older, event) &&
			(older.status == "received" || older.status == "processing" || older.status == "failed") {
			return storedEvent{}, false, nil
		}
	}
	event.status = "processing"
	event.attempts++
	stored := storedEvent{Ref: ref, Update: event.update, Attempts: event.attempts}
	if event.claimErr != nil {
		return stored, true, event.claimErr
	}
	return stored, true, nil
}

func memoryRecordBefore(left, right *memoryRecord) bool {
	if left.eventTimestamp != right.eventTimestamp {
		return left.eventTimestamp < right.eventTimestamp
	}
	return left.created < right.created
}

func (s memorySession) complete(_ context.Context, key string) error {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	event := s.store.events[key]
	if event == nil || event.status != "processing" {
		return errors.New("event is not processing")
	}
	event.status = "completed"
	event.update = maxapi.Update{}
	event.payloadCleared = true
	event.lastError = ""
	return nil
}

func (s memorySession) fail(_ context.Context, key, message string, nextAttempt time.Time, terminal bool) error {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	event := s.store.events[key]
	if event == nil || event.status != "processing" {
		return errors.New("event is not processing")
	}
	if terminal {
		event.status = "dead"
		event.nextAttempt = time.Time{}
	} else {
		event.status = "failed"
		event.nextAttempt = nextAttempt
	}
	event.lastError = message
	return nil
}

func (s memorySession) interrupt(_ context.Context, key, message string, nextAttempt time.Time) error {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	event := s.store.events[key]
	if event == nil || event.status != "processing" {
		return errors.New("event is not processing")
	}
	event.status = "failed"
	event.attempts = max(event.attempts-1, 0)
	event.nextAttempt = nextAttempt
	event.lastError = message
	return nil
}
