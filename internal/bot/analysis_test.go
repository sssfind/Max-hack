package bot

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"Max-hack/internal/catalog"
	"Max-hack/internal/maxapi"
	"Max-hack/internal/pdfreport"
	"Max-hack/internal/vacancies"
)

type recordedAnswer struct {
	callbackID string
	chatID     int64
	req        maxapi.SendAnswerRequest
}

type recordedMessage struct {
	chatID int64
	body   maxapi.NewMessageBody
}

type recordedFile struct {
	chatID  int64
	name    string
	data    []byte
	caption string
}

type fakeMessenger struct {
	answers  chan recordedAnswer
	messages chan recordedMessage
	files    chan recordedFile
}

func newFakeMessenger() *fakeMessenger {
	return &fakeMessenger{
		answers: make(chan recordedAnswer, 10), messages: make(chan recordedMessage, 10), files: make(chan recordedFile, 10),
	}
}

func (m *fakeMessenger) SendMessage(_ context.Context, chatID, _ int64, body maxapi.NewMessageBody) error {
	m.messages <- recordedMessage{chatID: chatID, body: body}
	return nil
}

func (m *fakeMessenger) AnswerCallback(_ context.Context, callbackID string, chatID int64, req maxapi.SendAnswerRequest) error {
	m.answers <- recordedAnswer{callbackID: callbackID, chatID: chatID, req: req}
	return nil
}

func (m *fakeMessenger) SendFile(_ context.Context, chatID, _ int64, name string, data []byte, caption string) error {
	m.files <- recordedFile{chatID: chatID, name: name, data: append([]byte(nil), data...), caption: caption}
	return nil
}

type blockingAnalyzer struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	calls   int
	mu      sync.Mutex
	result  vacancies.Snapshot
	stats   vacancies.Statistics
	err     error
}

func (a *blockingAnalyzer) Analyze(ctx context.Context, _ vacancies.SearchRequest) (vacancies.Snapshot, vacancies.Statistics, error) {
	a.mu.Lock()
	a.calls++
	a.mu.Unlock()
	a.once.Do(func() { close(a.started) })
	select {
	case <-a.release:
		return a.result, a.stats, a.err
	case <-ctx.Done():
		return vacancies.Snapshot{}, vacancies.Statistics{}, ctx.Err()
	}
}

type fakeReportGenerator struct {
	document pdfreport.Document
	err      error
}

type cancelAwareAnalyzer struct {
	started  chan struct{}
	canceled chan struct{}
}

func (a *cancelAwareAnalyzer) Analyze(ctx context.Context, _ vacancies.SearchRequest) (vacancies.Snapshot, vacancies.Statistics, error) {
	close(a.started)
	<-ctx.Done()
	close(a.canceled)
	return vacancies.Snapshot{}, vacancies.Statistics{}, ctx.Err()
}

func (g fakeReportGenerator) Generate(_ context.Context, _ pdfreport.Input) (pdfreport.Document, error) {
	return g.document, g.err
}

