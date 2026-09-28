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

type temporaryDeliveryError struct{}

func (temporaryDeliveryError) Error() string   { return "temporary network failure" }
func (temporaryDeliveryError) Timeout() bool   { return true }
func (temporaryDeliveryError) Temporary() bool { return true }

type flakyMessageMessenger struct {
	mu       sync.Mutex
	attempts []maxapi.NewMessageBody
}

func (m *flakyMessageMessenger) SendMessage(_ context.Context, _ int64, _ int64, body maxapi.NewMessageBody) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.attempts = append(m.attempts, body)
	if len(m.attempts) == 1 {
		return temporaryDeliveryError{}
	}
	return nil
}

func (*flakyMessageMessenger) AnswerCallback(context.Context, string, int64, maxapi.SendAnswerRequest) error {
	return nil
}

func (*flakyMessageMessenger) SendFile(context.Context, int64, int64, string, []byte, string, ...[]maxapi.Button) error {
	return nil
}

type failingMessenger struct {
	err error
}

func (m failingMessenger) SendMessage(context.Context, int64, int64, maxapi.NewMessageBody) error {
	return m.err
}

func (m failingMessenger) AnswerCallback(context.Context, string, int64, maxapi.SendAnswerRequest) error {
	return m.err
}

func (m failingMessenger) SendFile(context.Context, int64, int64, string, []byte, string, ...[]maxapi.Button) error {
	return m.err
}

func TestHandleReturnsMessageDeliveryFailure(t *testing.T) {
	deliveryErr := errors.New("MAX API unavailable")
	h := NewHandler(failingMessenger{err: deliveryErr}, loadTestCatalog(t), nil, nil, AnalysisConfig{})
	update := maxapi.Update{
		UpdateType: maxapi.UpdateMessageCreated,
		ChatID:     42,
		Message:    &maxapi.Message{Body: maxapi.MessageBody{Text: "/start"}},
	}

	if err := h.Handle(t.Context(), update); !errors.Is(err, deliveryErr) {
		t.Fatalf("Handle error = %v, want delivery error", err)
	}
}

func TestHandleReturnsCallbackDeliveryFailure(t *testing.T) {
	deliveryErr := errors.New("MAX callback unavailable")
	h := NewHandler(failingMessenger{err: deliveryErr}, loadTestCatalog(t), nil, nil, AnalysisConfig{})
	update := maxapi.Update{
		UpdateType: maxapi.UpdateMessageCallback,
		ChatID:     42,
		Callback: &maxapi.Callback{
			CallbackID: "callback",
			Payload:    payloadHelp,
			User:       maxapi.User{UserID: 7},
		},
	}

	if err := h.Handle(t.Context(), update); !errors.Is(err, deliveryErr) {
		t.Fatalf("Handle error = %v, want callback delivery error", err)
	} else {
		var classified interface{ NonRetryable() bool }
		if !errors.As(err, &classified) || !classified.NonRetryable() {
			t.Fatalf("delivery error was not classified as non-retryable: %v", err)
		}
	}
}

func TestMessageRetryReplaysExactResponseWithoutRepeatingTransition(t *testing.T) {
	messenger := &flakyMessageMessenger{}
	h := NewHandler(messenger, loadTestCatalog(t), nil, nil, AnalysisConfig{})
	h.sessions.set(44, session{Step: stepAwaitRegion, Generation: 3})
	update := maxapi.Update{
		UpdateType: maxapi.UpdateMessageCreated,
		Timestamp:  100,
		MessageID:  "region-message",
		ChatID:     44,
		Message:    &maxapi.Message{Body: maxapi.MessageBody{Text: "Москва"}},
	}

	if err := h.Handle(t.Context(), update); err == nil {
		t.Fatal("first Handle() unexpectedly succeeded")
	}
	if got := h.sessions.get(44).Step; got != stepAwaitProgram {
		t.Fatalf("step after first attempt = %q, want await_program", got)
	}
	if err := h.Handle(t.Context(), update); err != nil {
		t.Fatalf("replayed Handle() error = %v", err)
	}
	if got := h.sessions.get(44).Step; got != stepAwaitProgram {
		t.Fatalf("transition was repeated on delivery retry: step=%q", got)
	}
	if err := h.Handle(t.Context(), update); err != nil {
		t.Fatalf("completed duplicate Handle() error = %v", err)
	}

	messenger.mu.Lock()
	defer messenger.mu.Unlock()
	if len(messenger.attempts) != 2 {
		t.Fatalf("delivery attempts = %d, want 2", len(messenger.attempts))
	}
	for index, body := range messenger.attempts {
		if body.Text == nil || !strings.Contains(*body.Text, "Шаг 2/3") {
			t.Fatalf("attempt %d replayed wrong response: %#v", index+1, body.Text)
		}
	}
}

