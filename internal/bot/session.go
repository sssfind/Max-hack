package bot

import (
	"context"
	"errors"
	"sync"
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
	SearchRegions  []string // short-коды регионов из последнего поиска
	Generation     uint64
	AnalysisID     uint64
	analysisCancel context.CancelFunc
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
	mu     sync.Mutex
	byChat map[int64]session
}

func newSessionStore() *sessionStore {
	return &sessionStore{byChat: make(map[int64]session)}
}

func (s *sessionStore) get(chatID int64) session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneSession(s.byChat[chatID])
}

func (s *sessionStore) set(chatID int64, sess session) session {
	s.mu.Lock()
	defer s.mu.Unlock()
	if current := s.byChat[chatID]; current.Generation > sess.Generation {
		sess.Generation = current.Generation
	}
	sess = cloneSession(sess)
	s.byChat[chatID] = sess
	return cloneSession(sess)
}

func (s *sessionStore) update(chatID int64, update func(*session)) session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := cloneSession(s.byChat[chatID])
	update(&sess)
	s.byChat[chatID] = cloneSession(sess)
	return cloneSession(sess)
}

// updateIf atomically applies a state transition only to the session snapshot
// that produced the current message or keyboard. This prevents late webhook
// workers and stale buttons from overwriting a newer user choice.
func (s *sessionStore) updateIf(chatID int64, generation uint64, expectedStep step, update func(*session)) (session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byChat[chatID]
	if !ok || sess.Generation != generation || sess.Step != expectedStep {
		return cloneSession(sess), false
	}
	sess = cloneSession(sess)
	update(&sess)
	s.byChat[chatID] = cloneSession(sess)
	return cloneSession(sess), true
}

func (s *sessionStore) reset(chatID int64) session {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.byChat[chatID]
	if current.analysisCancel != nil {
		current.analysisCancel()
	}
	sess := session{
		Step:       stepAwaitRegion,
		Generation: current.Generation + 1,
		AnalysisID: current.AnalysisID,
	}
	s.byChat[chatID] = sess
	return sess
}

func (s *sessionStore) startAnalysis(
	chatID int64,
	expectedGeneration uint64,
	expectedProgramCode string,
	cancel context.CancelFunc,
) (selection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.byChat[chatID]
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
	sess.analysisCancel = cancel
	s.byChat[chatID] = cloneSession(sess)
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
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byChat[chatID]
	if !ok || sess.Generation != generation || sess.AnalysisID != analysisID || sess.Step != stepAnalyzing {
		return
	}
	sess.Step = stepReady
	sess.analysisCancel = nil
	s.byChat[chatID] = cloneSession(sess)
}

func (s *sessionStore) isCurrentAnalysis(chatID int64, generation, analysisID uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byChat[chatID]
	return ok && sess.Generation == generation && sess.AnalysisID == analysisID && sess.Step == stepAnalyzing
}

// isSameAnalysisAttempt remains true after a fast job has already moved back
// to ready. It is used only to acknowledge the callback that queued that job;
// output suppression still requires isCurrentAnalysis.
func (s *sessionStore) isSameAnalysisAttempt(chatID int64, generation, analysisID uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byChat[chatID]
	return ok && sess.Generation == generation && sess.AnalysisID == analysisID
}

func cloneSession(sess session) session {
	sess.SearchPrograms = append([]string(nil), sess.SearchPrograms...)
	sess.SearchRegions = append([]string(nil), sess.SearchRegions...)
	return sess
}
