package worker

import (
	"Max-hack/internal/maxapi"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type UpdateHandler interface {
	Handle(context.Context, maxapi.Update) error
}

type Config struct {
	HandlerTimeout    time.Duration
	ProcessingTimeout time.Duration
	RecoveryInterval  time.Duration
	CleanupInterval   time.Duration
	CompletedTTL      time.Duration
	FailedTTL         time.Duration
	RecoveryBatchSize int
	MaxAttempts       int
}

func DefaultConfig() Config {
	return Config{
		HandlerTimeout:    30 * time.Second,
		ProcessingTimeout: 2 * time.Minute,
		RecoveryInterval:  2 * time.Second,
		CleanupInterval:   6 * time.Hour,
		CompletedTTL:      30 * 24 * time.Hour,
		FailedTTL:         7 * 24 * time.Hour,
		RecoveryBatchSize: 500,
		MaxAttempts:       8,
	}
}

type Pool struct {
	jobs        chan eventRef
	workerCount int
	store       eventStore
	handler     UpdateHandler
	config      Config
	wg          sync.WaitGroup
	started     atomic.Bool
	chatLocks   [256]sync.Mutex
	pendingMu   sync.Mutex
	pending     map[string]struct{}
}

const eventStateWriteTimeout = 5 * time.Second

func NewPool(workerCount, bufferSize int, db *pgxpool.Pool, handler UpdateHandler) *Pool {
	return NewPoolWithOptions(workerCount, bufferSize, db, handler, DefaultConfig())
}

func NewPoolWithOptions(workerCount, bufferSize int, db *pgxpool.Pool, handler UpdateHandler, config Config) *Pool {
	return newPoolWithStore(workerCount, bufferSize, newPostgresStore(db), handler, config)
}

func newPoolWithStore(workerCount, bufferSize int, store eventStore, handler UpdateHandler, config Config) *Pool {
	if workerCount < 1 {
		workerCount = 1
	}
	if bufferSize < 1 {
		bufferSize = 1
	}
	config = normalizedConfig(config)
	return &Pool{
		jobs:        make(chan eventRef, bufferSize),
		workerCount: workerCount,
		store:       store,
		handler:     handler,
		config:      config,
		pending:     make(map[string]struct{}),
	}
}

func normalizedConfig(config Config) Config {
	defaults := DefaultConfig()
	if config.HandlerTimeout <= 0 {
		config.HandlerTimeout = defaults.HandlerTimeout
	}
	if config.ProcessingTimeout <= config.HandlerTimeout {
		config.ProcessingTimeout = max(defaults.ProcessingTimeout, config.HandlerTimeout*2)
	}
	if config.RecoveryInterval <= 0 {
		config.RecoveryInterval = defaults.RecoveryInterval
	}
	if config.CleanupInterval <= 0 {
		config.CleanupInterval = defaults.CleanupInterval
	}
	if config.CompletedTTL <= 0 {
		config.CompletedTTL = defaults.CompletedTTL
	}
	if config.FailedTTL <= 0 {
		config.FailedTTL = defaults.FailedTTL
	}
	if config.RecoveryBatchSize <= 0 {
		config.RecoveryBatchSize = defaults.RecoveryBatchSize
	}
	if config.MaxAttempts <= 0 {
		config.MaxAttempts = defaults.MaxAttempts
	}
	return config
}

func (p *Pool) Start(ctx context.Context) {
	if !p.started.CompareAndSwap(false, true) {
		return
	}
	for i := 1; i <= p.workerCount; i++ {
		p.wg.Add(1)
		go p.worker(ctx, i)
	}
	p.wg.Add(2)
	go p.recoverer(ctx)
	go p.cleaner(ctx)
	slog.Info("Worker pool started", "workers", p.workerCount)
}

func (p *Pool) Wait() {
	p.wg.Wait()
}

// Ready verifies that workers were started and their durable PostgreSQL store
// (including the processed_events migration) is available.
func (p *Pool) Ready(ctx context.Context) error {
	if !p.started.Load() {
		return errors.New("worker pool is not started")
	}
	return p.store.ready(ctx)
}

func (p *Pool) worker(ctx context.Context, id int) {
	defer p.wg.Done()
	for {
		// A receive from a buffered jobs channel and ctx.Done can both be ready.
		// Check cancellation before entering the select so shutdown never starts
		// another persisted event merely because the select chose the queue case.
		if ctx.Err() != nil {
			slog.Info("Worker stopped", "worker_id", id)
			return
		}
		select {
		case <-ctx.Done():
			slog.Info("Worker stopped", "worker_id", id)
			return
		case job := <-p.jobs:
			// Cancellation may race with the queue receive. Do not claim a new
			// database row once shutdown has begun.
			if ctx.Err() != nil {
				p.forget(job)
				slog.Info("Worker stopped", "worker_id", id)
				return
			}
			jobCtx, cancel := context.WithTimeout(ctx, p.config.ProcessingTimeout)
			processed := p.process(jobCtx, job)
			cancel()
			p.forget(job)
			if processed && ctx.Err() == nil {
				// Wake the next persisted event for this chat without
				// waiting for the periodic recovery tick.
				p.recoverChat(ctx, job.ChatID)
			}
		}
	}
}

func (p *Pool) process(ctx context.Context, ref eventRef) bool {
	lock := &p.chatLocks[uint64(ref.ChatID)%uint64(len(p.chatLocks))]
	if !lock.TryLock() {
		// The row remains in received/failed state and the recovery loop will
		// enqueue it again. Do not let a hot chat occupy every worker goroutine.
		return false
	}
	defer lock.Unlock()

	processed := false
	err := p.store.withChatLock(ctx, ref.ChatID, func(session eventSession) error {
		event, claimed, claimErr := session.claim(ctx, ref, p.config.ProcessingTimeout)
		if !claimed {
			return claimErr
		}
		processed = true
		if claimErr != nil {
			return p.failClaimedEventDurably(ctx, session, ref, event.Attempts, claimErr)
		}
		if event.Attempts > p.config.MaxAttempts {
			return p.failClaimedEventDurably(ctx, session, ref, event.Attempts, errors.New("maximum processing attempts exceeded"))
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return p.failClaimedEventDurably(ctx, session, ref, event.Attempts, fmt.Errorf("processing interrupted: %w", ctxErr))
		}

		slog.Info("Processing update", "type", event.Update.UpdateType, "attempt", event.Attempts)
		handlerCtx, cancel := context.WithTimeout(ctx, p.config.HandlerTimeout)
		handleErr := p.invokeHandler(handlerCtx, event.Update)
		cancel()
		if handleErr != nil {
			return p.failClaimedEventDurably(ctx, session, ref, event.Attempts, handleErr)
		}
		completeCtx, cancelComplete := p.stateWriteContext(ctx)
		defer cancelComplete()
		return session.complete(completeCtx, ref.Key)
	})
	if err != nil && !errors.Is(err, errChatBusy) {
		slog.Error("Failed to process persisted webhook event", "error", err)
	}
	return processed
}

// stateWriteContext gives the durable state transition a short grace period
// even when the worker context was cancelled. Without it, an in-flight event
// can remain in "processing" until the stale-event timeout after shutdown.
func (p *Pool) stateWriteContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), eventStateWriteTimeout)
}