func TestPendingDeliveryReceiptSurvivesProcessRestart(t *testing.T) {
	repository := newMemorySessionRepository()
	firstMessenger := &flakyMessageMessenger{}
	first := NewHandlerWithSessionRepository(firstMessenger, loadTestCatalog(t), nil, nil, AnalysisConfig{}, repository, time.Hour)
	first.sessions.set(45, session{Step: stepAwaitRegion, Generation: 2})
	update := maxapi.Update{
		UpdateType: maxapi.UpdateMessageCreated,
		Timestamp:  101,
		MessageID:  "durable-region-message",
		ChatID:     45,
		Message:    &maxapi.Message{Body: maxapi.MessageBody{Text: "Москва"}},
	}
	if err := first.Handle(t.Context(), update); err == nil {
		t.Fatal("first Handle() unexpectedly succeeded")
	}

	secondMessenger := newFakeMessenger()
	restarted := NewHandlerWithSessionRepository(secondMessenger, loadTestCatalog(t), nil, nil, AnalysisConfig{}, repository, time.Hour)
	if err := restarted.Handle(t.Context(), update); err != nil {
		t.Fatalf("restarted Handle() error = %v", err)
	}
	message := receive(t, secondMessenger.messages)
	if message.body.Text == nil || !strings.Contains(*message.body.Text, "Шаг 2/3") {
		t.Fatalf("restarted delivery replayed wrong response: %#v", message.body.Text)
	}
	if got := restarted.sessions.get(45).Step; got != stepAwaitProgram {
		t.Fatalf("restored state = %q, want await_program", got)
	}
}

func TestIncompleteTransitionRollsBackAfterProcessRestart(t *testing.T) {
	repository := newMemorySessionRepository()
	firstMessenger := newFakeMessenger()
	first := NewHandlerWithSessionRepository(firstMessenger, loadTestCatalog(t), nil, nil, AnalysisConfig{}, repository, time.Hour)
	first.sessions.set(46, session{Step: stepAwaitRegion, Generation: 2})
	repository.mu.Lock()
	repository.failSaveAt = 3 // initial state, transitioned state, then receipt
	repository.saveErr = errors.New("database interrupted before receipt")
	repository.mu.Unlock()

	update := maxapi.Update{
		UpdateType: maxapi.UpdateMessageCreated,
		Timestamp:  102,
		MessageID:  "transition-crash",
		ChatID:     46,
		Message:    &maxapi.Message{Body: maxapi.MessageBody{Text: "Москва"}},
	}
	if err := first.Handle(t.Context(), update); !errors.Is(err, repository.saveErr) {
		t.Fatalf("first Handle() error = %v, want injected persistence failure", err)
	}
	select {
	case message := <-firstMessenger.messages:
		t.Fatalf("response was sent before its receipt became durable: %#v", message)
	default:
	}

	repository.mu.Lock()
	stored := cloneSession(repository.sessions[46])
	repository.mu.Unlock()
	if stored.Step != stepAwaitProgram || stored.LastTransitionEventKey == "" || len(stored.TransitionBackup) == 0 || stored.LastDelivery != nil {
		t.Fatalf("unexpected pre-crash durable state: %+v", stored)
	}

	restartedMessenger := newFakeMessenger()
	restarted := NewHandlerWithSessionRepository(restartedMessenger, loadTestCatalog(t), nil, nil, AnalysisConfig{}, repository, time.Hour)
	if err := restarted.Handle(t.Context(), update); err != nil {
		t.Fatalf("Handle() after restart: %v", err)
	}
	message := receive(t, restartedMessenger.messages)
	if message.body.Text == nil || !strings.Contains(*message.body.Text, "Шаг 2/3") {
		t.Fatalf("recovered event produced wrong response: %#v", message.body.Text)
	}
	recovered := restarted.sessions.get(46)
	if recovered.Step != stepAwaitProgram || recovered.LastTransitionEventKey != "" || recovered.LastDelivery == nil || !recovered.LastDelivery.Delivered {
		t.Fatalf("recovered durable state = %+v", recovered)
	}
}

