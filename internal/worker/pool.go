package worker

import (
	"Max-hack/internal/bot"
	"Max-hack/internal/maxapi"
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Pool struct {
	jobs        chan maxapi.Update
	workerCount int
	db          *pgxpool.Pool
	handler     *bot.Handler
	wg          sync.WaitGroup
}

func NewPool(workerCount, bufferSize int, db *pgxpool.Pool, handler *bot.Handler) *Pool {
	return &Pool{
		jobs:        make(chan maxapi.Update, bufferSize),
		workerCount: workerCount,
		db:          db,
		handler:     handler,
	}
}

func (p *Pool) Start(ctx context.Context) {
	for i := 1; i <= p.workerCount; i++ {
		p.wg.Add(1)
		go p.worker(ctx, i)
	}
	slog.Info("Worker pool started", "workers", p.workerCount)
}

func (p *Pool) Wait() {
	p.wg.Wait()
}

func (p *Pool) worker(ctx context.Context, id int) {
	defer p.wg.Done()
	for {
		select {
		case <-ctx.Done():
			slog.Info("Worker stopped", "worker_id", id)
			return
		case job := <-p.jobs:
			jobCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
			p.process(jobCtx, job)
			cancel()
		}
	}
}

func (p *Pool) process(ctx context.Context, update maxapi.Update) {
	chatID := update.ResolvedChatID()

	query := `
		INSERT INTO processed_events (chat_id, timestamp, update_type)
		VALUES ($1, $2, $3)
		ON CONFLICT (chat_id, timestamp, update_type) DO NOTHING
	`

	commandTag, err := p.db.Exec(ctx, query, chatID, update.Timestamp, update.UpdateType)
	if err != nil {
		slog.Error("Failed to insert processed_event", "error", err, "chat_id", chatID)
		return
	}

	if commandTag.RowsAffected() == 0 {
		slog.Info("Duplicate webhook detected, skipping", "chat_id", chatID, "type", update.UpdateType)
		return
	}

	slog.Info("Processing update", "type", update.UpdateType, "chat_id", chatID)
	p.handler.Handle(ctx, update)
}

// Submit ставит апдейт в очередь. false — очередь полна (webhook должен вернуть 503).
func (p *Pool) Submit(job maxapi.Update) bool {
	select {
	case p.jobs <- job:
		return true
	default:
		slog.Error("CRITICAL: Job queue is full", "chat_id", job.ResolvedChatID())
		return false
	}
}
