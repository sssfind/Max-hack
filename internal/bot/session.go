package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

type step string

const (
	stepIdle         step = ""
	stepAwaitRegion  step = "await_region"
	stepAwaitProgram step = "await_program"
	stepAwaitQual    step = "await_qual"
	stepReady        step = "ready"
	stepAnalyzing    step = "analyzing"
)

var (
	errSelectionNotReady = errors.New("selection is not ready")
	errAnalysisRunning   = errors.New("analysis is already running")
	errAnalysisDuplicate = errors.New("analysis callback was already handled")
	errStaleSelection    = errors.New("selection callback is stale")
)

// session хранит прогресс сценария «Выбор направления» для диалога (chat_id).
// Значения всегда копируются под mutex: изменяемые указатели наружу не выдаются.
type session struct {
	Step           step
	RegionShort    string
	RegionCode     string
	RegionName     string
	ProgramCode    string
	Qualification  string
	SearchPrograms []string // коды программ из последнего поиска
	ProgramQuery   string
	ProgramOffset  int
	SearchRegions  []string // short-коды регионов из последнего поиска
	Generation     uint64
	AnalysisID     uint64
	FeedbackFor    uint64
	// AnalysisInterrupted is persisted when an in-flight analysis is recovered
	// after a process restart. It keeps old callback buttons honest until the
	// user explicitly retries or changes the selection.
	AnalysisInterrupted    bool
	AnalysisDeliveryFailed bool
	LastAnalysisCallbackID string
	LastDelivery           *deliveryReceipt
	LastTransitionEventKey string
	TransitionBackup       []byte
	UpdatedAt              time.Time
	analysisCancel         context.CancelFunc
}

type deliveryReceipt struct {
	EventKey   string `json:"event_key"`
	Kind       string `json:"kind"`
	ChatID     int64  `json:"chat_id"`
	UserID     int64  `json:"user_id,omitempty"`
	CallbackID string `json:"callback_id,omitempty"`
	Purpose    string `json:"purpose,omitempty"`
	Payload    []byte `json:"payload"`
	Delivered  bool   `json:"delivered"`
	Durable    bool   `json:"durable"`
}

type selection struct {
	RegionCode    string
	RegionName    string
	ProgramCode   string
	Qualification string
	Generation    uint64
	AnalysisID    uint64
}

// sessionStore ключует сессии по chat_id: в message_created user_id часто пустой,
// а chat_id стабилен и для текста, и для callback.
type sessionStore struct {
	mu           sync.Mutex
	byChat       map[int64]session
	chatLocks    [256]sync.Mutex
	ttl          time.Duration
	lastPrune    time.Time
	repo         SessionRepository
	activeEvents map[int64]activeSessionEvent
}

type activeSessionEvent struct {
	key    string
	backup []byte
}

func newSessionStore() *sessionStore {
	return newSessionStoreWithTTL(24 * time.Hour)
}

func newSessionStoreWithTTL(ttl time.Duration) *sessionStore {
	return newSessionStoreWithRepository(nil, ttl)
}

func newSessionStoreWithRepository(repo SessionRepository, ttl time.Duration) *sessionStore {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &sessionStore{
		byChat: make(map[int64]session), ttl: ttl, lastPrune: time.Now(), repo: repo,
		activeEvents: make(map[int64]activeSessionEvent),
	}
}

// lockChat serializes state transitions for one conversation while leaving
// other chats independent. The store-wide mutex below only protects small
// in-memory map operations and is never held across repository calls.
func (s *sessionStore) lockChat(chatID int64) func() {
	chatLock := &s.chatLocks[uint64(chatID)%uint64(len(s.chatLocks))]
	chatLock.Lock()
	return chatLock.Unlock
}

func (s *sessionStore) cached(chatID int64) (session, bool) {
	s.mu.Lock()
	sess, ok := s.byChat[chatID]
	s.mu.Unlock()
	return cloneSession(sess), ok
}

func (s *sessionStore) putCached(chatID int64, sess session) {
	s.mu.Lock()
	s.byChat[chatID] = cloneSession(sess)
	s.mu.Unlock()
}

