package bot

import "sync"

type step string

const (
	stepIdle         step = ""
	stepAwaitRegion  step = "await_region"
	stepAwaitProgram step = "await_program"
	stepAwaitQual    step = "await_qual"
	stepReady        step = "ready"
)

// session хранит прогресс сценария «Выбор направления» для пользователя.
type session struct {
	Step           step
	RegionShort    string
	RegionCode     string
	RegionName     string
	ProgramCode    string
	Qualification  string
	SearchPrograms []string // коды программ из последнего поиска
	SearchRegions  []string // short-коды регионов из последнего поиска
}

type sessionStore struct {
	mu   sync.Mutex
	byID map[int64]*session
}

func newSessionStore() *sessionStore {
	return &sessionStore{byID: make(map[int64]*session)}
}

func (s *sessionStore) get(userID int64) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[userID]
	if !ok {
		sess = &session{}
		s.byID[userID] = sess
	}
	return sess
}

func (s *sessionStore) reset(userID int64) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := &session{Step: stepAwaitRegion}
	s.byID[userID] = sess
	return sess
}
