package worker

import (
	"Max-hack/internal/maxapi"
	"crypto/sha256"
	"encoding/binary"
)

// EventKey returns an idempotency key that survives webhook retries and process
// restarts. MAX identifiers take precedence; the hash is only a fallback for
// lifecycle updates that do not carry a message or callback identifier.
func EventKey(update maxapi.Update) (string, error) {
	return maxapi.EventKey(update)
}

// compatibilityTimestamp provides a deterministic fallback for distinct MAX
// events that share the legacy (chat_id, timestamp, update_type) tuple. The
// original tuple remains in use for the first event so the previous binary can
// still deduplicate normal retries during rollback.
func compatibilityTimestamp(eventKey string, probe uint32) int64 {
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(eventKey))
	var encodedProbe [4]byte
	binary.BigEndian.PutUint32(encodedProbe[:], probe)
	_, _ = hasher.Write(encodedProbe[:])
	return int64(binary.BigEndian.Uint64(hasher.Sum(nil)[:8]))
}