func (s *sessionStore) deleteCached(chatID int64) {
	s.mu.Lock()
	delete(s.byChat, chatID)
	s.mu.Unlock()
}

// ensureLoaded is the fail-closed entry point for webhook handling. A
// transient repository failure must abort the event instead of being treated
// as an empty session and overwriting a user's durable selection.
func (s *sessionStore) ensureLoaded(chatID int64) error {
	unlock := s.lockChat(chatID)
	defer unlock()
	if _, ok := s.cached(chatID); ok {
		return nil
	}
	_, ok, err := s.load(chatID)
	if err != nil {
		return err
	}
	if !ok {
		s.putCached(chatID, session{})
	}
	return nil
}

func (s *sessionStore) get(chatID int64) session {
	unlock := s.lockChat(chatID)
	defer unlock()
	sess, ok := s.cached(chatID)
	if !ok {
		var err error
		sess, ok, err = s.load(chatID)
		if err != nil {
			slog.Warn("Failed to restore bot session", "error", err)
			return session{}
		}
		if !ok {
			return session{}
		}
	}
	if s.expired(sess, time.Now()) {
		if sess.analysisCancel != nil {
			sess.analysisCancel()
		}
		s.deleteCached(chatID)
		if err := s.deletePersisted(chatID); err != nil {
			slog.Warn("Failed to delete expired persisted session", "error", err)
		}
		return session{}
	}
	return cloneSession(sess)
}

func (s *sessionStore) set(chatID int64, sess session) session {
	now := time.Now()
	s.prune(now, false)
	unlock := s.lockChat(chatID)
	defer unlock()
	if current, ok := s.cached(chatID); ok && current.Generation > sess.Generation {
		sess.Generation = current.Generation
	}
	sess.UpdatedAt = now
	sess = cloneSession(sess)
	s.putCached(chatID, sess)
	s.persist(chatID, sess)
	return cloneSession(sess)
}

// updateIf atomically applies a state transition only to the session snapshot
// that produced the current message or keyboard. This prevents late webhook
// workers and stale buttons from overwriting a newer user choice.
func (s *sessionStore) updateIf(chatID int64, generation uint64, expectedStep step, update func(*session)) (session, bool) {
	unlock := s.lockChat(chatID)
	defer unlock()
	sess, ok := s.cached(chatID)
	if !ok {
		var err error
		sess, ok, err = s.load(chatID)
		if err != nil {
			slog.Warn("Failed to restore bot session for transition", "error", err)
			return session{}, false
		}
	}
	if !ok || sess.Generation != generation || sess.Step != expectedStep {
		return cloneSession(sess), false
	}
	sess = cloneSession(sess)
	update(&sess)
	sess.UpdatedAt = time.Now()
	s.putCached(chatID, sess)
	s.persist(chatID, sess)
	return cloneSession(sess), true
}

func (s *sessionStore) reset(chatID int64) session {
	unlock := s.lockChat(chatID)
	defer unlock()
	current, ok := s.cached(chatID)
	if !ok {
		var err error
		current, _, err = s.load(chatID)
		if err != nil {
			slog.Warn("Failed to restore bot session for reset", "error", err)
			return session{}
		}
	}
	if current.analysisCancel != nil {
		current.analysisCancel()
	}
	sess := session{
		Step:        stepAwaitRegion,
		Generation:  current.Generation + 1,
		AnalysisID:  current.AnalysisID,
		FeedbackFor: current.AnalysisID,
		UpdatedAt:   time.Now(),
	}
	s.putCached(chatID, sess)
	s.persist(chatID, sess)
	return sess
}