func (p *Pool) failClaimedEventDurably(parent context.Context, session eventSession, ref eventRef, attempt int, cause error) error {
	stateCtx, cancel := p.stateWriteContext(parent)
	defer cancel()
	if parent.Err() != nil {
		// Shutdown/processing cancellation is infrastructure-driven, not a
		// terminal defect in the event. Return the row to retry without spending
		// a business attempt, including on the configured last attempt.
		nextAttempt := time.Now().Add(retryDelay(attempt))
		if failErr := session.interrupt(stateCtx, ref.Key, cause.Error(), nextAttempt); failErr != nil {
			return errors.Join(cause, failErr)
		}
		slog.Info("Webhook update interrupted; scheduled for retry", "error", cause, "attempt", attempt, "next_attempt_at", nextAttempt)
		return nil
	}
	return p.failClaimedEvent(stateCtx, session, ref, attempt, cause)
}

func (p *Pool) failClaimedEvent(ctx context.Context, session eventSession, ref eventRef, attempt int, cause error) error {
	terminal := attempt >= p.config.MaxAttempts || isNonRetryable(cause)
	nextAttempt := time.Now()
	if !terminal {
		nextAttempt = nextAttempt.Add(retryDelay(attempt))
	}
	if failErr := session.fail(ctx, ref.Key, cause.Error(), nextAttempt, terminal); failErr != nil {
		return errors.Join(cause, failErr)
	}
	if terminal {
		slog.Error("Webhook update moved to dead-letter state", "error", cause, "attempt", attempt)
	} else {
		slog.Error("Webhook update failed; scheduled for retry", "error", cause, "attempt", attempt, "next_attempt_at", nextAttempt)
	}
	return nil
}

