package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SessionRepository persists navigation state and a short-lived outbound
// delivery receipt. Runtime-only cancellation functions are never stored;
// receipt contents and addressing data are cleared immediately after delivery.
type SessionRepository interface {
	Ready(context.Context) error
	Load(context.Context, int64) (session, bool, error)
	Save(context.Context, int64, session, time.Time) error
	Delete(context.Context, int64) error
	Cleanup(context.Context, time.Time) (int64, error)
}

func (r *postgresSessionRepository) Ready(ctx context.Context) error {
	if r == nil || r.db == nil {
		return errors.New("session repository is not configured")
	}
	if _, err := r.db.Exec(ctx, `SELECT chat_id, state, updated_at, expires_at FROM bot_sessions LIMIT 0`); err != nil {
		return fmt.Errorf("bot session storage is not ready: %w", err)
	}
	var hasExpiryIndex bool
	if err := r.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM pg_indexes
			WHERE schemaname = ANY (current_schemas(false))
			  AND tablename = 'bot_sessions'
			  AND indexdef ILIKE '%(expires_at)%'
		)
	`).Scan(&hasExpiryIndex); err != nil {
		return fmt.Errorf("check bot session expiry index: %w", err)
	}
	if !hasExpiryIndex {
		return errors.New("bot session storage is not ready: expires_at index is missing")
	}
	return nil
}

type postgresSessionRepository struct {
	db *pgxpool.Pool
}

func NewPostgresSessionRepository(db *pgxpool.Pool) SessionRepository {
	return &postgresSessionRepository{db: db}
}

type persistedSession struct {
	Step                   step             `json:"step"`
	RegionShort            string           `json:"region_short,omitempty"`
	RegionCode             string           `json:"region_code,omitempty"`
	RegionName             string           `json:"region_name,omitempty"`
	ProgramCode            string           `json:"program_code,omitempty"`
	Qualification          string           `json:"qualification,omitempty"`
	SearchPrograms         []string         `json:"search_programs,omitempty"`
	ProgramQuery           string           `json:"program_query,omitempty"`
	ProgramOffset          int              `json:"program_offset,omitempty"`
	SearchRegions          []string         `json:"search_regions,omitempty"`
	Generation             uint64           `json:"generation"`
	AnalysisID             uint64           `json:"analysis_id"`
	FeedbackFor            uint64           `json:"feedback_for,omitempty"`
	AnalysisInterrupted    bool             `json:"analysis_interrupted,omitempty"`
	AnalysisDeliveryFailed bool             `json:"analysis_delivery_failed,omitempty"`
	LastAnalysisCallbackID string           `json:"last_analysis_callback_id,omitempty"`
	LastDelivery           *deliveryReceipt `json:"last_delivery,omitempty"`
	LastTransitionEventKey string           `json:"last_transition_event_key,omitempty"`
	TransitionBackup       []byte           `json:"transition_backup,omitempty"`
	UpdatedAt              time.Time        `json:"updated_at"`
}

func (r *postgresSessionRepository) Load(ctx context.Context, chatID int64) (session, bool, error) {
	if r == nil || r.db == nil {
		return session{}, false, errors.New("session repository is not configured")
	}
	var raw []byte
	err := r.db.QueryRow(ctx, `
		SELECT state
		FROM bot_sessions
		WHERE chat_id = $1 AND expires_at > NOW()
	`, chatID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return session{}, false, nil
	}
	if err != nil {
		return session{}, false, fmt.Errorf("load bot session: %w", err)
	}
	var stored persistedSession
	if err := json.Unmarshal(raw, &stored); err != nil {
		return session{}, false, fmt.Errorf("decode bot session: %w", err)
	}
	return stored.runtime(), true, nil
}

func (r *postgresSessionRepository) Save(ctx context.Context, chatID int64, sess session, expiresAt time.Time) error {
	if r == nil || r.db == nil {
		return errors.New("session repository is not configured")
	}
	raw, err := json.Marshal(persistedFromRuntime(sess))
	if err != nil {
		return fmt.Errorf("encode bot session: %w", err)
	}
	_, err = r.db.Exec(ctx, `
		INSERT INTO bot_sessions (chat_id, state, updated_at, expires_at)
		VALUES ($1, $2::jsonb, $3, $4)
		ON CONFLICT (chat_id) DO UPDATE
		SET state = EXCLUDED.state,
			updated_at = EXCLUDED.updated_at,
			expires_at = EXCLUDED.expires_at
		WHERE bot_sessions.updated_at <= EXCLUDED.updated_at
	`, chatID, string(raw), sess.UpdatedAt, expiresAt)
	if err != nil {
		return fmt.Errorf("save bot session: %w", err)
	}
	return nil
}

func (r *postgresSessionRepository) Delete(ctx context.Context, chatID int64) error {
	if r == nil || r.db == nil {
		return errors.New("session repository is not configured")
	}
	if _, err := r.db.Exec(ctx, `DELETE FROM bot_sessions WHERE chat_id = $1`, chatID); err != nil {
		return fmt.Errorf("delete bot session: %w", err)
	}
	return nil
}

func (r *postgresSessionRepository) Cleanup(ctx context.Context, expiredBefore time.Time) (int64, error) {
	if r == nil || r.db == nil {
		return 0, errors.New("session repository is not configured")
	}
	tag, err := r.db.Exec(ctx, `DELETE FROM bot_sessions WHERE expires_at < $1`, expiredBefore)
	if err != nil {
		return 0, fmt.Errorf("cleanup bot sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}

func persistedFromRuntime(sess session) persistedSession {
	return persistedSession{
		Step: sess.Step, RegionShort: sess.RegionShort, RegionCode: sess.RegionCode,
		RegionName: sess.RegionName, ProgramCode: sess.ProgramCode, Qualification: sess.Qualification,
		SearchPrograms: append([]string(nil), sess.SearchPrograms...), ProgramQuery: sess.ProgramQuery,
		ProgramOffset: sess.ProgramOffset, SearchRegions: append([]string(nil), sess.SearchRegions...),
		Generation: sess.Generation, AnalysisID: sess.AnalysisID, FeedbackFor: sess.FeedbackFor,
		AnalysisInterrupted:    sess.AnalysisInterrupted,
		AnalysisDeliveryFailed: sess.AnalysisDeliveryFailed,
		LastAnalysisCallbackID: sess.LastAnalysisCallbackID,
		LastDelivery:           cloneSession(sess).LastDelivery,
		LastTransitionEventKey: sess.LastTransitionEventKey,
		TransitionBackup:       append([]byte(nil), sess.TransitionBackup...),
		UpdatedAt:              sess.UpdatedAt,
	}
}

func (p persistedSession) runtime() session {
	return session{
		Step: p.Step, RegionShort: p.RegionShort, RegionCode: p.RegionCode,
		RegionName: p.RegionName, ProgramCode: p.ProgramCode, Qualification: p.Qualification,
		SearchPrograms: append([]string(nil), p.SearchPrograms...), ProgramQuery: p.ProgramQuery,
		ProgramOffset: p.ProgramOffset, SearchRegions: append([]string(nil), p.SearchRegions...),
		Generation: p.Generation, AnalysisID: p.AnalysisID, FeedbackFor: p.FeedbackFor,
		AnalysisInterrupted:    p.AnalysisInterrupted,
		AnalysisDeliveryFailed: p.AnalysisDeliveryFailed,
		LastAnalysisCallbackID: p.LastAnalysisCallbackID,
		LastDelivery:           p.LastDelivery,
		LastTransitionEventKey: p.LastTransitionEventKey,
		TransitionBackup:       append([]byte(nil), p.TransitionBackup...),
		UpdatedAt:              p.UpdatedAt,
	}
}
