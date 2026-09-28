package maxapi

import (
	"testing"
	"time"
)

func TestChatLimiterRegistryEvictsLeastRecentlyUsedInactiveEntry(t *testing.T) {
	now := time.Unix(1_000, 0)
	registry := newChatLimiterRegistry(2, time.Hour)
	registry.now = func() time.Time { return now }

	_, releaseFirst := registry.acquire(1)
	releaseFirst()
	_, releaseSecond := registry.acquire(2)
	releaseSecond()
	now = now.Add(chatLimiterRefillGuard)
	_, releaseFirstAgain := registry.acquire(1)
	releaseFirstAgain()
	now = now.Add(chatLimiterRefillGuard)
	_, releaseThird := registry.acquire(3)
	releaseThird()

	registry.mu.Lock()
	defer registry.mu.Unlock()
	if len(registry.entries) != 2 {
		t.Fatalf("registry size = %d, want 2", len(registry.entries))
	}
	if _, ok := registry.entries[2]; ok {
		t.Fatal("least recently used limiter was not evicted")
	}
	if _, ok := registry.entries[1]; !ok {
		t.Fatal("recent limiter was evicted")
	}
	if _, ok := registry.entries[3]; !ok {
		t.Fatal("new limiter was not stored")
	}
}

func TestChatLimiterRegistryDoesNotEvictActiveWait(t *testing.T) {
	now := time.Unix(2_000, 0)
	registry := newChatLimiterRegistry(1, time.Minute)
	registry.now = func() time.Time { return now }

	first, releaseFirst := registry.acquire(1)
	now = now.Add(time.Hour)
	second, releaseSecond := registry.acquire(2)
	if first == second {
		t.Fatal("active chat unexpectedly received the overflow limiter")
	}
	registry.mu.Lock()
	_, firstRetained := registry.entries[1]
	_, secondStored := registry.entries[2]
	registry.mu.Unlock()
	if !firstRetained || secondStored {
		t.Fatalf("active entry retention = first:%v second:%v, want true/false", firstRetained, secondStored)
	}

	releaseSecond()
	releaseFirst()
	now = now.Add(chatLimiterRefillGuard)
	third, releaseThird := registry.acquire(2)
	defer releaseThird()
	if third == second {
		t.Fatal("inactive slot was not reclaimed from the overflow limiter")
	}
}

func TestChatLimiterRegistryExpiresIdleEntries(t *testing.T) {
	now := time.Unix(3_000, 0)
	registry := newChatLimiterRegistry(3, time.Minute)
	registry.now = func() time.Time { return now }
	_, releaseFirst := registry.acquire(1)
	releaseFirst()
	_, releaseSecond := registry.acquire(2)
	releaseSecond()

	now = now.Add(time.Minute)
	_, releaseThird := registry.acquire(3)
	releaseThird()
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if len(registry.entries) != 1 {
		t.Fatalf("registry size after TTL cleanup = %d, want 1", len(registry.entries))
	}
}