func (s *sessionStore) startAnalysis(
	chatID int64,
	expectedGeneration uint64,
	expectedProgramCode string,
	callbackID string,
	cancel context.CancelFunc,
) (selection, error) {
	unlock := s.lockChat(chatID)
	defer unlock()
	sess, ok := s.cached(chatID)
	if !ok {
		var err error
		sess, _, err = s.load(chatID)
		if err != nil {
			return selection{}, fmt.Errorf("restore session before analysis: %w", err)
		}
	}
	if callbackID != "" && sess.LastAnalysisCallbackID == callbackID {
		return selection{}, errAnalysisDuplicate
	}
	if sess.Step == stepAnalyzing {
		return selection{}, errAnalysisRunning
	}
	if sess.Generation != expectedGeneration || sess.ProgramCode != expectedProgramCode {
		return selection{}, errStaleSelection
	}
	if sess.Step != stepReady || sess.RegionCode == "" || sess.ProgramCode == "" {
		return selection{}, errSelectionNotReady
	}
	sess.Step = stepAnalyzing
	sess.AnalysisID++
	sess.AnalysisInterrupted = false
	sess.AnalysisDeliveryFailed = false
	sess.LastAnalysisCallbackID = callbackID
	sess.UpdatedAt = time.Now()
	sess.analysisCancel = cancel
	s.putCached(chatID, sess)
	s.persist(chatID, sess)
	return selection{
		RegionCode:    sess.RegionCode,
		RegionName:    sess.RegionName,
		ProgramCode:   sess.ProgramCode,
		Qualification: sess.Qualification,
		Generation:    sess.Generation,
		AnalysisID:    sess.AnalysisID,
	}, nil
}

func (s *sessionStore) finishAnalysis(chatID int64, generation, analysisID uint64) {
	unlock := s.lockChat(chatID)
	defer unlock()
	sess, ok := s.cached(chatID)
	if !ok || sess.Generation != generation || sess.AnalysisID != analysisID || sess.Step != stepAnalyzing {
		return
	}
	sess.Step = stepReady
	sess.AnalysisInterrupted = false
	sess.AnalysisDeliveryFailed = false
	sess.analysisCancel = nil
	settleAnalysisStartReceipt(&sess)
	sess.UpdatedAt = time.Now()
	s.putCached(chatID, sess)
	if err := s.saveWithRetry(chatID, sess, 3); err != nil {
		slog.Error("Failed to persist completed analysis session",
			"error", err, "generation", generation, "analysis_id", analysisID)
	}
}

func (s *sessionStore) markAnalysisDeliveryFailed(chatID int64, generation, analysisID uint64) {
	unlock := s.lockChat(chatID)
	defer unlock()
	sess, ok := s.cached(chatID)
	if !ok || sess.Generation != generation || sess.AnalysisID != analysisID || sess.Step != stepAnalyzing {
		return
	}
	sess.Step = stepReady
	sess.AnalysisInterrupted = false
	sess.AnalysisDeliveryFailed = true
	sess.analysisCancel = nil
	settleAnalysisStartReceipt(&sess)
	sess.UpdatedAt = time.Now()
	s.putCached(chatID, sess)
	if err := s.saveWithRetry(chatID, sess, 3); err != nil {
		slog.Error("Failed to persist analysis delivery failure",
			"error", err, "generation", generation, "analysis_id", analysisID)
	}
}

func settleAnalysisStartReceipt(sess *session) {
	if sess == nil || sess.LastDelivery == nil || sess.LastDelivery.Purpose != "analysis_start" || sess.LastDelivery.Delivered {
		return
	}
	sess.LastDelivery.Delivered = true
	sess.LastDelivery.Durable = true
	sess.LastDelivery.Kind = ""
	sess.LastDelivery.ChatID = 0
	sess.LastDelivery.UserID = 0
	sess.LastDelivery.CallbackID = ""
	sess.LastDelivery.Purpose = ""
	sess.LastDelivery.Payload = nil
}