func TestCompletedDeliveryReceiptDropsPayloadAndAddressingData(t *testing.T) {
	repository := newMemorySessionRepository()
	messenger := newFakeMessenger()
	h := NewHandlerWithSessionRepository(messenger, loadTestCatalog(t), nil, nil, AnalysisConfig{}, repository, time.Hour)
	update := maxapi.Update{
		UpdateType: maxapi.UpdateBotStarted,
		Timestamp:  103,
		ChatID:     47,
		User:       &maxapi.User{UserID: 88, FirstName: "Иван"},
	}
	if err := h.Handle(t.Context(), update); err != nil {
		t.Fatal(err)
	}
	receive(t, messenger.messages)

	repository.mu.Lock()
	stored := cloneSession(repository.sessions[47])
	repository.mu.Unlock()
	if stored.LastDelivery == nil || !stored.LastDelivery.Delivered || stored.LastDelivery.EventKey == "" {
		t.Fatalf("completed receipt missing: %+v", stored.LastDelivery)
	}
	if len(stored.LastDelivery.Payload) != 0 || stored.LastDelivery.Kind != "" || stored.LastDelivery.ChatID != 0 || stored.LastDelivery.UserID != 0 || stored.LastDelivery.CallbackID != "" || stored.LastDelivery.Purpose != "" {
		t.Fatalf("completed receipt retained response data: %+v", stored.LastDelivery)
	}
}

func TestInterruptedAnalysisDoesNotReplayStaleStartedReceipt(t *testing.T) {
	repository := newMemorySessionRepository()
	recorded := newFakeMessenger()
	firstAnalyzer := &blockingAnalyzer{started: make(chan struct{}), release: make(chan struct{})}
	first := NewHandlerWithSessionRepository(
		answerFailingMessenger{fakeMessenger: recorded, err: temporaryDeliveryError{}},
		loadTestCatalog(t), firstAnalyzer, fakeReportGenerator{},
		withAllCatalog(AnalysisConfig{Workers: 1, Queue: 1, Timeout: time.Minute}), repository, time.Hour,
	)
	first.sessions.set(48, session{
		Step: stepReady, RegionCode: "7700000000000", RegionName: "Москва",
		ProgramCode: "09.02.07", Qualification: "Программист",
	})
	first.analysis.started.Store(true)
	update := analysisUpdate(48, "restart-during-analysis")
	if err := first.Handle(t.Context(), update); err == nil {
		t.Fatal("initial callback unexpectedly succeeded")
	}
	startedAnswer := receive(t, recorded.answers)
	if startedAnswer.req.Message == nil || startedAnswer.req.Message.Text == nil || !strings.Contains(*startedAnswer.req.Message.Text, "Запустил анализ") {
		t.Fatalf("unexpected initial answer: %#v", startedAnswer.req)
	}

	repository.mu.Lock()
	beforeRestart := cloneSession(repository.sessions[48])
	repository.mu.Unlock()
	if beforeRestart.Step != stepAnalyzing || beforeRestart.LastDelivery == nil || beforeRestart.LastDelivery.Purpose != "analysis_start" || beforeRestart.LastDelivery.Delivered {
		t.Fatalf("unexpected durable analysis state: %+v", beforeRestart)
	}

	restartedMessenger := newFakeMessenger()
	restartedAnalyzer := &blockingAnalyzer{started: make(chan struct{}), release: make(chan struct{})}
	restarted := NewHandlerWithSessionRepository(
		restartedMessenger, loadTestCatalog(t), restartedAnalyzer, fakeReportGenerator{},
		withAllCatalog(AnalysisConfig{Workers: 1, Queue: 1, Timeout: time.Minute}), repository, time.Hour,
	)
	restarted.analysis.started.Store(true)
	if err := restarted.Handle(t.Context(), update); err != nil {
		t.Fatalf("callback retry after restart: %v", err)
	}
	recoveryAnswer := receive(t, restartedMessenger.answers)
	if recoveryAnswer.req.Message == nil || recoveryAnswer.req.Message.Text == nil || !strings.Contains(*recoveryAnswer.req.Message.Text, "прерван перезапуском") {
		t.Fatalf("stale start acknowledgement was replayed: %#v", recoveryAnswer.req)
	}
	restartedAnalyzer.mu.Lock()
	calls := restartedAnalyzer.calls
	restartedAnalyzer.mu.Unlock()
	if calls != 0 {
		t.Fatalf("analysis was relaunched from duplicate callback: calls=%d", calls)
	}
}