func isNonRetryable(err error) bool {
	if err == nil {
		return false
	}
	if classified, ok := err.(interface{ NonRetryable() bool }); ok {
		return classified.NonRetryable()
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !isNonRetryable(cause) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return isNonRetryable(wrapped.Unwrap())
	}
	return false
}

func (p *Pool) invokeHandler(ctx context.Context, update maxapi.Update) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("bot handler panic: %v\n%s", recovered, debug.Stack())
		}
	}()
	handleErr := p.handler.Handle(ctx, update)
	if contextErr := ctx.Err(); contextErr != nil {
		return errors.Join(handleErr, fmt.Errorf("bot handler did not complete in time: %w", contextErr))
	}
	return handleErr
}

// Accept persists an update before it can be acknowledged by the webhook. Once
// this method returns nil, a full in-memory queue or a process restart cannot
// lose the update: the recovery loop will enqueue it again from PostgreSQL.
func (p *Pool) Accept(ctx context.Context, update maxapi.Update) error {
	key, err := EventKey(update)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(update)
	if err != nil {
		return fmt.Errorf("marshal webhook event: %w", err)
	}
	ref, shouldQueue, err := p.store.save(ctx, key, update, payload)
	if err != nil {
		return err
	}
	if shouldQueue && !p.enqueue(ref) {
		slog.Warn("Webhook queue is full; persisted event will be recovered")
	}
	return nil
}

func (p *Pool) enqueue(ref eventRef) bool {
	p.pendingMu.Lock()
	if _, exists := p.pending[ref.Key]; exists {
		p.pendingMu.Unlock()
		return true
	}
	p.pending[ref.Key] = struct{}{}
	select {
	case p.jobs <- ref:
		p.pendingMu.Unlock()
		return true
	default:
		delete(p.pending, ref.Key)
		p.pendingMu.Unlock()
		return false
	}
}

func (p *Pool) forget(ref eventRef) {
	p.pendingMu.Lock()
	delete(p.pending, ref.Key)
	p.pendingMu.Unlock()
}

func (p *Pool) recoverer(ctx context.Context) {
	defer p.wg.Done()
	p.recoverDue(ctx)
	ticker := time.NewTicker(p.config.RecoveryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.recoverDue(ctx)
		}
	}
}

func (p *Pool) recoverDue(ctx context.Context) {
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	refs, err := p.store.due(queryCtx, p.config.RecoveryBatchSize, p.config.ProcessingTimeout)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("Failed to recover persisted webhook events", "error", err)
		}
		return
	}
	for _, ref := range refs {
		if !p.enqueue(ref) {
			return
		}
	}
}

func (p *Pool) recoverChat(ctx context.Context, chatID int64) {
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ref, found, err := p.store.nextDue(queryCtx, chatID, p.config.ProcessingTimeout)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("Failed to recover next webhook event for chat", "error", err)
		}
		return
	}
	if found {
		_ = p.enqueue(ref)
	}
}

func (p *Pool) cleaner(ctx context.Context) {
	defer p.wg.Done()
	ticker := time.NewTicker(p.config.CleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			cleanupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			deleted, err := p.store.cleanup(cleanupCtx, now.Add(-p.config.CompletedTTL), now.Add(-p.config.FailedTTL))
			cancel()
			if err != nil {
				if ctx.Err() == nil {
					slog.Error("Failed to cleanup processed webhook events", "error", err)
				}
				continue
			}
			if deleted > 0 {
				slog.Info("Cleaned up processed webhook events", "count", deleted)
			}
		}
	}
}

func retryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	shift := min(attempt-1, 6)
	return time.Second * time.Duration(1<<shift)
}
