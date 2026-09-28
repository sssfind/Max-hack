package maxapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// EventKey returns a stable identifier for one MAX update. It is shared by
// the durable webhook queue and the bot's outbound-delivery receipt so a
// retry can replay the exact response without repeating a dialog transition.
func EventKey(update Update) (string, error) {
	if update.Callback != nil {
		if callbackID := strings.TrimSpace(update.Callback.CallbackID); callbackID != "" {
			return "callback:" + callbackID, nil
		}
	}
	messageID := strings.TrimSpace(update.MessageID)
	if messageID == "" && update.Message != nil {
		messageID = strings.TrimSpace(update.Message.Body.Mid)
	}
	if messageID != "" {
		return "message:" + update.UpdateType + ":" + messageID, nil
	}
	payload, err := json.Marshal(update)
	if err != nil {
		return "", fmt.Errorf("marshal update for event key: %w", err)
	}
	sum := sha256.Sum256(payload)
	return "update:" + hex.EncodeToString(sum[:]), nil
}