func TestAnalysisJobStartsOnlyAfterDurableAcknowledgement(t *testing.T) {
	repository := newMemorySessionRepository()
	firstMessenger := newFakeMessenger()
	firstAnalyzer := &blockingAnalyzer{started: make(chan struct{}), release: make(chan struct{})}
	first := NewHandlerWithSessionRepository(
		firstMessenger, loadTestCatalog(t), firstAnalyzer, fakeReportGenerator{},
		withAllCatalog(AnalysisConfig{Workers: 1, Queue: 1, Timeout: time.Minute}), repository, time.Hour,
	)
	first.sessions.set(49, session{
		Step: stepReady, RegionCode: "7700000000000", RegionName: "Москва",
		ProgramCode: "09.02.07", Qualification: "Программист",
	})
	repository.mu.Lock()
	repository.failSaveAt = 3 // initial state, analyzing transition, then start receipt
	repository.saveErr = errors.New("database unavailable before start receipt")
	repository.mu.Unlock()
	firstCtx, stopFirst := context.WithCancel(context.Background())
	first.StartAnalysisWorkers(firstCtx)
	update := analysisUpdate(49, "durable-start-gate")
	if err := first.Handle(t.Context(), update); !errors.Is(err, repository.saveErr) {
		t.Fatalf("first Handle() error = %v, want receipt persistence failure", err)
	}
	select {
	case <-firstAnalyzer.started:
		t.Fatal("analysis started before its acknowledgement was durable")
	case <-time.After(100 * time.Millisecond):
	}
	stopFirst()
	first.WaitAnalysisWorkers()

	secondMessenger := newFakeMessenger()
	secondRelease := make(chan struct{})
	secondAnalyzer := &blockingAnalyzer{started: make(chan struct{}), release: secondRelease, err: errors.New("stop after proof")}
	second := NewHandlerWithSessionRepository(
		secondMessenger, loadTestCatalog(t), secondAnalyzer, fakeReportGenerator{},
		withAllCatalog(AnalysisConfig{Workers: 1, Queue: 1, Timeout: time.Minute}), repository, time.Hour,
	)
	secondCtx, stopSecond := context.WithCancel(context.Background())
	second.StartAnalysisWorkers(secondCtx)
	t.Cleanup(func() {
		stopSecond()
		select {
		case <-secondRelease:
		default:
			close(secondRelease)
		}
		second.WaitAnalysisWorkers()
	})
	if err := second.Handle(t.Context(), update); err != nil {
		t.Fatalf("retry after restart: %v", err)
	}
	answer := receive(t, secondMessenger.answers)
	if answer.req.Message == nil || answer.req.Message.Text == nil || !strings.Contains(*answer.req.Message.Text, "Запустил анализ") {
		t.Fatalf("retry was not accepted: %#v", answer.req)
	}
	receive(t, secondAnalyzer.started)
	secondAnalyzer.mu.Lock()
	calls := secondAnalyzer.calls
	secondAnalyzer.mu.Unlock()
	if calls != 1 {
		t.Fatalf("analysis calls after durable retry = %d, want 1", calls)
	}
}