func TestAnalyzeCallbackAcknowledgesImmediatelyAndSendsResults(t *testing.T) {
	cat := loadTestCatalog(t)
	messenger := newFakeMessenger()
	release := make(chan struct{})
	salary := int64(80_000)
	analyzer := &blockingAnalyzer{
		started: make(chan struct{}), release: release,
		result: vacancies.Snapshot{
			Vacancies: []vacancies.Vacancy{{ID: "v1", Title: "Программист", Employer: "Работодатель", SalaryFrom: &salary, Currency: "RUB"}},
			Source:    "Работа России", FetchedAt: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC), Mode: vacancies.ModeLive,
		},
		stats: vacancies.Statistics{VacancyCount: 1, EmployerCount: 1, SalaryCount: 1, SalaryCoveragePercent: 100, SalaryMedian: &salary, SalaryMin: &salary, SalaryMax: &salary, Currency: "RUB", EntryLevelPercent: 100, SmallSample: true},
	}
	reporter := fakeReportGenerator{document: pdfreport.Document{Filename: "report.pdf", MediaType: "application/pdf", Data: []byte("%PDF-test")}}
	h := NewHandler(messenger, cat, analyzer, reporter, AnalysisConfig{Workers: 1, Queue: 2, Timeout: 5 * time.Second})
	h.sessions.set(42, session{Step: stepReady, RegionCode: "7700000000000", RegionName: "Москва", ProgramCode: "09.02.07", Qualification: "Программист"})

	ctx, cancel := context.WithCancel(context.Background())
	h.StartAnalysisWorkers(ctx)
	t.Cleanup(func() {
		cancel()
		select {
		case <-release:
		default:
			close(release)
		}
		h.WaitAnalysisWorkers()
	})

	h.onCallback(context.Background(), analysisUpdate(42, "cb-1"))
	answer := receive(t, messenger.answers)
	if answer.req.Message == nil || answer.req.Message.Text == nil || !strings.Contains(*answer.req.Message.Text, "Запустил анализ") {
		t.Fatalf("unexpected callback answer: %#v", answer.req)
	}
	receive(t, analyzer.started)
	select {
	case <-messenger.files:
		t.Fatal("PDF was sent before analyzer completed")
	default:
	}

	close(release)
	message := receive(t, messenger.messages)
	if message.body.Text == nil || !strings.Contains(*message.body.Text, "медианный ориентир") {
		t.Fatalf("unexpected statistics message: %#v", message.body)
	}
	file := receive(t, messenger.files)
	if file.name != "report.pdf" || string(file.data) != "%PDF-test" {
		t.Fatalf("unexpected file: %#v", file)
	}
	waitFor(t, func() bool { return h.sessions.get(42).Step == stepReady })
}

func TestAnalyzeCallbackDoesNotQueueDuplicateJob(t *testing.T) {
	cat := loadTestCatalog(t)
	messenger := newFakeMessenger()
	release := make(chan struct{})
	analyzer := &blockingAnalyzer{started: make(chan struct{}), release: release, err: errors.New("source unavailable")}
	h := NewHandler(messenger, cat, analyzer, fakeReportGenerator{}, AnalysisConfig{Workers: 1, Queue: 2, Timeout: 5 * time.Second})
	h.sessions.set(7, session{Step: stepReady, RegionCode: "7700000000000", RegionName: "Москва", ProgramCode: "09.02.07", Qualification: "Программист"})

	ctx, cancel := context.WithCancel(context.Background())
	h.StartAnalysisWorkers(ctx)
	t.Cleanup(func() {
		cancel()
		select {
		case <-release:
		default:
			close(release)
		}
		h.WaitAnalysisWorkers()
	})

	h.onCallback(context.Background(), analysisUpdate(7, "first"))
	receive(t, messenger.answers)
	receive(t, analyzer.started)
	h.onCallback(context.Background(), analysisUpdate(7, "second"))
	second := receive(t, messenger.answers)
	if second.req.Message == nil || second.req.Message.Text == nil || !strings.Contains(*second.req.Message.Text, "уже готовится") {
		t.Fatalf("duplicate callback not rejected: %#v", second.req)
	}
	analyzer.mu.Lock()
	calls := analyzer.calls
	analyzer.mu.Unlock()
	if calls != 1 {
		t.Fatalf("Analyze calls = %d, want 1", calls)
	}
}