// cancelAnalysis cancels only the active analysis and keeps the selected
// region/program so the user can immediately retry or change one choice.
func (s *sessionStore) cancelAnalysis(chatID int64, generation, analysisID uint64) (session, bool) {
	unlock := s.lockChat(chatID)
	defer unlock()
	sess, ok := s.cached(chatID)
	if !ok {
		var err error
		sess, ok, err = s.load(chatID)
		if err != nil {
			slog.Warn("Failed to restore bot session for analysis cancellation", "error", err)
			return session{}, false
		}
	}
	if !ok || sess.Step != stepAnalyzing || sess.Generation != generation || sess.AnalysisID != analysisID {
		return cloneSession(sess), false
	}
	if sess.analysisCancel != nil {
		sess.analysisCancel()
	}
	sess.analysisCancel = nil
	sess.Step = stepReady
	sess.AnalysisInterrupted = false
	sess.AnalysisDeliveryFailed = false
	sess.UpdatedAt = time.Now()
	s.putCached(chatID, sess)
	s.persist(chatID, sess)
	return cloneSession(sess), true
}

// reselectProgram preserves the region, invalidates stale callbacks and
// returns the session to program search.
func (s *sessionStore) reselectProgram(chatID int64) (session, bool) {
	unlock := s.lockChat(chatID)
	defer unlock()
	sess, ok := s.cached(chatID)
	if !ok {
		var err error
		sess, ok, err = s.load(chatID)
		if err != nil {
			slog.Warn("Failed to restore bot session for program selection", "error", err)
			return session{}, false
		}
	}
	if !ok || sess.RegionCode == "" {
		return cloneSession(sess), false
	}
	if sess.analysisCancel != nil {
		sess.analysisCancel()
	}
	sess.Generation++
	sess.ProgramCode = ""
	sess.Qualification = ""
	sess.SearchPrograms = nil
	sess.ProgramQuery = ""
	sess.ProgramOffset = 0
	sess.AnalysisInterrupted = false
	sess.AnalysisDeliveryFailed = false
	sess.analysisCancel = nil
	sess.Step = stepAwaitProgram
	sess.UpdatedAt = time.Now()
	s.putCached(chatID, sess)
	s.persist(chatID, sess)
	return cloneSession(sess), true
}

func (s *sessionStore) recordFeedback(chatID int64, analysisID uint64) bool {
	unlock := s.lockChat(chatID)
	defer unlock()
	sess, ok := s.cached(chatID)
	if !ok {
		var err error
		sess, ok, err = s.load(chatID)
		if err != nil {
			slog.Warn("Failed to restore bot session for feedback", "error", err)
			return false
		}
	}
	if !ok || analysisID == 0 || sess.AnalysisID != analysisID || sess.FeedbackFor == analysisID {
		return false
	}
	sess.FeedbackFor = analysisID
	sess.UpdatedAt = time.Now()
	s.putCached(chatID, sess)
	s.persist(chatID, sess)
	return true
}

func (s *sessionStore) isCurrentAnalysis(chatID int64, generation, analysisID uint64) bool {
	unlock := s.lockChat(chatID)
	defer unlock()
	sess, ok := s.cached(chatID)
	return ok && sess.Generation == generation && sess.AnalysisID == analysisID && sess.Step == stepAnalyzing
}

// isSameAnalysisAttempt remains true after a fast job has already moved back
// to ready. It is used only to acknowledge the callback that queued that job;
// output suppression still requires isCurrentAnalysis.
func (s *sessionStore) isSameAnalysisAttempt(chatID int64, generation, analysisID uint64) bool {
	unlock := s.lockChat(chatID)
	defer unlock()
	sess, ok := s.cached(chatID)
	return ok && sess.Generation == generation && sess.AnalysisID == analysisID
}

func cloneSession(sess session) session {
	sess.SearchPrograms = append([]string(nil), sess.SearchPrograms...)
	sess.SearchRegions = append([]string(nil), sess.SearchRegions...)
	sess.TransitionBackup = append([]byte(nil), sess.TransitionBackup...)
	if sess.LastDelivery != nil {
		receipt := *sess.LastDelivery
		receipt.Payload = append([]byte(nil), receipt.Payload...)
		sess.LastDelivery = &receipt
	}
	return sess
}

