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

// session хранит прогресс сценария «Выбор направления» для диалога (chat_id).
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

// sessionStore ключует сессии по chat_id: в message_created user_id часто пустой,
// а chat_id стабилен и для текста, и для callback.
type sessionStore struct {
	mu     sync.Mutex
	byChat map[int64]*session
}

func newSessionStore() *sessionStore {
	return &sessionStore{byChat: make(map[int64]*session)}
}

func (s *sessionStore) get(chatID int64) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byChat[chatID]
	if !ok {
		sess = &session{}
		s.byChat[chatID] = sess
	}
	return sess
}

func (s *sessionStore) reset(chatID int64) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := &session{Step: stepAwaitRegion}
	s.byChat[chatID] = sess
	return sess
}