func TestAnalyzeButtonCanBeReusedAfterFinish(t *testing.T) {
	cat := loadTestCatalog(t)
	messenger := newFakeMessenger()
	release := make(chan struct{})
	close(release)
	analyzer := &blockingAnalyzer{
		started: make(chan struct{}),
		release: release,
		result: vacancies.Snapshot{
			Source: "Работа России", FetchedAt: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC), Mode: vacancies.ModeLive,
		},
	}
	h := NewHandler(messenger, cat, analyzer, fakeReportGenerator{}, AnalysisConfig{Workers: 1, Queue: 2, Timeout: time.Second})
	h.sessions.set(17, session{Step: stepReady, RegionCode: "7700000000000", RegionName: "Москва", ProgramCode: "09.02.07", Qualification: "Программист"})

	ctx, cancel := context.WithCancel(context.Background())
	h.StartAnalysisWorkers(ctx)
	t.Cleanup(func() {
		cancel()
		h.WaitAnalysisWorkers()
	})

	for _, callbackID := range []string{"first", "second"} {
		h.onCallback(context.Background(), analysisUpdate(17, callbackID))
		answer := receive(t, messenger.answers)
		if answer.req.Message == nil || answer.req.Message.Text == nil || !strings.Contains(*answer.req.Message.Text, "Запустил анализ") {
			t.Fatalf("analysis %s was not accepted: %#v", callbackID, answer.req)
		}
		message := receive(t, messenger.messages)
		if message.body.Text == nil || !strings.Contains(*message.body.Text, "не найдено") {
			t.Fatalf("unexpected analysis %s result: %#v", callbackID, message.body)
		}
		waitFor(t, func() bool { return h.sessions.get(17).Step == stepReady })
	}

	analyzer.mu.Lock()
	calls := analyzer.calls
	analyzer.mu.Unlock()
	if calls != 2 {
		t.Fatalf("Analyze calls = %d, want 2", calls)
	}

	h.sessions.reset(17)
	h.onCallback(context.Background(), analysisUpdate(17, "stale-after-reset"))
	answer := receive(t, messenger.answers)
	if answer.req.Message == nil || answer.req.Message.Text == nil || !strings.Contains(*answer.req.Message.Text, "старому выбору") {
		t.Fatalf("old button was not rejected after reset: %#v", answer.req)
	}
}