func (s *sessionStore) beginEvent(chatID int64, eventKey string) error {
	unlock := s.lockChat(chatID)
	defer unlock()
	sess, ok := s.cached(chatID)
	if !ok {
		var err error
		sess, _, err = s.load(chatID)
		if err != nil {
			return fmt.Errorf("restore session before event: %w", err)
		}
	}
	backup := cloneSession(sess)
	backup.LastTransitionEventKey = ""
	backup.TransitionBackup = nil
	raw, err := json.Marshal(persistedFromRuntime(backup))
	if err != nil {
		return fmt.Errorf("encode session transition backup: %w", err)
	}
	s.mu.Lock()
	s.activeEvents[chatID] = activeSessionEvent{key: eventKey, backup: raw}
	s.mu.Unlock()
	return nil
}

func (s *sessionStore) endEvent(chatID int64, eventKey string) {
	unlock := s.lockChat(chatID)
	defer unlock()
	s.mu.Lock()
	if active, ok := s.activeEvents[chatID]; ok && active.key == eventKey {
		delete(s.activeEvents, chatID)
	}
	s.mu.Unlock()
}

func (s *sessionStore) rollbackIncompleteTransition(chatID int64, eventKey string) (bool, error) {
	unlock := s.lockChat(chatID)
	defer unlock()
	sess, ok := s.cached(chatID)
	if !ok {
		var err error
		sess, ok, err = s.load(chatID)
		if err != nil {
			return false, fmt.Errorf("restore session before rollback: %w", err)
		}
	}
	if !ok || sess.LastTransitionEventKey != eventKey || len(sess.TransitionBackup) == 0 {
		return false, nil
	}
	if sess.AnalysisInterrupted {
		// A started analysis cannot be recreated safely after restart. Keep the
		// recovered ready/interrupted state and let the duplicate callback
		// explain that the user must launch a fresh attempt.
		sess.LastTransitionEventKey = ""
		sess.TransitionBackup = nil
		sess.UpdatedAt = time.Now()
		s.putCached(chatID, sess)
		if err := s.save(chatID, sess); err != nil {
			return false, fmt.Errorf("clear interrupted transition marker: %w", err)
		}
		return false, nil
	}
	var stored persistedSession
	if err := json.Unmarshal(sess.TransitionBackup, &stored); err != nil {
		return false, fmt.Errorf("decode session transition backup: %w", err)
	}
	restored := stored.runtime()
	// The backup captures only the webhook-owned transition. Preserve a
	// concurrently completed analysis for the same generation/attempt instead
	// of resurrecting an in-memory job that no longer exists. When the job is
	// still running in this process, keep its runtime cancellation function.
	if restored.Step == stepAnalyzing && restored.Generation == sess.Generation && restored.AnalysisID == sess.AnalysisID {
		switch sess.Step {
		case stepReady:
			restored.Step = stepReady
			restored.AnalysisInterrupted = sess.AnalysisInterrupted
			restored.AnalysisDeliveryFailed = sess.AnalysisDeliveryFailed
		case stepAnalyzing:
			restored.analysisCancel = sess.analysisCancel
		}
	}
	restored.LastTransitionEventKey = ""
	restored.TransitionBackup = nil
	restored.UpdatedAt = time.Now()
	s.putCached(chatID, restored)
	if err := s.save(chatID, restored); err != nil {
		return false, fmt.Errorf("restore incomplete session transition: %w", err)
	}
	return true, nil
}

func (s *sessionStore) delivery(chatID int64, eventKey string) (deliveryReceipt, bool, error) {
	unlock := s.lockChat(chatID)
	defer unlock()
	sess, ok := s.cached(chatID)
	if !ok {
		var err error
		sess, ok, err = s.load(chatID)
		if err != nil {
			return deliveryReceipt{}, false, fmt.Errorf("restore session before delivery replay: %w", err)
		}
	}
	if !ok || sess.LastDelivery == nil || sess.LastDelivery.EventKey != eventKey {
		return deliveryReceipt{}, false, nil
	}
	receipt := *sess.LastDelivery
	receipt.Payload = append([]byte(nil), receipt.Payload...)
	return receipt, true, nil
}

