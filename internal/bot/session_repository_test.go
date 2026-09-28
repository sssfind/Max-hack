package bot

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"Max-hack/internal/maxapi"
)

type memorySessionRepository struct {
	mu         sync.Mutex
	sessions   map[int64]session
	expires    map[int64]time.Time
	deleteErr  error
	loadErr    error
	saveCalls  int
	failSaveAt int
	saveErr    error
}

type blockingSaveSessionRepository struct {
	*memorySessionRepository
	blockChat int64
	started   chan struct{}
	release   chan struct{}
	once      sync.Once
}

func (r *blockingSaveSessionRepository) Save(
	ctx context.Context,
	chatID int64,
	sess session,
	expiresAt time.Time,
) error {
	shouldBlock := false
	if chatID == r.blockChat {
		r.once.Do(func() {
			shouldBlock = true
			close(r.started)
		})
	}
	if shouldBlock {
		select {
		case <-r.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return r.memorySessionRepository.Save(ctx, chatID, sess, expiresAt)
}

type flakySaveSessionRepository struct {
	*memorySessionRepository
	failureMu        sync.Mutex
	remainingFailure int
	attempts         int
}

func (r *flakySaveSessionRepository) Save(
	ctx context.Context,
	chatID int64,
	sess session,
	expiresAt time.Time,
) error {
	r.failureMu.Lock()
	r.attempts++
	if r.remainingFailure > 0 {
		r.remainingFailure--
		r.failureMu.Unlock()
		return errors.New("temporary save failure")
	}
	r.failureMu.Unlock()
	return r.memorySessionRepository.Save(ctx, chatID, sess, expiresAt)
}

func (r *flakySaveSessionRepository) failNext(count int) int {
	r.failureMu.Lock()
	defer r.failureMu.Unlock()
	r.remainingFailure = count
	return r.attempts
}

func (r *memorySessionRepository) Ready(context.Context) error { return nil }

func newMemorySessionRepository() *memorySessionRepository {
	return &memorySessionRepository{sessions: make(map[int64]session), expires: make(map[int64]time.Time)}
}

func TestSessionStoreSlowSaveDoesNotBlockOtherChat(t *testing.T) {
	repository := &blockingSaveSessionRepository{
		memorySessionRepository: newMemorySessionRepository(),
		blockChat:               1,
		started:                 make(chan struct{}),
		release:                 make(chan struct{}),
	}
	store := newSessionStoreWithRepository(repository, time.Hour)

	chatADone := make(chan struct{})
	go func() {
		store.set(1, session{Step: stepAwaitProgram, RegionCode: "77"})
		close(chatADone)
	}()
	select {
	case <-repository.started:
	case <-time.After(time.Second):
		t.Fatal("chat A did not reach the blocking repository save")
	}

	released := false
	defer func() {
		if !released {
			close(repository.release)
		}
	}()
	chatBDone := make(chan session, 1)
	go func() {
		store.set(2, session{Step: stepAwaitProgram, RegionCode: "78"})
		chatBDone <- store.get(2)
	}()
	select {
	case got := <-chatBDone:
		if got.RegionCode != "78" || got.Step != stepAwaitProgram {
			t.Fatalf("chat B state = %+v", got)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("slow save for chat A blocked chat B")
	}

	close(repository.release)
	released = true
	select {
	case <-chatADone:
	case <-time.After(time.Second):
		t.Fatal("chat A did not finish after repository save was released")
	}
}

func TestSessionStoreFinishAnalysisRetriesPersistence(t *testing.T) {
	repository := &flakySaveSessionRepository{memorySessionRepository: newMemorySessionRepository()}
	store := newSessionStoreWithRepository(repository, time.Hour)
	store.set(11, session{Step: stepReady, RegionCode: "77", ProgramCode: "09.02.07", Generation: 4})
	selected, err := store.startAnalysis(11, 4, "09.02.07", "callback", func() {})
	if err != nil {
		t.Fatal(err)
	}

	before := repository.failNext(2)
	store.finishAnalysis(11, selected.Generation, selected.AnalysisID)

	repository.failureMu.Lock()
	attempts := repository.attempts - before
	repository.failureMu.Unlock()
	if attempts != 3 {
		t.Fatalf("finish persistence attempts = %d, want 3", attempts)
	}
	repository.mu.Lock()
	persisted := repository.sessions[11]
	repository.mu.Unlock()
	if persisted.Step != stepReady || persisted.Generation != selected.Generation || persisted.AnalysisID != selected.AnalysisID {
		t.Fatalf("persisted finished analysis = %+v", persisted)
	}

	newer := store.reset(11)
	store.finishAnalysis(11, selected.Generation, selected.AnalysisID)
	repository.mu.Lock()
	persisted = repository.sessions[11]
	repository.mu.Unlock()
	if persisted.Step != stepAwaitRegion || persisted.Generation != newer.Generation {
		t.Fatalf("stale finish overwrote newer generation: %+v", persisted)
	}
}

func TestConcurrentEventDoesNotClaimAsyncAnalysisFinish(t *testing.T) {
	repository := newMemorySessionRepository()
	store := newSessionStoreWithRepository(repository, time.Hour)
	store.set(12, session{Step: stepReady, RegionCode: "77", ProgramCode: "09.02.07", Generation: 5})
	selected, err := store.startAnalysis(12, 5, "09.02.07", "analysis-callback", func() {})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.beginEvent(12, "message:message_created:help"); err != nil {
		t.Fatal(err)
	}
	store.finishAnalysis(12, selected.Generation, selected.AnalysisID)
	store.endEvent(12, "message:message_created:help")

	restarted := newSessionStoreWithRepository(repository, time.Hour)
	if rolledBack, err := restarted.rollbackIncompleteTransition(12, "message:message_created:help"); err != nil {
		t.Fatal(err)
	} else if rolledBack {
		t.Fatal("async analysis completion was incorrectly marked as a webhook transition")
	}
	got := restarted.get(12)
	if got.Step != stepReady || got.AnalysisInterrupted {
		t.Fatalf("analysis completion was reverted after restart: %+v", got)
	}
}

func TestRollbackPreservesAnalysisFinishedDuringFeedbackTransition(t *testing.T) {
	repository := newMemorySessionRepository()
	store := newSessionStoreWithRepository(repository, time.Hour)
	store.set(13, session{Step: stepReady, RegionCode: "77", ProgramCode: "09.02.07", Generation: 6})
	selected, err := store.startAnalysis(13, 6, "09.02.07", "analysis-callback", func() {})
	if err != nil {
		t.Fatal(err)
	}
	const eventKey = "callback:feedback"
	if err := store.beginEvent(13, eventKey); err != nil {
		t.Fatal(err)
	}
	if !store.recordFeedback(13, selected.AnalysisID) {
		t.Fatal("feedback transition was not recorded")
	}
	store.finishAnalysis(13, selected.Generation, selected.AnalysisID)
	store.endEvent(13, eventKey)

	restarted := newSessionStoreWithRepository(repository, time.Hour)
	if rolledBack, err := restarted.rollbackIncompleteTransition(13, eventKey); err != nil {
		t.Fatal(err)
	} else if !rolledBack {
		t.Fatal("incomplete feedback transition was not rolled back")
	}
	got := restarted.get(13)
	if got.Step != stepReady || got.AnalysisInterrupted || got.FeedbackFor != 0 {
		t.Fatalf("rollback resurrected a completed analysis: %+v", got)
	}
}

func (r *memorySessionRepository) Load(_ context.Context, chatID int64) (session, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.loadErr != nil {
		return session{}, false, r.loadErr
	}
	sess, ok := r.sessions[chatID]
	if !ok || time.Now().After(r.expires[chatID]) {
		return session{}, false, nil
	}
	return cloneSession(sess), true, nil
}

func TestHandlerDoesNotOverwriteDurableSessionWhenLoadFails(t *testing.T) {
	repository := newMemorySessionRepository()
	seed := newSessionStoreWithRepository(repository, time.Hour)
	seed.set(24, session{
		Step: stepReady, RegionCode: "77", RegionName: "Москва",
		ProgramCode: "09.02.11", Qualification: "Программист", Generation: 8,
	})
	loadErr := errors.New("temporary database read failure")
	repository.mu.Lock()
	repository.loadErr = loadErr
	repository.mu.Unlock()

	messenger := newFakeMessenger()
	h := NewHandlerWithSessionRepository(messenger, loadTestCatalog(t), nil, nil, AnalysisConfig{}, repository, time.Hour)
	update := maxapi.Update{
		UpdateType: maxapi.UpdateMessageCallback,
		ChatID:     24,
		Callback: &maxapi.Callback{
			CallbackID: "restart-after-db-error",
			Payload:    payloadRestart,
			User:       maxapi.User{UserID: 7},
		},
	}
	if err := h.Handle(t.Context(), update); !errors.Is(err, loadErr) {
		t.Fatalf("Handle() error = %v, want load failure", err)
	}
	select {
	case answer := <-messenger.answers:
		t.Fatalf("bot answered after failing to load durable state: %#v", answer)
	default:
	}
	repository.mu.Lock()
	persisted := cloneSession(repository.sessions[24])
	repository.loadErr = nil
	repository.mu.Unlock()
	if persisted.Generation != 8 || persisted.Step != stepReady || persisted.ProgramCode != "09.02.11" {
		t.Fatalf("load failure overwrote durable state: %+v", persisted)
	}

	if err := h.Handle(t.Context(), update); err != nil {
		t.Fatalf("retry after database recovery: %v", err)
	}
	receive(t, messenger.answers)
	repository.mu.Lock()
	persisted = cloneSession(repository.sessions[24])
	repository.mu.Unlock()
	if persisted.Generation != 9 || persisted.Step != stepAwaitRegion {
		t.Fatalf("recovered event did not continue from durable generation: %+v", persisted)
	}
}

func (r *memorySessionRepository) Save(_ context.Context, chatID int64, sess session, expiresAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.saveCalls++
	if r.failSaveAt > 0 && r.saveCalls == r.failSaveAt {
		if r.saveErr != nil {
			return r.saveErr
		}
		return errors.New("injected session save failure")
	}
	r.sessions[chatID] = cloneSession(sess)
	r.expires[chatID] = expiresAt
	return nil
}

func TestPersistedSessionKeepsCrashRecoveryMarkers(t *testing.T) {
	original := session{
		Step:                   stepAwaitProgram,
		Generation:             4,
		LastTransitionEventKey: "message:message_created:42",
		TransitionBackup:       []byte(`{"step":"await_region","generation":4}`),
		UpdatedAt:              time.Now(),
	}
	restored := persistedFromRuntime(original).runtime()
	if restored.LastTransitionEventKey != original.LastTransitionEventKey || string(restored.TransitionBackup) != string(original.TransitionBackup) {
		t.Fatalf("transition recovery fields were lost: %+v", restored)
	}
}

func (r *memorySessionRepository) Delete(_ context.Context, chatID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.deleteErr != nil {
		return r.deleteErr
	}
	delete(r.sessions, chatID)
	delete(r.expires, chatID)
	return nil
}

func TestSessionStoreForgetReportsPersistenceFailure(t *testing.T) {
	repository := newMemorySessionRepository()
	store := newSessionStoreWithRepository(repository, time.Hour)
	store.set(9, session{Step: stepAwaitProgram, RegionCode: "77"})
	repository.deleteErr = errors.New("database unavailable")

	if err := store.forget(9); !errors.Is(err, repository.deleteErr) {
		t.Fatalf("forget error = %v, want %v", err, repository.deleteErr)
	}
	if got := newSessionStoreWithRepository(repository, time.Hour).get(9); got.Step != stepAwaitProgram {
		t.Fatalf("persisted state was falsely deleted: %+v", got)
	}
}

func (r *memorySessionRepository) Cleanup(_ context.Context, before time.Time) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var removed int64
	for chatID, expiresAt := range r.expires {
		if expiresAt.Before(before) {
			delete(r.sessions, chatID)
			delete(r.expires, chatID)
			removed++
		}
	}
	return removed, nil
}

func TestSessionStoreRestoresSelectionAfterRestart(t *testing.T) {
	repository := newMemorySessionRepository()
	first := newSessionStoreWithRepository(repository, time.Hour)
	first.set(42, session{
		Step: stepReady, RegionCode: "77", RegionName: "Москва",
		ProgramCode: "09.02.11", Qualification: "Программист", Generation: 3,
	})

	restarted := newSessionStoreWithRepository(repository, time.Hour)
	restored := restarted.get(42)
	if restored.Step != stepReady || restored.ProgramCode != "09.02.11" || restored.RegionCode != "77" {
		t.Fatalf("restored session = %+v", restored)
	}
}

func TestSessionStoreTurnsInterruptedAnalysisIntoRetryableSelection(t *testing.T) {
	repository := newMemorySessionRepository()
	stored := session{
		Step: stepAnalyzing, RegionCode: "77", RegionName: "Москва",
		ProgramCode: "09.02.11", Qualification: "Программист", Generation: 1, AnalysisID: 2,
		UpdatedAt: time.Now(),
	}
	if err := repository.Save(t.Context(), 7, stored, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	restored := newSessionStoreWithRepository(repository, time.Hour).get(7)
	if restored.Step != stepReady {
		t.Fatalf("step = %q, want ready", restored.Step)
	}
	if !restored.AnalysisInterrupted {
		t.Fatal("interrupted analysis was restored without an interruption marker")
	}

	restoredAgain := newSessionStoreWithRepository(repository, time.Hour).get(7)
	if !restoredAgain.AnalysisInterrupted || restoredAgain.Step != stepReady {
		t.Fatalf("persisted recovery marker was lost: %+v", restoredAgain)
	}
}

func TestSessionStoreForgetDeletesPersistedState(t *testing.T) {
	repository := newMemorySessionRepository()
	store := newSessionStoreWithRepository(repository, time.Hour)
	store.set(5, session{Step: stepAwaitProgram, RegionCode: "77"})
	if err := store.forget(5); err != nil {
		t.Fatal(err)
	}

	restarted := newSessionStoreWithRepository(repository, time.Hour)
	if got := restarted.get(5); got.Step != stepIdle {
		t.Fatalf("forgotten session restored as %+v", got)
	}
}

func TestSessionStoreResetAfterRestartKeepsGenerationMonotonic(t *testing.T) {
	repository := newMemorySessionRepository()
	first := newSessionStoreWithRepository(repository, time.Hour)
	first.set(17, session{
		Step: stepReady, RegionCode: "77", RegionName: "Москва",
		ProgramCode: "09.02.11", Generation: 8,
	})

	restarted := newSessionStoreWithRepository(repository, time.Hour)
	reset := restarted.reset(17)
	if reset.Generation != 9 || reset.Step != stepAwaitRegion {
		t.Fatalf("reset session = %+v, want generation 9 at region selection", reset)
	}

	restoredAgain := newSessionStoreWithRepository(repository, time.Hour).get(17)
	if restoredAgain.Generation != 9 || restoredAgain.Step != stepAwaitRegion {
		t.Fatalf("persisted reset session = %+v", restoredAgain)
	}
}

func TestSessionStoreCanUsePersistedReadySelectionAfterRestart(t *testing.T) {
	repository := newMemorySessionRepository()
	first := newSessionStoreWithRepository(repository, time.Hour)
	first.set(23, session{
		Step: stepReady, RegionCode: "77", RegionName: "Москва",
		ProgramCode: "09.02.11", Qualification: "Программист", Generation: 4,
	})

	restarted := newSessionStoreWithRepository(repository, time.Hour)
	selection, err := restarted.startAnalysis(23, 4, "09.02.11", "callback-1", func() {})
	if err != nil {
		t.Fatalf("start analysis from persisted selection: %v", err)
	}
	if selection.Generation != 4 || selection.ProgramCode != "09.02.11" || selection.AnalysisID != 1 {
		t.Fatalf("selection = %+v", selection)
	}
	if got := restarted.get(23); got.AnalysisInterrupted {
		t.Fatal("retry did not clear the interrupted-analysis marker")
	}
}

func TestHandlerExplainsAnalysisInterruptedByRestart(t *testing.T) {
	repository := newMemorySessionRepository()
	stored := session{
		Step: stepAnalyzing, RegionCode: "7700000000000", RegionName: "Москва",
		ProgramCode: "09.02.07", Qualification: "Программист", Generation: 3, AnalysisID: 4,
		UpdatedAt: time.Now(),
	}
	if err := repository.Save(t.Context(), 91, stored, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	messenger := newFakeMessenger()
	h := NewHandlerWithSessionRepository(
		messenger, loadTestCatalog(t), nil, nil, withAllCatalog(AnalysisConfig{}), repository, time.Hour,
	)
	messageUpdate := maxapi.Update{
		UpdateType: maxapi.UpdateMessageCreated,
		ChatID:     91,
		Message:    &maxapi.Message{Body: maxapi.MessageBody{Text: "статус"}},
	}
	if err := h.Handle(t.Context(), messageUpdate); err != nil {
		t.Fatal(err)
	}
	message := receive(t, messenger.messages)
	if message.body.Text == nil || !strings.Contains(*message.body.Text, "прерван перезапуском") || !strings.Contains(*message.body.Text, "повторно") {
		t.Fatalf("unexpected recovery message: %#v", message.body.Text)
	}
	if !hasCallbackPrefix(inlineKeyboardButtons(message.body), payloadAnalyze) {
		t.Fatalf("recovery message has no retry action: %#v", message.body)
	}

	cancelUpdate := maxapi.Update{
		UpdateType: maxapi.UpdateMessageCallback,
		ChatID:     91,
		Callback: &maxapi.Callback{
			CallbackID: "old-cancel",
			Payload:    payloadCancel + "3:4",
			User:       maxapi.User{UserID: 5},
		},
	}
	if err := h.Handle(t.Context(), cancelUpdate); err != nil {
		t.Fatal(err)
	}
	answer := receive(t, messenger.answers)
	if answer.req.Message == nil || answer.req.Message.Text == nil || !strings.Contains(*answer.req.Message.Text, "прерван перезапуском") {
		t.Fatalf("old cancel callback was misleading: %#v", answer.req)
	}
}
