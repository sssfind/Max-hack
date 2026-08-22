package worker

import (
	"bot/internal/maxapi"
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Pool struct {
	jobs        chan maxapi.Update
	workerCount int
	db          *pgxpool.Pool
	maxClient   *maxapi.Client
}

func NewPool(workerCount, bufferSize int, db *pgxpool.Pool, maxClient *maxapi.Client) *Pool {
	return &Pool{
		jobs:        make(chan maxapi.Update, bufferSize),
		workerCount: workerCount,
		db:          db,
		maxClient:   maxClient,
	}
}

func (p *Pool) Start(ctx context.Context) {
	for i := 1; i <= p.workerCount; i++ {
		go p.worker(ctx, i)
	}
	slog.Info("Worker pool started", "workers", p.workerCount)
}

func (p *Pool) worker(ctx context.Context, id int) {
	for {
		select {
		case <-ctx.Done():
			slog.Info("Worker stopped", "worker_id", id)
			return
		case job := <-p.jobs:
			p.process(ctx, job)
		}
	}
}

func (p *Pool) process(ctx context.Context, update maxapi.Update) {
	query := `
		INSERT INTO processed_events (chat_id, timestamp, update_type)
		VALUES ($1, $2, $3)
		ON CONFLICT (chat_id, timestamp, update_type) DO NOTHING
	`

	commandTag, err := p.db.Exec(ctx, query, update.ChatID, update.Timestamp, update.UpdateType)
	if err != nil {
		slog.Error("Failed to insert processed_event", "error", err)
		return
	}

	if commandTag.RowsAffected() == 0 {
		slog.Info("Duplicate webhook detected, skipping", "chat_id", update.ChatID)
		return
	}

	slog.Info("Processing NEW update", "chat_id", update.ChatID)

	slog.Info("Processing update", "type", update.UpdateType, "chat_id", update.ChatID)
}

func (p *Pool) Submit(job maxapi.Update) {
	select {
	case p.jobs <- job:
	default:
		slog.Error("CRITICAL: Job queue is full, dropping update!", "chat_id", job.ChatID)
	}
}