func (s *sessionStore) recordDelivery(chatID int64, receipt deliveryReceipt) error {
	unlock := s.lockChat(chatID)
	defer unlock()
	sess, ok := s.cached(chatID)
	if !ok {
		var err error
		sess, _, err = s.load(chatID)
		if err != nil {
			return fmt.Errorf("restore session before recording delivery: %w", err)
		}
	}
	receipt.Payload = append([]byte(nil), receipt.Payload...)
	receipt.Durable = true
	transitionEventKey := sess.LastTransitionEventKey
	transitionBackup := append([]byte(nil), sess.TransitionBackup...)
	sess.LastDelivery = &receipt
	sess.LastTransitionEventKey = ""
	sess.TransitionBackup = nil
	sess.UpdatedAt = time.Now()
	if err := s.save(chatID, sess); err != nil {
		// Nothing was sent yet and the response plan was not durable. Keep only
		// the transition marker so a retry can roll back and recompute safely;
		// retaining an in-memory receipt here could later be persisted without
		// the analysis job it describes.
		sess.LastDelivery = nil
		sess.LastTransitionEventKey = transitionEventKey
		sess.TransitionBackup = transitionBackup
		s.putCached(chatID, sess)
		return fmt.Errorf("persist outbound delivery receipt: %w", err)
	}
	s.putCached(chatID, sess)
	return nil
}

func (s *sessionStore) ensureDeliveryDurable(chatID int64, eventKey string) error {
	unlock := s.lockChat(chatID)
	defer unlock()
	sess, ok := s.cached(chatID)
	if !ok {
		var err error
		sess, ok, err = s.load(chatID)
		if err != nil {
			return fmt.Errorf("restore session before persisting delivery: %w", err)
		}
	}
	if !ok || sess.LastDelivery == nil || sess.LastDelivery.EventKey != eventKey {
		return errors.New("outbound delivery receipt is missing")
	}
	if sess.LastDelivery.Durable {
		return nil
	}
	sess.LastDelivery.Durable = true
	sess.UpdatedAt = time.Now()
	if err := s.save(chatID, sess); err != nil {
		sess.LastDelivery.Durable = false
		s.putCached(chatID, sess)
		return fmt.Errorf("persist outbound delivery receipt: %w", err)
	}
	s.putCached(chatID, sess)
	return nil
}

func (s *sessionStore) markDeliveryCompleted(chatID int64, eventKey string) error {
	unlock := s.lockChat(chatID)
	defer unlock()
	sess, ok := s.cached(chatID)
	if !ok || sess.LastDelivery == nil || sess.LastDelivery.EventKey != eventKey {
		return errors.New("outbound delivery receipt is missing")
	}
	sess.LastDelivery.Delivered = true
	sess.LastDelivery.Durable = true
	// The event key is enough to suppress a completed replay. Drop message
	// contents and addressing data immediately after successful delivery.
	sess.LastDelivery.Kind = ""
	sess.LastDelivery.ChatID = 0
	sess.LastDelivery.UserID = 0
	sess.LastDelivery.CallbackID = ""
	sess.LastDelivery.Purpose = ""
	sess.LastDelivery.Payload = nil
	sess.UpdatedAt = time.Now()
	s.putCached(chatID, sess)
	if err := s.saveWithRetry(chatID, sess, 3); err != nil {
		return fmt.Errorf("mark outbound delivery complete: %w", err)
	}
	return nil
}

func (s *sessionStore) forget(chatID int64) error {
	unlock := s.lockChat(chatID)
	defer unlock()
	if sess, ok := s.cached(chatID); ok && sess.analysisCancel != nil {
		sess.analysisCancel()
	}
	s.deleteCached(chatID)
	return s.deletePersisted(chatID)
}