func TestRestartCancelsActiveAnalysis(t *testing.T) {
	cat := loadTestCatalog(t)
	messenger := newFakeMessenger()
	analyzer := &cancelAwareAnalyzer{started: make(chan struct{}), canceled: make(chan struct{})}
	h := NewHandler(messenger, cat, analyzer, fakeReportGenerator{}, AnalysisConfig{Workers: 1, Queue: 1, Timeout: time.Second})
	h.sessions.set(88, session{Step: stepReady, RegionCode: "7700000000000", RegionName: "Москва", ProgramCode: "09.02.07", Qualification: "Программист"})

	ctx, cancel := context.WithCancel(context.Background())
	h.StartAnalysisWorkers(ctx)
	t.Cleanup(func() {
		cancel()
		h.WaitAnalysisWorkers()
	})

	h.onCallback(context.Background(), analysisUpdate(88, "cancel"))
	receive(t, messenger.answers)
	receive(t, analyzer.started)
	h.beginDirectionContent(88)
	receive(t, analyzer.canceled)
	if got := h.sessions.get(88).Step; got != stepAwaitRegion {
		t.Fatalf("step = %q, want await_region", got)
	}
	select {
	case message := <-messenger.messages:
		t.Fatalf("canceled analysis sent a stale message: %#v", message)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestOldAnalyzeButtonCannotStartCurrentSelection(t *testing.T) {
	cat := loadTestCatalog(t)
	messenger := newFakeMessenger()
	analyzer := &blockingAnalyzer{started: make(chan struct{}), release: make(chan struct{})}
	h := NewHandler(messenger, cat, analyzer, fakeReportGenerator{}, AnalysisConfig{})
	h.sessions.set(99, session{
		Step: stepReady, Generation: 4, RegionCode: "7700000000000", RegionName: "Москва",
		ProgramCode: "09.02.07", Qualification: "Программист",
	})
	ctx, cancel := context.WithCancel(context.Background())
	h.StartAnalysisWorkers(ctx)
	t.Cleanup(func() {
		cancel()
		h.WaitAnalysisWorkers()
	})

	update := analysisUpdate(99, "stale")
	update.Callback.Payload = payloadAnalyze + "3:09.02.07"
	h.onCallback(context.Background(), update)
	answer := receive(t, messenger.answers)
	if answer.req.Message == nil || answer.req.Message.Text == nil || !strings.Contains(*answer.req.Message.Text, "старому выбору") {
		t.Fatalf("unexpected stale callback response: %#v", answer.req)
	}
	if got := h.sessions.get(99).Step; got != stepReady {
		t.Fatalf("stale callback changed step to %q", got)
	}
	select {
	case <-analyzer.started:
		t.Fatal("stale analyze callback started analyzer")
	default:
	}
}

func TestSessionResetDoesNotLetOldAnalysisOverwriteSelection(t *testing.T) {
	store := newSessionStore()
	store.set(1, session{Step: stepReady, RegionCode: "77", ProgramCode: "09.02.07"})
	selected, err := store.startAnalysis(1, 0, "09.02.07", func() {})
	if err != nil {
		t.Fatal(err)
	}
	store.reset(1)
	store.finishAnalysis(1, selected.Generation, selected.AnalysisID)
	if got := store.get(1).Step; got != stepAwaitRegion {
		t.Fatalf("step = %q, want %q", got, stepAwaitRegion)
	}
}

func TestSessionResetAtomicallyCancelsStartedAnalysis(t *testing.T) {
	store := newSessionStore()
	store.set(1, session{Step: stepReady, RegionCode: "77", ProgramCode: "09.02.07"})
	lifecycle, cancel := context.WithCancel(context.Background())
	defer cancel()

	selected, err := store.startAnalysis(1, 0, "09.02.07", cancel)
	if err != nil {
		t.Fatal(err)
	}
	store.reset(1)

	select {
	case <-lifecycle.Done():
	case <-time.After(time.Second):
		t.Fatal("reset did not cancel the analysis lifecycle")
	}
	if store.isCurrentAnalysis(1, selected.Generation, selected.AnalysisID) {
		t.Fatal("reset analysis is still current")
	}
}

func TestFinishedAnalysisCanBeRepeatedFromSameButton(t *testing.T) {
	store := newSessionStore()
	store.set(1, session{Step: stepReady, Generation: 7, RegionCode: "77", ProgramCode: "09.02.07"})

	first, err := store.startAnalysis(1, 7, "09.02.07", func() {})
	if err != nil {
		t.Fatal(err)
	}
	store.finishAnalysis(1, first.Generation, first.AnalysisID)
	second, err := store.startAnalysis(1, 7, "09.02.07", func() {})
	if err != nil {
		t.Fatalf("same generation button could not repeat analysis: %v", err)
	}
	if second.Generation != 7 {
		t.Fatalf("selection generation = %d, want 7", second.Generation)
	}
	if second.AnalysisID <= first.AnalysisID {
		t.Fatalf("analysis id did not advance: first=%d second=%d", first.AnalysisID, second.AnalysisID)
	}
}

func TestAnalysisTimeoutStillNotifiesUser(t *testing.T) {
	cat := loadTestCatalog(t)
	messenger := newFakeMessenger()
	analyzer := &blockingAnalyzer{started: make(chan struct{}), release: make(chan struct{})}
	h := NewHandler(messenger, cat, analyzer, fakeReportGenerator{}, AnalysisConfig{
		Workers: 1,
		Queue:   1,
		Timeout: 25 * time.Millisecond,
	})
	h.sessions.set(55, session{Step: stepReady, RegionCode: "7700000000000", RegionName: "Москва", ProgramCode: "09.02.07", Qualification: "Программист"})

	ctx, cancel := context.WithCancel(context.Background())
	h.StartAnalysisWorkers(ctx)
	t.Cleanup(func() {
		cancel()
		h.WaitAnalysisWorkers()
	})

	h.onCallback(context.Background(), analysisUpdate(55, "timeout"))
	receive(t, messenger.answers)
	receive(t, analyzer.started)
	message := receive(t, messenger.messages)
	if message.body.Text == nil || !strings.Contains(*message.body.Text, "Не удалось получить вакансии") {
		t.Fatalf("unexpected timeout message: %#v", message.body)
	}
	waitFor(t, func() bool { return h.sessions.get(55).Step == stepReady })
}

func TestOldAnalysisFailureIsNotSentAfterRestart(t *testing.T) {
	cat := loadTestCatalog(t)
	messenger := newFakeMessenger()
	release := make(chan struct{})
	analyzer := &blockingAnalyzer{
		started: make(chan struct{}), release: release, err: errors.New("source unavailable"),
	}
	h := NewHandler(messenger, cat, analyzer, fakeReportGenerator{}, AnalysisConfig{Workers: 1, Queue: 1, Timeout: time.Second})
	h.sessions.set(77, session{Step: stepReady, RegionCode: "7700000000000", RegionName: "Москва", ProgramCode: "09.02.07", Qualification: "Программист"})

	ctx, cancel := context.WithCancel(context.Background())
	h.StartAnalysisWorkers(ctx)
	t.Cleanup(func() {
		cancel()
		h.WaitAnalysisWorkers()
	})

	h.onCallback(context.Background(), analysisUpdate(77, "old"))
	receive(t, messenger.answers)
	receive(t, analyzer.started)
	h.sessions.reset(77)
	close(release)
	waitFor(t, func() bool { return h.sessions.get(77).Step == stepAwaitRegion })
	select {
	case message := <-messenger.messages:
		t.Fatalf("stale analysis sent a message after restart: %#v", message)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSessionUpdateIfRejectsStaleGeneration(t *testing.T) {
	store := newSessionStore()
	initial := store.reset(1)
	store.reset(1)
	if _, ok := store.updateIf(1, initial.Generation, stepAwaitRegion, func(sess *session) {
		sess.Step = stepReady
		sess.ProgramCode = "09.02.07"
	}); ok {
		t.Fatal("stale transition was accepted")
	}
	current := store.get(1)
	if current.Generation <= initial.Generation || current.Step != stepAwaitRegion || current.ProgramCode != "" {
		t.Fatalf("current session was overwritten: %+v", current)
	}
}

func TestQualificationPayloadIncludesProgramAndValidIndex(t *testing.T) {
	programCode, index, ok := parseQualificationPayload("09.02.07:2")
	if !ok || programCode != "09.02.07" || index != 2 {
		t.Fatalf("valid payload parsed as %q, %d, %v", programCode, index, ok)
	}
	for _, payload := range []string{"", "0", "09.02.07:no", "09.02.07:-1"} {
		if _, _, ok := parseQualificationPayload(payload); ok {
			t.Errorf("invalid payload %q was accepted", payload)
		}
	}
}

func TestAnalyzePayloadBindsGenerationAndProgram(t *testing.T) {
	generation, programCode, ok := parseAnalyzePayload("12:09.02.07")
	if !ok || generation != 12 || programCode != "09.02.07" {
		t.Fatalf("valid payload parsed as %d, %q, %v", generation, programCode, ok)
	}
	for _, payload := range []string{"", "12", "bad:09.02.07", "12:"} {
		if _, _, ok := parseAnalyzePayload(payload); ok {
			t.Errorf("invalid payload %q was accepted", payload)
		}
	}
}

func analysisUpdate(chatID int64, callbackID string) maxapi.Update {
	return maxapi.Update{
		UpdateType: maxapi.UpdateMessageCallback,
		ChatID:     chatID,
		Callback: &maxapi.Callback{
			CallbackID: callbackID,
			Payload:    payloadAnalyze + "0:09.02.07",
			User:       maxapi.User{UserID: 99},
		},
	}
}

func loadTestCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(file), "..", "..", "spo_program_vacancy_map.json")
	cat, err := catalog.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cat
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		var zero T
		t.Fatal("timed out waiting for channel")
		return zero
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition was not satisfied")
}
