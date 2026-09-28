package maxapi

import (
	"container/list"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	defaultChatLimiterCapacity = 10_000
	defaultChatLimiterIdleTTL  = 15 * time.Minute
	chatLimiterCleanupInterval = time.Minute
	// A burst-one limiter at two requests per second is fully replenished after
	// 500 ms. The wider guard keeps eviction from resetting a recent chat's
	// budget at the capacity boundary.
	chatLimiterRefillGuard = time.Second
)

type chatLimiterEntry struct {
	chatID   int64
	limiter  *rate.Limiter
	lastUsed time.Time
	active   int
}

// chatLimiterRegistry bounds memory used by user-controlled chat IDs. Entries
// are LRU ordered, expire after an idle TTL, and are never evicted while a Wait
// call is using their limiter. If every slot is active or too recent, unknown
// chats share a conservative overflow limiter until a safe slot is available.
type chatLimiterRegistry struct {
	mu          sync.Mutex
	entries     map[int64]*list.Element
	lru         *list.List
	capacity    int
	idleTTL     time.Duration
	nextCleanup time.Time
	overflow    *rate.Limiter
	now         func() time.Time
}

func newChatLimiterRegistry(capacity int, idleTTL time.Duration) *chatLimiterRegistry {
	if capacity < 1 {
		capacity = 1
	}
	if idleTTL <= 0 {
		idleTTL = defaultChatLimiterIdleTTL
	}
	return &chatLimiterRegistry{
		entries:  make(map[int64]*list.Element, capacity),
		lru:      list.New(),
		capacity: capacity,
		idleTTL:  idleTTL,
		overflow: rate.NewLimiter(limitPerSec, burstCapacity),
		now:      time.Now,
	}
}

func (r *chatLimiterRegistry) acquire(chatID int64) (*rate.Limiter, func()) {
	r.mu.Lock()
	now := r.now()
	r.cleanupExpiredLocked(now)
	if element, ok := r.entries[chatID]; ok {
		entry := element.Value.(*chatLimiterEntry)
		entry.active++
		entry.lastUsed = now
		r.lru.MoveToFront(element)
		r.mu.Unlock()
		return entry.limiter, r.releaseFunc(entry)
	}

	if len(r.entries) >= r.capacity {
		r.evictOneLocked(now)
	}
	if len(r.entries) >= r.capacity {
		r.mu.Unlock()
		return r.overflow, func() {}
	}

	entry := &chatLimiterEntry{
		chatID:   chatID,
		limiter:  rate.NewLimiter(limitPerSec, burstCapacity),
		lastUsed: now,
		active:   1,
	}
	r.entries[chatID] = r.lru.PushFront(entry)
	r.mu.Unlock()
	return entry.limiter, r.releaseFunc(entry)
}

func (r *chatLimiterRegistry) releaseFunc(entry *chatLimiterEntry) func() {
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		element, ok := r.entries[entry.chatID]
		if !ok || element.Value.(*chatLimiterEntry) != entry {
			return
		}
		if entry.active > 0 {
			entry.active--
		}
		entry.lastUsed = r.now()
		r.lru.MoveToFront(element)
	}
}

func (r *chatLimiterRegistry) cleanupExpiredLocked(now time.Time) {
	if !r.nextCleanup.IsZero() && now.Before(r.nextCleanup) {
		return
	}
	for element := r.lru.Back(); element != nil; {
		previous := element.Prev()
		entry := element.Value.(*chatLimiterEntry)
		if entry.active == 0 && now.Sub(entry.lastUsed) >= r.idleTTL {
			r.removeLocked(element)
		}
		element = previous
	}
	r.nextCleanup = now.Add(min(r.idleTTL, chatLimiterCleanupInterval))
}

func (r *chatLimiterRegistry) evictOneLocked(now time.Time) {
	for element := r.lru.Back(); element != nil; element = element.Prev() {
		entry := element.Value.(*chatLimiterEntry)
		if entry.active == 0 && now.Sub(entry.lastUsed) >= chatLimiterRefillGuard {
			r.removeLocked(element)
			return
		}
	}
}

func (r *chatLimiterRegistry) removeLocked(element *list.Element) {
	entry := element.Value.(*chatLimiterEntry)
	delete(r.entries, entry.chatID)
	r.lru.Remove(element)
}

// Store is a test seam for replacing a chat limiter with an unbounded one.
func (r *chatLimiterRegistry) Store(chatID int64, limiter *rate.Limiter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if element, ok := r.entries[chatID]; ok {
		entry := element.Value.(*chatLimiterEntry)
		entry.limiter = limiter
		entry.lastUsed = now
		r.lru.MoveToFront(element)
		return
	}
	if len(r.entries) >= r.capacity {
		r.evictOneLocked(now)
	}
	if len(r.entries) >= r.capacity {
		return
	}
	entry := &chatLimiterEntry{chatID: chatID, limiter: limiter, lastUsed: now}
	r.entries[chatID] = r.lru.PushFront(entry)
}