// load is called with the per-chat lock held. Repository I/O intentionally
// happens without the store-wide map mutex so a slow database call for one
// chat cannot stall unrelated conversations.
func (s *sessionStore) load(chatID int64) (session, bool, error) {
	if s.repo == nil {
		return session{}, false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	sess, ok, err := s.repo.Load(ctx, chatID)
	if err != nil {
		return session{}, false, fmt.Errorf("load bot session: %w", err)
	}
	if !ok || s.expired(sess, time.Now()) {
		return session{}, false, nil
	}
	changed := false
	// Analysis jobs are process-local. A pending start acknowledgement cannot
	// be replayed after restart because there is no corresponding durable job.
	if sess.LastDelivery != nil && sess.LastDelivery.Purpose == "analysis_start" && !sess.LastDelivery.Delivered {
		sess.LastDelivery = nil
		changed = true
	}
	if sess.Step == stepAnalyzing {
		sess.Step = stepReady
		sess.AnalysisInterrupted = true
		sess.AnalysisDeliveryFailed = false
		sess.analysisCancel = nil
		changed = true
	}
	if changed {
		sess.UpdatedAt = time.Now()
		sess = s.prepareForPersist(chatID, sess)
		s.putCached(chatID, sess)
		if err := s.save(chatID, sess); err != nil {
			s.deleteCached(chatID)
			return session{}, false, fmt.Errorf("persist recovered bot session: %w", err)
		}
	} else {
		s.putCached(chatID, sess)
	}
	return sess, true, nil
}

func (s *sessionStore) prepareForPersist(chatID int64, sess session) session {
	s.mu.Lock()
	active, ok := s.activeEvents[chatID]
	s.mu.Unlock()
	if ok && sess.LastTransitionEventKey != active.key {
		sess.LastTransitionEventKey = active.key
		sess.TransitionBackup = append([]byte(nil), active.backup...)
	}
	return sess
}

func (s *sessionStore) persist(chatID int64, sess session) {
	sess = s.prepareForPersist(chatID, sess)
	s.putCached(chatID, sess)
	if err := s.save(chatID, sess); err != nil {
		slog.Warn("Failed to persist bot session", "error", err)
	}
}

func (s *sessionStore) save(chatID int64, sess session) error {
	if s.repo == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if err := s.repo.Save(ctx, chatID, cloneSession(sess), sess.UpdatedAt.Add(s.ttl)); err != nil {
		return err
	}
	return nil
}

func (s *sessionStore) saveWithRetry(chatID int64, sess session, attempts int) error {
	if attempts < 1 {
		attempts = 1
	}
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err = s.save(chatID, sess); err == nil {
			return nil
		}
		if attempt < attempts {
			time.Sleep(time.Duration(attempt*25) * time.Millisecond)
		}
	}
	return fmt.Errorf("save bot session after %d attempts: %w", attempts, err)
}

func (s *sessionStore) deletePersisted(chatID int64) error {
	if s.repo == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if err := s.repo.Delete(ctx, chatID); err != nil {
		return fmt.Errorf("delete bot session: %w", err)
	}
	return nil
}

func (s *sessionStore) expired(sess session, now time.Time) bool {
	return !sess.UpdatedAt.IsZero() && now.Sub(sess.UpdatedAt) > s.ttl
}

func (s *sessionStore) prune(now time.Time, force bool) {
	s.mu.Lock()
	if !force && now.Sub(s.lastPrune) < 10*time.Minute {
		s.mu.Unlock()
		return
	}
	// Reserve this cleanup interval before doing any slow work. Other chats can
	// continue and will not start duplicate repository cleanup calls.
	s.lastPrune = now
	chatIDs := make([]int64, 0, len(s.byChat))
	for chatID := range s.byChat {
		chatIDs = append(chatIDs, chatID)
	}
	s.mu.Unlock()

	for _, chatID := range chatIDs {
		unlock := s.lockChat(chatID)
		sess, ok := s.cached(chatID)
		if ok && s.expired(sess, now) {
			if sess.analysisCancel != nil {
				sess.analysisCancel()
			}
			s.deleteCached(chatID)
		}
		unlock()
	}
	if s.repo != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		if _, err := s.repo.Cleanup(ctx, now); err != nil {
			slog.Warn("Failed to clean expired bot sessions", "error", err)
		}
		cancel()
	}
}

func (s *sessionStore) cleanupExpired() {
	// Force the same bounded cleanup used by writes, even if the bot is idle.
	s.prune(time.Now(), true)
}
